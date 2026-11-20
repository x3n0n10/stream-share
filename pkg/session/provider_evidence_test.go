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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestEvidenceServingOwnStream(t *testing.T) {
	own := newFakeUpstream(t, 'O', 0)
	sm, _ := failoverRig(t, nil)
	aliceWatches(t, sm, own)

	serving, failing := sm.ProviderEvidence()
	if serving != 1 || len(failing) != 0 {
		t.Fatalf("serving=%d failing=%d, want 1 and 0", serving, len(failing))
	}
}

func TestEvidenceSlatingStream(t *testing.T) {
	sm := slateManager(t, newFakeSlate(t, bytes.Repeat([]byte("S"), 2048)))
	sm.SetSlateRetryMax(20 * time.Second)
	defer sm.Stop()
	own := newFakeUpstream(t, 'O', 456)

	buffer, viewer, err := sm.RequestStream("viewer", "ch-ev", "live", "Channel", mustURL(t, own.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "ch-ev", viewer, stop)
	if waitReady(t, buffer) == nil || !sm.Slating("ch-ev") {
		t.Fatal("the stream should be slating")
	}

	serving, failing := sm.ProviderEvidence()
	if serving != 0 || len(failing) != 1 || failing[0].StatusCode != 456 {
		t.Fatalf("serving=%d failing=%+v, want 0 and one 456", serving, failing)
	}
}

func TestEvidenceFallbackAfterOwnError(t *testing.T) {
	own := newFakeUpstream(t, 'O', 456)
	spare := newFakeUpstream(t, 'F', 0)
	sm, _ := failoverRig(t, nil, target(t, "Spare", spare.URL))

	buffer, bob := watch(t, sm, "bob", "ch2", "live", own.URL)
	if uerr := waitReady(t, buffer); uerr != nil {
		t.Fatalf("StartError = %+v, want served via the fallback", uerr)
	}
	if !served(sm, "ch2", bob, 'F', 3*time.Second) {
		t.Fatal("bob should receive the fallback's bytes")
	}

	serving, failing := sm.ProviderEvidence()
	if serving != 0 || len(failing) != 1 || failing[0].StatusCode != 456 {
		t.Fatalf("serving=%d failing=%+v, want 0 and one 456", serving, failing)
	}
}

func TestEvidenceFallbackForConnectionLimit(t *testing.T) {
	own := newFakeUpstream(t, 'O', 0)
	spare := newFakeUpstream(t, 'F', 0)
	sm, _ := failoverRig(t, nil, target(t, "Spare", spare.URL))
	aliceWatches(t, sm, own)

	buffer, bob := watch(t, sm, "bob", "ch2", "live", own.URL)
	if uerr := waitReady(t, buffer); uerr != nil {
		t.Fatalf("StartError = %+v, want served via the fallback", uerr)
	}
	if !served(sm, "ch2", bob, 'F', 3*time.Second) {
		t.Fatal("bob should receive the fallback's bytes")
	}

	// A busy slot is not the provider's fault.
	serving, failing := sm.ProviderEvidence()
	if serving != 1 || len(failing) != 0 {
		t.Fatalf("serving=%d failing=%d, want 1 and 0", serving, len(failing))
	}
}

func TestEvidenceClearedOnRecovery(t *testing.T) {
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

	buffer, viewer, err := sm.RequestStream("viewer", "ch-rec", "live", "Channel", mustURL(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "ch-rec", viewer, stop)
	if waitReady(t, buffer) == nil {
		t.Fatal("the stream should have failed to start")
	}
	if _, failing := sm.ProviderEvidence(); len(failing) != 1 {
		t.Fatalf("failing=%d while slating, want 1", len(failing))
	}

	healthy.Store(true)
	deadline := time.Now().Add(15 * time.Second)
	for sm.Slating("ch-rec") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if sm.Slating("ch-rec") {
		t.Fatal("Slating should be false once the upstream recovered")
	}

	serving, failing := sm.ProviderEvidence()
	if serving != 1 || len(failing) != 0 {
		t.Fatalf("serving=%d failing=%d, want 1 and 0", serving, len(failing))
	}
	removeKey(sm, "ch-rec", viewer) // let the pump end before srv.Close waits on it
}

func TestEvidenceStartingStreamNotCounted(t *testing.T) {
	hang := hangingUpstream(t)
	sm, _ := failoverRig(t, nil)

	buffer, _ := watch(t, sm, "alice", "ch1", "live", hang.URL)
	select {
	case <-buffer.Ready():
		t.Fatal("the stream should still be dialing")
	case <-time.After(100 * time.Millisecond):
	}

	serving, failing := sm.ProviderEvidence()
	if serving != 0 || len(failing) != 0 {
		t.Fatalf("serving=%d failing=%d, want 0 and 0", serving, len(failing))
	}
}
