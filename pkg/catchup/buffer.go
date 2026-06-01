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

package catchup

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	indexSampleInterval = 1 << 20 // sample every 1 MB
	writeChanCap        = 2048    // ~256 MB slack at 128 KB chunks before drops
)

type indexEntry struct {
	wallTime   time.Time
	byteOffset int64
}

// DiskBuffer writes an append-only MPEG-TS file and maintains a time→offset index.
// All disk writes happen in a dedicated goroutine so callers are never blocked.
type DiskBuffer struct {
	streamID  string
	filePath  string
	startTime time.Time
	maxBytes  int64 // 0 = unlimited

	writeCh chan []byte    // pump goroutine sends slices here
	stopCh  chan struct{}  // closed by Stop()
	stopped atomic.Bool

	stoppedMu sync.Mutex
	stoppedAt time.Time

	indexMu    sync.RWMutex
	index      []indexEntry
	total      int64 // bytes successfully written
	baseOffset int64 // earliest readable byte (advances when maxBytes exceeded)
}

// NewDiskBuffer creates a buffer that writes to dir/<streamID>_<unix>.ts.
// maxBytes == 0 means unlimited; otherwise old data is logically trimmed.
func NewDiskBuffer(dir, streamID string, start time.Time, maxBytes int64) (*DiskBuffer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catchup: mkdir %s: %w", dir, err)
	}

	fname := fmt.Sprintf("%s_%d.ts", sanitizeID(streamID), start.Unix())
	fpath := filepath.Join(dir, fname)

	b := &DiskBuffer{
		streamID:  streamID,
		filePath:  fpath,
		startTime: start,
		maxBytes:  maxBytes,
		writeCh:   make(chan []byte, writeChanCap),
		stopCh:    make(chan struct{}),
	}

	go b.drainLoop()
	return b, nil
}

// drainLoop runs in its own goroutine and is the ONLY writer to the file.
func (b *DiskBuffer) drainLoop() {
	f, err := os.OpenFile(b.filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", b.filePath, err)
		// Drain channel so senders never block on a full channel
		for range b.writeCh {
		}
		return
	}
	defer f.Close()

	var written int64
	var sinceLastSample int64

	for chunk := range b.writeCh {
		n, werr := f.Write(chunk)
		if werr != nil {
			utils.WarnLog("Catchup: write error for %s: %v (continuing)", b.filePath, werr)
			continue
		}
		written += int64(n)
		sinceLastSample += int64(n)

		// Record a time→offset index entry every 1 MB
		if sinceLastSample >= indexSampleInterval {
			b.indexMu.Lock()
			b.index = append(b.index, indexEntry{wallTime: time.Now(), byteOffset: written})
			b.total = written
			if b.maxBytes > 0 && written-b.baseOffset > b.maxBytes {
				b.baseOffset = written - b.maxBytes
			}
			b.indexMu.Unlock()
			sinceLastSample = 0
		} else {
			b.indexMu.Lock()
			b.total = written
			b.indexMu.Unlock()
		}
	}

	// Channel was closed; record final total
	b.indexMu.Lock()
	b.total = written
	b.indexMu.Unlock()
}

// Write enqueues a copy of p for async disk write. Never blocks; drops if channel full.
func (b *DiskBuffer) Write(p []byte) {
	if b.stopped.Load() {
		return
	}
	chunk := make([]byte, len(p))
	copy(chunk, p)
	select {
	case b.writeCh <- chunk:
	default:
		utils.WarnLog("Catchup: write channel full for stream %s, dropping %d bytes", b.streamID, len(p))
	}
}

// Stop signals the drain goroutine to finish after processing remaining queued writes.
// Safe to call multiple times.
func (b *DiskBuffer) Stop() {
	if b.stopped.CompareAndSwap(false, true) {
		b.stoppedMu.Lock()
		b.stoppedAt = time.Now()
		b.stoppedMu.Unlock()
		close(b.writeCh)
	}
}

// Delete removes the buffer file from disk. Call only after Stop().
func (b *DiskBuffer) Delete() error {
	b.Stop() // idempotent
	return os.Remove(b.filePath)
}

// IsStopped reports whether Stop has been called.
func (b *DiskBuffer) IsStopped() bool { return b.stopped.Load() }

// StoppedAt returns when Stop was called (zero if not yet stopped).
func (b *DiskBuffer) StoppedAt() time.Time {
	b.stoppedMu.Lock()
	defer b.stoppedMu.Unlock()
	return b.stoppedAt
}

// FilePath returns the path of the buffer file.
func (b *DiskBuffer) FilePath() string { return b.filePath }

// StreamID returns the stream ID this buffer belongs to.
func (b *DiskBuffer) StreamID() string { return b.streamID }

// StartTime returns when the buffer started recording.
func (b *DiskBuffer) StartTime() time.Time { return b.startTime }

// BytesBuffered returns the number of bytes written so far.
func (b *DiskBuffer) BytesBuffered() int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.total
}

// OffsetForTime returns the byte offset in the file that corresponds to wallTime t.
// Returns baseOffset if t is before the oldest buffered data, or total if after.
func (b *DiskBuffer) OffsetForTime(t time.Time) int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()

	if len(b.index) == 0 {
		return b.baseOffset
	}

	// Binary search: find last entry whose wallTime <= t
	pos := sort.Search(len(b.index), func(i int) bool {
		return b.index[i].wallTime.After(t)
	})
	// pos is the first entry AFTER t; pos-1 is what we want
	pos--

	var offset int64
	if pos < 0 {
		// t is before the first index entry — start from the beginning
		offset = 0
	} else {
		offset = b.index[pos].byteOffset
	}

	// Clamp to baseOffset (data before this is no longer on disk)
	if offset < b.baseOffset {
		offset = b.baseOffset
	}
	return offset
}

// sanitizeID strips characters unsafe for use in file names.
func sanitizeID(id string) string {
	safe := make([]byte, 0, len(id))
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c == '/' || c == '\\' || c == ':' || c == '*' || c == '?' || c == '"' || c == '<' || c == '>' || c == '|' || c == '.' {
			safe = append(safe, '_')
		} else {
			safe = append(safe, c)
		}
	}
	return string(safe)
}
