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

package catchup

import (
	"testing"
	"time"
)

// A channel the provider flags as archived but has no recording for is treated
// as unarchived for a day, even when the channel list is refreshed meanwhile.
func TestNoUpstreamArchiveMark(t *testing.T) {
	m := New(true, t.TempDir(), 1)
	m.SetUpstreamCatchup(map[string]bool{"7": true})
	m.MarkNoUpstreamArchive("7.ts")
	if m.HasUpstreamCatchup("7") {
		t.Fatal("marked channel still counts as provider-archived")
	}
	m.SetUpstreamCatchup(map[string]bool{"7": true})
	if m.HasUpstreamCatchup("7") {
		t.Fatal("a refreshed channel list dropped the mark")
	}
	m.mu.Lock()
	m.noArchive["7"] = time.Now().Add(-25 * time.Hour)
	m.mu.Unlock()
	if !m.HasUpstreamCatchup("7") {
		t.Fatal("the mark should expire after a day")
	}
	m.MarkNoUpstreamArchive("8") // not flagged: nothing to unlearn
	if len(m.noArchive) != 0 {
		t.Fatalf("unflagged channel was marked: %v", m.noArchive)
	}
}

// The stream pump writes while a viewer leaving stops the buffer: a write
// racing the stop must be dropped, never sent on the closed channel.
func TestWriteRacingStop(t *testing.T) {
	b, err := NewDiskBuffer(t.TempDir(), "9", time.Now(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100000; i++ {
			if i == 100 {
				close(started)
			}
			b.Write([]byte("x"))
		}
	}()
	<-started
	b.Stop()
	<-done
}
