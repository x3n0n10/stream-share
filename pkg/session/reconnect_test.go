/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
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
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// droppingUpstream streams small chunks; its first connection ends cleanly
// when drop closes. Later connections stream on, or answer 503 when fail.
func droppingUpstream(t *testing.T, opens *int64, drop <-chan struct{}, fail bool) *httptest.Server {
	t.Helper()
	payload := make([]byte, 4096)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := atomic.AddInt64(opens, 1) == 1
		if !first && fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		for {
			if _, err := w.Write(payload); err != nil {
				return
			}
			if fl != nil {
				fl.Flush()
			}
			var dropped <-chan struct{}
			if first {
				dropped = drop
			}
			select {
			case <-r.Context().Done():
				return
			case <-dropped:
				return // a clean end of the body: EOF for the pump
			case <-time.After(2 * time.Millisecond):
			}
		}
	}))
}

// reconnectManager is a catchupTestManager over a droppingUpstream with a
// short reconnect backoff.
func reconnectManager(t *testing.T, opens *int64, drop <-chan struct{}, fail bool) (*SessionManager, string) {
	t.Helper()
	srv := droppingUpstream(t, opens, drop, fail)
	sm := catchupTestManager(t, srv, 1, time.Minute)
	sm.SetPauseStallTimeout(200 * time.Millisecond)
	sm.reconnectBackoff = 10 * time.Millisecond
	return sm, srv.URL
}

func waitUntil(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

// A paused stream whose upstream ends cleanly reconnects and keeps recording
// into the same disk buffer, instead of stopping and losing the catchup.
func TestPausedStreamReconnectsOnUpstreamEnd(t *testing.T) {
	var opens int64
	drop := make(chan struct{})
	sm, base := reconnectManager(t, &opens, drop, false)
	buffer, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	if !buffer.paused.Load() {
		t.Fatal("the stalled viewer should pause the stream")
	}
	disk := buffer.diskBuffer

	close(drop)
	if !waitUntil(3*time.Second, func() bool { return atomic.LoadInt64(&opens) >= 2 }) {
		t.Fatalf("upstream opens = %d, want a reconnect", atomic.LoadInt64(&opens))
	}
	if !streamActive(sm, "42.ts") {
		t.Fatal("the paused stream must stay active")
	}
	if buffer.diskBuffer != disk {
		t.Fatal("the reconnect must keep the same disk buffer")
	}
	before := disk.BytesBuffered()
	if !waitUntil(3*time.Second, func() bool { return disk.BytesBuffered() > before }) {
		t.Fatal("the disk buffer should keep growing after the reconnect")
	}
}

// A stream nobody protects stops when its upstream ends, as before: its
// players reconnect themselves.
func TestUnprotectedStreamStopsOnUpstreamEnd(t *testing.T) {
	var opens int64
	drop := make(chan struct{})
	sm, base := reconnectManager(t, &opens, drop, false)
	_, key := startLive(t, sm, base, "alice", "42")
	stop := make(chan struct{})
	defer close(stop)
	go keepDraining(sm, "42.ts", key, stop)
	if !waitUntil(3*time.Second, func() bool { return atomic.LoadInt64(&opens) == 1 }) {
		t.Fatal("stream should start")
	}

	close(drop)
	if !waitUntil(3*time.Second, func() bool { return !streamActive(sm, "42.ts") }) {
		t.Fatal("an unprotected stream should stop when its upstream ends")
	}
	// A reconnect would come within the 10 ms backoff: give it ample time.
	if waitUntil(200*time.Millisecond, func() bool { return atomic.LoadInt64(&opens) > 1 }) {
		t.Fatalf("upstream opens = %d, want no reconnect", atomic.LoadInt64(&opens))
	}
}

// A protected stream whose upstream keeps failing stops after the retries.
func TestReconnectGivesUpAfterRetries(t *testing.T) {
	var opens int64
	drop := make(chan struct{})
	sm, base := reconnectManager(t, &opens, drop, true)
	_, key := startLive(t, sm, base, "alice", "42")
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("stream should play")
	}
	removeKey(sm, "42.ts", key)
	end := sm.BeginCatchupRead("alice", "42")
	defer end(false)

	close(drop)
	if !waitUntil(3*time.Second, func() bool { return !streamActive(sm, "42.ts") }) {
		t.Fatal("the stream should stop once the reconnects fail")
	}
	if n := atomic.LoadInt64(&opens); n != 1+reconnectAttempts {
		t.Fatalf("upstream opens = %d, want %d", n, 1+reconnectAttempts)
	}
}

// A stopped stream's catchup files outlive the grace while someone reads
// them, and go a grace after the last reader leaves.
func TestStoppedBufferKeptWhileRead(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	sm := catchupTestManager(t, srv, 1, time.Minute)
	sm.catchupManager.SetGracePeriod(50 * time.Millisecond)
	buffer, key := startLive(t, sm, srv.URL, "alice", "42")
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("stream should play")
	}
	file := buffer.diskBuffer.FilePath()
	end := sm.BeginCatchupRead("alice", "42")

	sm.stopStreamLocking("42.ts")
	time.Sleep(150 * time.Millisecond) // 3x the grace
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("catchup file gone while read: %v", err)
	}

	end(false)
	if !waitUntil(2*time.Second, func() bool { _, err := os.Stat(file); return os.IsNotExist(err) }) {
		t.Fatal("catchup file should be deleted a grace after the last reader left")
	}
}
