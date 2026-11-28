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

	"github.com/lucasduport/stream-share/pkg/provider"
)

// pauseManager is a catchupTestManager with a one-minute pause grace and a
// short pause stall timeout, over an upstream that streams for longer than
// any test; it returns the upstream's base URL too.
func pauseManager(t *testing.T, capacity int) (*SessionManager, string) {
	t.Helper()
	var written int64
	srv := streamingUpstream(t, 1<<20, 16*1024, 2*time.Millisecond, &written)
	sm := catchupTestManager(t, srv, capacity, time.Minute)
	sm.SetPauseStallTimeout(200 * time.Millisecond)
	return sm, srv.URL
}

// startLive starts user on channel id (e.g. "42") and returns the client key.
func startLive(t *testing.T, sm *SessionManager, base, user, id string) (*StreamBuffer, string) {
	t.Helper()
	buffer, key, err := sm.RequestStream(user, id+".ts", "live", "Live "+id, mustURL(t, base+"/live/u/p/"+id+".ts"))
	if err != nil {
		t.Fatal(err)
	}
	return buffer, key
}

// stallOut waits until the pump closes the never-reading client key, then
// releases it the way the HTTP handler does once its done channel closes.
func stallOut(t *testing.T, sm *SessionManager, streamID, key string) {
	t.Helper()
	done, ok := sm.GetClientDone(streamID, key)
	if !ok {
		t.Fatalf("client %s not attached", key)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stalled client %s was not closed", key)
	}
	removeKey(sm, streamID, key)
}

// drainThenLeave has client key of streamID read for d, then leave while
// reading (a stop, not a pause).
func drainThenLeave(sm *SessionManager, streamID, key string, d time.Duration) {
	stop := make(chan struct{})
	go keepDraining(sm, streamID, key, stop)
	time.Sleep(d)
	removeKey(sm, streamID, key)
	close(stop)
}

func bufferOf(sm *SessionManager, streamID string) *StreamBuffer {
	sm.streamLock.RLock()
	defer sm.streamLock.RUnlock()
	return sm.streamBuffers[streamID]
}

func TestStalledViewerOnCachedChannelIsClosedAndRecordingContinues(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)

	disk := sm.catchupManager.GetBuffer("42")
	if disk == nil {
		t.Fatal("no disk buffer")
	}
	before := disk.BytesBuffered()
	time.Sleep(500 * time.Millisecond)
	if after := disk.BytesBuffered(); after <= before {
		t.Fatalf("recording stalled: %d -> %d bytes", before, after)
	}
}

func TestPausedViewerKeepsSlot(t *testing.T) {
	sm, base := pauseManager(t, 1)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	if !bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("stalled viewer leaving should pause the stream")
	}

	buffer, _ := startLive(t, sm, base, "bob", "43")
	if uerr := waitReady(t, buffer); uerr == nil || uerr.Key != KeyAllInUse {
		t.Fatalf("StartError = %+v, want %s", uerr, KeyAllInUse)
	}
	if !streamActive(sm, "42.ts") {
		t.Fatal("the paused stream must keep its slot")
	}
}

func TestStoppedViewerFreesSlot(t *testing.T) {
	sm, base := pauseManager(t, 1)
	_, key := startLive(t, sm, base, "alice", "42")
	drainThenLeave(sm, "42.ts", key, 300*time.Millisecond)

	b := bufferOf(sm, "42.ts")
	if b.paused.Load() {
		t.Fatal("a viewer who kept reading stopped, not paused")
	}
	if c := b.audienceClass(); c != provider.Background {
		t.Fatalf("audienceClass = %v, want Background", c)
	}
	_, bobKey := startLive(t, sm, base, "bob", "43")
	if !receiving(sm, "43.ts", bobKey, 3*time.Second) {
		t.Fatal("bob should take the freed slot")
	}
}

func TestPauseSurvivesOtherViewerLeavingLast(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, carol := startLive(t, sm, base, "carol", "42")
	stop := make(chan struct{})
	go keepDraining(sm, "42.ts", carol, stop)
	_, alice := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", alice)
	removeKey(sm, "42.ts", carol)
	close(stop)

	if !bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("alice's pause should survive carol leaving last")
	}
}

func TestPausedGuestNotProtected(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, key := startLive(t, sm, base, ShareIdentityPrefix+"x", "42")
	stallOut(t, sm, "42.ts", key)
	if bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("a guest's stall must not pause the stream")
	}
}

func TestResumeClearsPause(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	b := bufferOf(sm, "42.ts")
	if !b.paused.Load() {
		t.Fatal("precondition: stream paused")
	}

	end := sm.BeginCatchupRead("alice", "42")
	defer end(false)
	if b.paused.Load() {
		t.Fatal("resume should clear the pause")
	}
	sm.streamLock.RLock()
	_, pending := sm.pendingStops["42.ts"]
	sm.streamLock.RUnlock()
	if pending {
		t.Fatal("resume should cancel the pending stop")
	}
}

func TestPauseGraceExpiryClearsPause(t *testing.T) {
	sm, base := pauseManager(t, 2)
	sm.SetPauseGrace(300 * time.Millisecond)
	_, key := startLive(t, sm, base, "alice", "42")
	b := bufferOf(sm, "42.ts")
	stallOut(t, sm, "42.ts", key)
	if !b.paused.Load() {
		t.Fatal("precondition: stream paused")
	}
	if !waitStopped(sm, "42.ts", 2*time.Second) {
		t.Fatal("stream still running after the pause grace")
	}
	if b.paused.Load() {
		t.Fatal("stopping should clear the pause")
	}
}

