/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package session

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/provider"
)

const lentViewer = FailoverIdentityPrefix + "Main/1"

// waitReady waits for the stream's start outcome and returns it.
func waitReady(t *testing.T, buffer *StreamBuffer) *UpstreamError {
	t.Helper()
	select {
	case <-buffer.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("buffer never became ready")
	}
	return buffer.StartError()
}

// waitStopped waits until streamID is no longer active.
func waitStopped(sm *SessionManager, streamID string, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for streamActive(sm, streamID) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

// failoverManager is a manager whose provider allows one connection.
func failoverManager() (*SessionManager, *provider.Arbiter) {
	sm := newTestManager()
	slots := provider.New(func() int { return 1 })
	sm.SetProviderSlots(slots)
	sm.slotWait = 200 * time.Millisecond
	return sm, slots
}

func TestFailoverIdentityIsGuestClass(t *testing.T) {
	b := &StreamBuffer{clients: map[string]*streamClient{"a": {user: lentViewer}}}
	if c := b.audienceClass(); c != provider.Share {
		t.Fatalf("lent stream class = %v, want Share", c)
	}
	b.clients["b"] = &streamClient{user: "lucas"}
	if c := b.audienceClass(); c != provider.Foreground {
		t.Fatalf("class with a household viewer = %v, want Foreground", c)
	}
}

func TestDialWithin(t *testing.T) {
	sm := newTestManager()
	defer sm.Stop()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(500 * time.Millisecond):
		case <-r.Context().Done():
		}
	}))
	defer slow.Close()
	start := time.Now()
	resp, uerr := sm.dialWithin(ctx, 50*time.Millisecond, mustURL(t, slow.URL), nil)
	if resp != nil || uerr == nil || uerr.Key != KeyTimeout {
		t.Fatalf("slow dial = %v, %+v; want a %s error", resp, uerr, KeyTimeout)
	}
	if el := time.Since(start); el > 300*time.Millisecond {
		t.Fatalf("slow dial took %s, want under 300ms", el)
	}

	var sawHeader atomic.Bool
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader.Store(r.Header.Get("X-Test") == "1")
		w.WriteHeader(http.StatusOK)
	}))
	defer fast.Close()
	resp, uerr = sm.dialWithin(ctx, 0, mustURL(t, fast.URL), http.Header{"X-Test": {"1"}})
	if uerr != nil {
		t.Fatalf("fast dial: %v", uerr)
	}
	_ = resp.Body.Close()
	if !sawHeader.Load() {
		t.Fatal("the upstream did not receive the extra header")
	}

	late := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		_, _ = w.Write([]byte("late data"))
	}))
	defer late.Close()
	resp, uerr = sm.dialWithin(ctx, 200*time.Millisecond, mustURL(t, late.URL), nil)
	if uerr != nil {
		t.Fatalf("late dial: %v", uerr)
	}
	defer func() { _ = resp.Body.Close() }()
	buf := make([]byte, 64)
	n, err := resp.Body.Read(buf)
	if n == 0 || !bytes.Contains(buf[:n], []byte("late data")) {
		t.Fatalf("read after the dial timeout = %q, %v; want the late data", buf[:n], err)
	}
}

// Review Focus 1: a lent stream must report the provider's failure, never a slate.
func TestForwardedStreamNeverSlates(t *testing.T) {
	sm := slateManager(t, newFakeSlate(t, []byte("SLATE")))
	defer sm.Stop()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(456)
	}))
	defer srv.Close()

	buffer, _, err := sm.RequestStream(lentViewer, "fw-456", "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	if uerr := waitReady(t, buffer); uerr == nil || uerr.StatusCode != 456 {
		t.Fatalf("StartError = %+v, want status 456", uerr)
	}
	if sm.ServesSlateFor("fw-456") {
		t.Fatal("a lent stream must not be served a slate")
	}
	if sm.Slating("fw-456") {
		t.Fatal("a lent stream must not be slating")
	}
	if !waitStopped(sm, "fw-456", 2*time.Second) {
		t.Fatal("the failed lent stream should end")
	}
}

func TestSlatingFlag(t *testing.T) {
	sm := slateManager(t, newFakeSlate(t, bytes.Repeat([]byte("S"), 2048)))
	sm.SetSlateRetryMax(20 * time.Second)
	defer sm.Stop()

	var healthy atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for {
			if _, err := w.Write(make([]byte, 4096)); err != nil {
				return
			}
			fl.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
			}
		}
	}))
	defer srv.Close()

	buffer, viewer, err := sm.RequestStream("viewer", "chan-slating", "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "chan-slating", viewer, stop)
	if waitReady(t, buffer) == nil {
		t.Fatal("the stream should have failed to start")
	}
	if !sm.Slating("chan-slating") {
		t.Fatal("Slating should be true while the slate is served")
	}

	healthy.Store(true)
	deadline := time.Now().Add(15 * time.Second)
	for sm.Slating("chan-slating") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sm.Slating("chan-slating") {
		t.Fatal("Slating should be false once the upstream recovered")
	}
	if !streamActive(sm, "chan-slating") {
		t.Fatal("the stream should play on after recovering")
	}
	removeKey(sm, "chan-slating", viewer)
}

