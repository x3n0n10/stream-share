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
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
)

// A channel the provider archives itself gets no local buffer: catchup for it
// goes to the provider, so recording it would only cost disk.
func TestLocalBufferOnlyWithoutProviderArchive(t *testing.T) {
	var opens int64
	srv := countingUpstream(t, &opens)
	defer srv.Close()
	cm := catchup.New(true, t.TempDir(), 1)
	cm.SetUpstreamCatchup(map[string]bool{"101": true})
	sm := newTestManager()
	defer sm.Stop()
	sm.SetCatchupManager(cm)

	for id, wantBuffer := range map[string]bool{"101": false, "102": true} {
		_, key, err := sm.RequestStream("viewer", "live-"+id, "live", "Live "+id, mustURL(t, srv.URL+"/live/u/p/"+id+".ts"))
		if err != nil {
			t.Fatalf("RequestStream %s: %v", id, err)
		}
		if !receiving(sm, "live-"+id, key, 3*time.Second) {
			t.Fatalf("stream %s should play", id)
		}
		if got := cm.GetBuffer(id) != nil; got != wantBuffer {
			t.Errorf("channel %s: local buffer = %v, want %v", id, got, wantBuffer)
		}
		removeKey(sm, "live-"+id, key)
	}
}
