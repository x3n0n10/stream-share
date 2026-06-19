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
	indexSampleInterval = 1 << 20       // sample every 1 MB
	writeChanCap        = 2048          // ~256 MB slack at 128 KB chunks before drops
	rotationGracePeriod = 60 * time.Second // keep old file alive for in-flight readers
)

type indexEntry struct {
	wallTime   time.Time
	byteOffset int64
}

// DiskBuffer writes an append-only MPEG-TS file and maintains a time→offset index.
// All disk writes happen in a dedicated goroutine so callers are never blocked.
// When maxBytes > 0 and the file reaches that size, it is rotated: the old file
// is deleted after a grace period (so in-flight readers can finish) and a new
// file is opened. This bounds disk usage to approximately maxBytes per channel.
type DiskBuffer struct {
	streamID string
	maxBytes int64 // 0 = unlimited

	writeCh   chan []byte  // pump goroutine sends slices here
	drainDone chan struct{} // closed by drainLoop when all writes are flushed to disk
	stopped   int32        // 0 = running, 1 = stopped; accessed via atomic

	stoppedMu sync.Mutex
	stoppedAt time.Time

	// indexMu guards filePath, startTime, index, total, and baseOffset.
	indexMu    sync.RWMutex
	filePath   string
	startTime  time.Time
	index      []indexEntry
	total      int64 // bytes written to the current file
	baseOffset int64 // always 0; kept for OffsetForTime compatibility
}

// NewDiskBuffer creates a buffer that writes to dir/<streamID>_<unix>.ts.
// maxBytes == 0 means unlimited; otherwise the file is rotated when it reaches maxBytes.
func NewDiskBuffer(dir, streamID string, start time.Time, maxBytes int64) (*DiskBuffer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catchup: mkdir %s: %w", dir, err)
	}

	fpath := newFilePath(dir, streamID, start)

	b := &DiskBuffer{
		streamID:  streamID,
		filePath:  fpath,
		startTime: start,
		maxBytes:  maxBytes,
		writeCh:   make(chan []byte, writeChanCap),
		drainDone: make(chan struct{}),
	}

	go b.drainLoop()
	return b, nil
}

// drainLoop runs in its own goroutine and is the ONLY writer to the file.
func (b *DiskBuffer) drainLoop() {
	b.indexMu.RLock()
	initialPath := b.filePath
	b.indexMu.RUnlock()

	f, err := os.OpenFile(initialPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", initialPath, err)
		for range b.writeCh {
		}
		return
	}

	var written int64
	var sinceLastSample int64

	for chunk := range b.writeCh {
		n, werr := f.Write(chunk)
		if werr != nil {
			utils.WarnLog("Catchup: write error for stream %s: %v (continuing)", b.streamID, werr)
			continue
		}
		written += int64(n)
		sinceLastSample += int64(n)

		if sinceLastSample >= indexSampleInterval {
			b.indexMu.Lock()
			b.index = append(b.index, indexEntry{wallTime: time.Now(), byteOffset: written})
			b.total = written
			b.indexMu.Unlock()
			sinceLastSample = 0

			// Rotate file when it reaches maxBytes so disk usage stays bounded.
			if b.maxBytes > 0 && written >= b.maxBytes {
				newF := b.rotate(f)
				if newF == nil {
					// Rotation failed — drain remaining writes and stop.
					for range b.writeCh {
					}
					return
				}
				f = newF
				written = 0
				sinceLastSample = 0
			}
		} else {
			b.indexMu.Lock()
			b.total = written
			b.indexMu.Unlock()
		}
	}

	// Channel closed — flush final total and signal readers.
	f.Close()
	b.indexMu.Lock()
	b.total = written
	b.indexMu.Unlock()
	close(b.drainDone)
}