func TestForwardedStreamYieldsToHousehold(t *testing.T) {
	var opens1, opens2 int64
	srv1 := countingUpstream(t, &opens1)
	defer srv1.Close()
	srv2 := countingUpstream(t, &opens2)
	defer srv2.Close()
	sm, _ := failoverManager()
	defer sm.Stop()

	_, mine, err := sm.RequestStream("lucas", "ch-x", "live", "X", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "ch-x", mine, stop)
	if !receiving(sm, "ch-x", mine, 3*time.Second) {
		t.Fatal("the household stream should play")
	}

	start := time.Now()
	buffer, _, err := sm.RequestStream(lentViewer, "ch-y", "live", "Y", mustURL(t, srv2.URL))
	if err != nil {
		t.Fatal(err)
	}
	uerr := waitReady(t, buffer)
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("the lent request took %s to be turned away, want under 100ms", el)
	}
	if uerr == nil || uerr.Key != KeyAllInUse {
		t.Fatalf("StartError = %+v, want %s", uerr, KeyAllInUse)
	}
	if n := atomic.LoadInt64(&opens2); n != 0 {
		t.Fatalf("the lent stream opened the upstream %d times, want 0", n)
	}
	if !streamActive(sm, "ch-x") {
		t.Fatal("the household stream must keep playing")
	}
	removeKey(sm, "ch-x", mine)
}

// Review Focus 2: the lent viewer zaps; the previous lent stream is still being
// torn down when the next request arrives.
func TestForwardedStreamWaitsForDepartingGuest(t *testing.T) {
	t.Run("departs", func(t *testing.T) {
		var opens1, opens2 int64
		srv1 := countingUpstream(t, &opens1)
		defer srv1.Close()
		srv2 := countingUpstream(t, &opens2)
		defer srv2.Close()
		sm, _ := failoverManager()
		defer sm.Stop()

		_, x, err := sm.RequestStream(lentViewer, "ch-x", "live", "X", mustURL(t, srv1.URL))
		if err != nil {
			t.Fatal(err)
		}
		if !receiving(sm, "ch-x", x, 3*time.Second) {
			t.Fatal("the first lent stream should play")
		}
		buffer, y, err := sm.RequestStream(FailoverIdentityPrefix+"Main/2", "ch-y", "live", "Y", mustURL(t, srv2.URL))
		if err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond)
		removeKey(sm, "ch-x", x)
		if uerr := waitReady(t, buffer); uerr != nil {
			t.Fatalf("StartError = %+v, want the next lent stream to start", uerr)
		}
		if !receiving(sm, "ch-y", y, 3*time.Second) {
			t.Fatal("the next lent stream should play")
		}
		removeKey(sm, "ch-y", y)
	})

	t.Run("stays", func(t *testing.T) {
		var opens1, opens2 int64
		srv1 := countingUpstream(t, &opens1)
		defer srv1.Close()
		srv2 := countingUpstream(t, &opens2)
		defer srv2.Close()
		sm, _ := failoverManager()
		defer sm.Stop()

		_, x, err := sm.RequestStream(lentViewer, "ch-x", "live", "X", mustURL(t, srv1.URL))
		if err != nil {
			t.Fatal(err)
		}
		stop := make(chan struct{})
		defer close(stop)
		go keepDraining(sm, "ch-x", x, stop)
		if !receiving(sm, "ch-x", x, 3*time.Second) {
			t.Fatal("the first lent stream should play")
		}
		start := time.Now()
		buffer, _, err := sm.RequestStream(FailoverIdentityPrefix+"Main/2", "ch-y", "live", "Y", mustURL(t, srv2.URL))
		if err != nil {
			t.Fatal(err)
		}
		uerr := waitReady(t, buffer)
		if uerr == nil || uerr.Key != KeyAllInUse {
			t.Fatalf("StartError = %+v, want %s", uerr, KeyAllInUse)
		}
		if el := time.Since(start); el < 150*time.Millisecond || el > time.Second {
			t.Fatalf("turned away after %s, want about the 200ms slot wait", el)
		}
		if n := atomic.LoadInt64(&opens2); n != 0 {
			t.Fatalf("the refused stream opened the upstream %d times, want 0", n)
		}
		removeKey(sm, "ch-x", x)
	})
}

func TestHouseholdPreemptsForwardedStream(t *testing.T) {
	var opens1, opens2 int64
	srv1 := countingUpstream(t, &opens1)
	defer srv1.Close()
	srv2 := countingUpstream(t, &opens2)
	defer srv2.Close()
	sm, slots := failoverManager()
	defer sm.Stop()

	_, guest, err := sm.RequestStream(lentViewer, "ch-y", "live", "Y", mustURL(t, srv2.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "ch-y", guest, stop)
	if !receiving(sm, "ch-y", guest, 3*time.Second) {
		t.Fatal("the lent stream should play while the slot is free")
	}
	if !slots.WouldGrant(provider.Foreground) {
		t.Fatal("the household must be able to take the slot")
	}

	_, mine, err := sm.RequestStream("lucas", "ch-x", "live", "X", mustURL(t, srv1.URL))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "ch-x", mine, 5*time.Second) {
		t.Fatal("the household stream should play")
	}
	if !waitStopped(sm, "ch-y", 3*time.Second) {
		t.Fatal("the lent stream should stop when the household takes the slot")
	}
	removeKey(sm, "ch-x", mine)
}

func TestForwardedLastClientStopsAtOnce(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()
	sm := newTestManager()
	defer sm.Stop()
	sm.SetCatchupManager(catchup.New(true, t.TempDir(), 1))
	sm.SetPauseGrace(time.Minute)

	_, key, err := sm.RequestStream(lentViewer, "42.ts", "live", "Live 42", mustURL(t, srv.URL+"/live/u/p/42.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("stream should play")
	}
	removeKey(sm, "42.ts", key)
	if streamActive(sm, "42.ts") {
		t.Fatal("a lent stream must stop as soon as its last viewer leaves")
	}
}

func TestCatalogHasInUse(t *testing.T) {
	got, ok := LoadCatalog("").entries[KeyAllInUse]
	want := ErrorInfo{Meaning: "In Use", Message: "All provider connections are in use by other viewers."}
	if !ok || got != want {
		t.Fatalf("catalog %s = %+v, want %+v", KeyAllInUse, got, want)
	}
}
