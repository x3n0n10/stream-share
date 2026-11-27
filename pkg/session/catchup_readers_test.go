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
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/provider"
)

// readerManager is a manager with catchup recording, a pause grace and
// capacity provider slots, where alice watched 42.ts and then left it (the
// stream is kept for the pause grace).
func readerManager(t *testing.T, capacity int, grace time.Duration) (*SessionManager, string) {
	t.Helper()
	var opens int64
	srv := countingUpstream(t, &opens)
	t.Cleanup(srv.Close)
	sm := newTestManager()
	t.Cleanup(sm.Stop)
	t.Cleanup(func() { // before srv.Close, which waits for paused upstreams
		sm.streamLock.Lock()
		defer sm.streamLock.Unlock()
		for id := range sm.streamBuffers {
			sm.stopStream(id)
		}
	})
	sm.SetProviderSlots(provider.New(func() int { return capacity }))
	sm.SetCatchupManager(catchup.New(true, t.TempDir(), 1))
	sm.SetPauseGrace(grace)

	_, key, err := sm.RequestStream("alice", "42.ts", "live", "Live 42", mustURL(t, srv.URL+"/live/u/p/42.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("stream should play")
	}
	removeKey(sm, "42.ts", key)
	return sm, srv.URL
}

// A viewer reading the catchup buffer watches the channel: a second watched
// start is turned away instead of taking the slot.
func TestCatchupReaderKeepsSlot(t *testing.T) {
	sm, base := readerManager(t, 1, time.Minute)
	end := sm.BeginCatchupRead("alice", "42")
	defer end()

	buffer, _, err := sm.RequestStream("bob", "43.ts", "live", "Live 43", mustURL(t, base+"/live/u/p/43.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if uerr := waitReady(t, buffer); uerr == nil || uerr.Key != KeyAllInUse {
		t.Fatalf("StartError = %+v, want %s", uerr, KeyAllInUse)
	}
	if !streamActive(sm, "42.ts") {
		t.Fatal("the read stream must keep its slot")
	}
}

// The last reader leaving starts the pause grace.
func TestCatchupReaderEndStartsPauseGrace(t *testing.T) {
	sm, _ := readerManager(t, 1, 200*time.Millisecond)
	end := sm.BeginCatchupRead("alice", "42")
	time.Sleep(400 * time.Millisecond) // past the grace: the reader keeps it
	if !streamActive(sm, "42.ts") {
		t.Fatal("stream stopped under a reader")
	}
	end()
	deadline := time.Now().Add(2 * time.Second)
	for streamActive(sm, "42.ts") {
		if time.Now().After(deadline) {
			t.Fatal("stream still running after the reader left and the grace expired")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Another viewer switching away does not stop a stream someone reads.
func TestOtherViewersSwitchKeepsReadersStream(t *testing.T) {
	sm, base := readerManager(t, 2, time.Minute)
	end := sm.BeginCatchupRead("alice", "42")
	defer end()

	_, key, err := sm.RequestStream("bob", "42.ts", "live", "Live 42", mustURL(t, base+"/live/u/p/42.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("bob should watch 42")
	}
	if _, _, err := sm.RequestStream("bob", "43.ts", "live", "Live 43", mustURL(t, base+"/live/u/p/43.ts")); err != nil {
		t.Fatal(err)
	}
	if !streamActive(sm, "42.ts") {
		t.Fatal("bob's switch stopped the stream alice reads")
	}
}

// A viewer's own reader does not keep their previous channel on a switch.
func TestOwnSwitchIgnoresOwnReader(t *testing.T) {
	sm, base := readerManager(t, 1, time.Minute)
	end := sm.BeginCatchupRead("alice", "42")
	defer end()

	_, key, err := sm.RequestStream("alice", "43.ts", "live", "Live 43", mustURL(t, base+"/live/u/p/43.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if streamActive(sm, "42.ts") {
		t.Fatal("alice's own reader kept her previous channel")
	}
	if !receiving(sm, "43.ts", key, 3*time.Second) {
		t.Fatal("the new channel should play")
	}
}

// Ending a read after its stream stopped leaves no reader behind, and a
// second end changes nothing.
func TestEndAfterStopLeavesNoReader(t *testing.T) {
	sm, _ := readerManager(t, 1, time.Minute)
	end := sm.BeginCatchupRead("alice", "42")
	sm.streamLock.Lock()
	buffer := sm.streamBuffers["42.ts"]
	sm.stopStream("42.ts")
	sm.streamLock.Unlock()
	end()
	end()

	buffer.readersMu.Lock()
	defer buffer.readersMu.Unlock()
	if len(buffer.readers) != 0 {
		t.Fatalf("readers = %v, want empty", buffer.readers)
	}
}

// The last client leaving a stream someone reads neither stops it nor starts
// its pause grace.
func TestClientLeavingKeepsReadersStream(t *testing.T) {
	sm, base := readerManager(t, 2, time.Minute)
	end := sm.BeginCatchupRead("alice", "42")
	defer end()

	_, key, err := sm.RequestStream("bob", "42.ts", "live", "Live 42", mustURL(t, base+"/live/u/p/42.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("bob should watch 42")
	}
	removeKey(sm, "42.ts", key)
	if !streamActive(sm, "42.ts") {
		t.Fatal("bob leaving stopped the stream alice reads")
	}
	sm.streamLock.RLock()
	_, pending := sm.pendingStops["42.ts"]
	sm.streamLock.RUnlock()
	if pending {
		t.Fatal("a pause grace started while alice reads")
	}
}
