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
	"os"
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

// A buffer whose file cannot be opened still signals DrainDone once stopped,
// so a catchup viewer reading it does not wait forever.
func TestDrainDoneAfterFailedOpen(t *testing.T) {
	dir, start := t.TempDir(), time.Now()
	// A directory where the buffer file should go makes opening it fail.
	if err := os.Mkdir(newFilePath(dir, "9", start), 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := NewDiskBuffer(dir, "9", start, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	b.Stop()
	select {
	case <-b.DrainDone():
	case <-time.After(2 * time.Second):
		t.Fatal("DrainDone never closed after a failed open")
	}
}

// When the next buffer file cannot be created, recording carries on in the
// current file instead of switching to a file that does not exist, up to
// twice the size limit.
func TestRotateFailureKeepsRecording(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the read-only folder this test relies on")
	}
	dir := t.TempDir()
	b, err := NewDiskBuffer(dir, "9", time.Now(), indexSampleInterval)
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(b.FilePath()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("first buffer file never appeared")
		}
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o755) }()

	for i := 0; i < 4; i++ {
		b.Write(make([]byte, indexSampleInterval)) // each crosses the size limit
	}
	b.Stop()
	<-b.DrainDone()

	if _, err := os.Stat(b.FilePath()); err != nil {
		t.Fatalf("buffer points at a file that does not exist: %v", err)
	}
	// Recording went on past the failed rotation (more than 1 MB) and stopped
	// at twice the size limit rather than growing without bound (not 4 MB).
	if got := b.BytesBuffered(); got != 2*indexSampleInterval {
		t.Fatalf("buffered %d bytes, want %d", got, 2*indexSampleInterval)
	}
}
