/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"strings"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/slate"
)

// A catchup request the provider answers 404 for has no recording at that
// time; any other failure keeps its own wording.
func TestCatchupSlateClipWording(t *testing.T) {
	fake := newFakeSlate(t, []byte("SLATE"))
	sm := slateManager(t, fake)
	defer sm.Stop()

	path, ok := sm.CatchupSlateClip("42.ts", http.StatusNotFound, nil)
	if !ok || path != fake.path {
		t.Fatalf("clip = %q, %v", path, ok)
	}
	if v := fake.lastView.Load().(slate.View); v.Meaning != "No Recording" || !strings.Contains(v.Message, "No recording") {
		t.Fatalf("404 slate says %+v", v)
	}
	if _, ok := sm.CatchupSlateClip("42.ts", 456, nil); !ok {
		t.Fatal("no clip for a connection-limit answer")
	}
	if v := fake.lastView.Load().(slate.View); v.Meaning != "Connection Limit" {
		t.Fatalf("456 slate says %+v", v)
	}
}

// Without slates (disabled, or no ffmpeg) the caller answers as before.
func TestCatchupSlateClipUnavailable(t *testing.T) {
	sm := slateManager(t, nil)
	defer sm.Stop()
	if _, ok := sm.CatchupSlateClip("42.ts", http.StatusNotFound, nil); ok {
		t.Fatal("clip offered without a slate provider")
	}
}

// The slate repeats for the one catchup viewer until they leave.
func TestPlayCatchupSlateRepeatsUntilViewerLeaves(t *testing.T) {
	fake := newFakeSlate(t, []byte("AB"))
	sm := slateManager(t, fake)
	defer sm.Stop()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	sm.PlayCatchupSlate(ctx, &out, fake.path)
	if !strings.HasPrefix(out.String(), "ABAB") {
		t.Fatalf("viewer got %q, want the clip repeated", out.String())
	}
}

// Leaving the live picture for catchup frees the provider connection the
// paused live stream holds; a stream someone still watches is left alone.
func TestReleasePausedStream(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()
	sm := newTestManager()
	defer sm.Stop()
	sm.SetCatchupManager(catchup.New(true, t.TempDir(), 1))
	sm.SetPauseGrace(time.Minute)

	_, key, err := sm.RequestStream("viewer", "42.ts", "live", "Live 42", mustURL(t, srv.URL+"/live/u/p/42.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !receiving(sm, "42.ts", key, 3*time.Second) {
		t.Fatal("stream should play")
	}
	if sm.ReleasePausedStream("42") {
		t.Fatal("released a stream someone is watching")
	}
	removeKey(sm, "42.ts", key) // the viewer leaves: paused, kept for the grace
	if !sm.ReleasePausedStream("42") {
		t.Fatal("paused stream not released")
	}
	sm.streamLock.RLock()
	b, exists := sm.streamBuffers["42.ts"]
	still := exists && b.active
	sm.streamLock.RUnlock()
	if still {
		t.Fatal("released stream still running")
	}
}