func TestStopAfterJitterIsNotAPause(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, key := startLive(t, sm, base, "alice", "42")
	drainThenLeave(sm, "42.ts", key, 400*time.Millisecond)
	if bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("leaving after reading normally is a stop, not a pause")
	}
}

// A viewer rejoining a paused stream ends the pause and forgets the stall: when
// that viewer then leaves while reading, it is a stop.
func TestRejoinForgetsPause(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	b := bufferOf(sm, "42.ts")
	if !b.paused.Load() {
		t.Fatal("precondition: stream paused")
	}

	_, key = startLive(t, sm, base, "alice", "42")
	if b.paused.Load() {
		t.Fatal("rejoining should clear the pause")
	}
	drainThenLeave(sm, "42.ts", key, 300*time.Millisecond)
	if b.paused.Load() {
		t.Fatal("an old stall turned a real stop into a pause")
	}
}

// leftReading starts alice on 42.ts in a one-slot pauseManager and has her
// leave while reading: the stream is kept for the pause grace, not paused.
func leftReading(t *testing.T) (*SessionManager, string, *StreamBuffer) {
	t.Helper()
	sm, base := pauseManager(t, 1)
	_, key := startLive(t, sm, base, "alice", "42")
	drainThenLeave(sm, "42.ts", key, 300*time.Millisecond)
	b := bufferOf(sm, "42.ts")
	if b.paused.Load() {
		t.Fatal("precondition: not paused")
	}
	return sm, base, b
}

// A catchup read that ended because the client stopped taking data (a pause
// while timeshifting) pauses the stream: it keeps its slot.
func TestStalledCatchupReadPauses(t *testing.T) {
	sm, base, b := leftReading(t)
	sm.BeginCatchupRead("alice", "42")(true)
	if !b.paused.Load() {
		t.Fatal("a stalled catchup read should pause the stream")
	}
	buffer, _ := startLive(t, sm, base, "bob", "43")
	if uerr := waitReady(t, buffer); uerr == nil || uerr.Key != KeyAllInUse {
		t.Fatalf("StartError = %+v, want %s", uerr, KeyAllInUse)
	}
	if !streamActive(sm, "42.ts") {
		t.Fatal("the paused stream must keep its slot")
	}
}

func TestFinishedCatchupReadIsNotAPause(t *testing.T) {
	sm, _, b := leftReading(t)
	sm.BeginCatchupRead("alice", "42")(false)
	if b.paused.Load() {
		t.Fatal("a catchup read that ended normally is not a pause")
	}
	if c := b.audienceClass(); c != provider.Background {
		t.Fatalf("audienceClass = %v, want Background", c)
	}
}

// A guest's stalled catchup read never protects the stream.
func TestStalledGuestCatchupReadIsNotAPause(t *testing.T) {
	sm, _, b := leftReading(t)
	sm.BeginCatchupRead(ShareIdentityPrefix+"x", "42")(true)
	if b.paused.Load() {
		t.Fatal("a guest's stalled read must not pause the stream")
	}
}

// A viewer back on a stream that still has clients (no pending stop to
// cancel) forgets their old stall: a later normal stop is a stop.
func TestRejoinWithOtherViewerForgetsStall(t *testing.T) {
	sm, base := pauseManager(t, 2)
	_, carol := startLive(t, sm, base, "carol", "42")
	stopCarol := make(chan struct{})
	go keepDraining(sm, "42.ts", carol, stopCarol)
	_, alice := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", alice)

	_, alice = startLive(t, sm, base, "alice", "42")
	drainThenLeave(sm, "42.ts", alice, 300*time.Millisecond)
	removeKey(sm, "42.ts", carol)
	close(stopCarol)
	if bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("an old stall turned a real stop into a pause")
	}
}

// A viewer who paused live and then starts a movie gets the paused stream's
// slot: LeavePreviousStream stops it and reports a takeover.
func TestLeavePausedLiveForMovieFreesSlot(t *testing.T) {
	sm, base := pauseManager(t, 1)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	if !bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("precondition: stream paused")
	}
	if !sm.LeavePreviousStream("alice", "601") {
		t.Fatal("leaving a stream that held a slot is a takeover")
	}
	if streamActive(sm, "42.ts") {
		t.Fatal("the viewer's paused stream should stop")
	}
	deadline := time.Now().Add(2 * time.Second)
	for !sm.providerSlots.WouldGrant(provider.Foreground) {
		if time.Now().After(deadline) {
			t.Fatal("the paused stream's slot was not freed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Another user's paused stream is not theirs to stop.
func TestLeavePreviousStreamLeavesOtherUsersStream(t *testing.T) {
	sm, base := pauseManager(t, 1)
	_, key := startLive(t, sm, base, "alice", "42")
	stallOut(t, sm, "42.ts", key)
	if sm.LeavePreviousStream("bob", "601") {
		t.Fatal("bob left nothing: not a takeover")
	}
	if !streamActive(sm, "42.ts") || !bufferOf(sm, "42.ts").paused.Load() {
		t.Fatal("alice's paused stream must stay")
	}
}