// rotate closes oldFile, schedules its deletion after rotationGracePeriod, and opens
// a new file. It resets the index and written counters. Returns the new file, or nil
// if the new file could not be opened. The caller must NOT hold indexMu.
func (b *DiskBuffer) rotate(oldFile *os.File) *os.File {
	oldFile.Close()

	b.indexMu.RLock()
	oldPath := b.filePath
	dir := filepath.Dir(b.filePath)
	b.indexMu.RUnlock()

	// Delete old file after grace period so in-flight timeshift readers can finish.
	go func() {
		time.Sleep(rotationGracePeriod)
		if err := os.Remove(oldPath); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to remove rotated file %s: %v", oldPath, err)
		}
	}()

	newTime := time.Now()
	newPath := newFilePath(dir, b.streamID, newTime)
	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open rotated buffer file %s: %v", newPath, err)
		return nil
	}

	b.indexMu.Lock()
	b.filePath = newPath
	b.startTime = newTime
	b.index = nil
	b.total = 0
	b.baseOffset = 0
	b.indexMu.Unlock()

	utils.InfoLog("Catchup: rotated buffer for stream %s → %s (old file deleted in %s)", b.streamID, newPath, rotationGracePeriod)
	return f
}

// Write enqueues a copy of p for async disk write. Never blocks; drops if channel full.
func (b *DiskBuffer) Write(p []byte) {
	if atomic.LoadInt32(&b.stopped) != 0 {
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
	if atomic.CompareAndSwapInt32(&b.stopped, 0, 1) {
		b.stoppedMu.Lock()
		b.stoppedAt = time.Now()
		b.stoppedMu.Unlock()
		close(b.writeCh)
	}
}

// Delete removes the buffer file from disk. Call only after Stop().
func (b *DiskBuffer) Delete() error {
	b.Stop() // idempotent
	b.indexMu.RLock()
	fpath := b.filePath
	b.indexMu.RUnlock()
	return os.Remove(fpath)
}

// IsStopped reports whether Stop has been called.
func (b *DiskBuffer) IsStopped() bool { return atomic.LoadInt32(&b.stopped) != 0 }

// DrainDone returns a channel that is closed once all queued writes have been flushed to disk.
// Use this (not IsStopped) as the signal to stop reading from the buffer file.
func (b *DiskBuffer) DrainDone() <-chan struct{} { return b.drainDone }

// StoppedAt returns when Stop was called (zero if not yet stopped).
func (b *DiskBuffer) StoppedAt() time.Time {
	b.stoppedMu.Lock()
	defer b.stoppedMu.Unlock()
	return b.stoppedAt
}

// FilePath returns the path of the current buffer file.
// The path changes on each file rotation, so callers should open the file immediately
// after obtaining the path if they intend to read from it.
func (b *DiskBuffer) FilePath() string {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.filePath
}

// StreamID returns the stream ID this buffer belongs to.
func (b *DiskBuffer) StreamID() string { return b.streamID }

// StartTime returns the wall-clock time when the current buffer file started recording.
// This is reset on each file rotation.
func (b *DiskBuffer) StartTime() time.Time {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.startTime
}

// BytesBuffered returns the number of bytes written to the current file.
func (b *DiskBuffer) BytesBuffered() int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.total
}

// OffsetForTime returns the byte offset in the current file that corresponds to wallTime t.
// Returns 0 if t predates the current file's startTime (data from before the last rotation
// is no longer available). Returns total if t is in the future.
func (b *DiskBuffer) OffsetForTime(t time.Time) int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()

	if len(b.index) == 0 {
		return 0
	}

	// Binary search: find last entry whose wallTime <= t
	pos := sort.Search(len(b.index), func(i int) bool {
		return b.index[i].wallTime.After(t)
	})
	pos-- // pos is the first entry AFTER t; pos-1 is what we want

	if pos < 0 {
		return 0
	}
	return b.index[pos].byteOffset
}

// newFilePath returns the canonical path for a buffer file in dir.
func newFilePath(dir, streamID string, t time.Time) string {
	return filepath.Join(dir, fmt.Sprintf("%s_%d.ts", sanitizeID(streamID), t.Unix()))
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
