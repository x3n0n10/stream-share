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
	indexSampleInterval = 1 << 20        // sample every 1 MB
	writeChanCap        = 2048           // ~256 MB slack at 128 KB chunks before drops
	rotationGracePeriod = 60 * time.Second
)

type indexEntry struct {
	wallTime   time.Time
	byteOffset int64
}

// DiskBuffer writes an append-only MPEG-TS file and maintains a time→offset index.
// All disk writes happen in a dedicated goroutine so callers are never blocked.
//
// When maxBytes > 0 and the current file reaches that size it is rotated: it becomes
// the "previous" file (still seekable for the next full window), a new file is opened
// for continued recording, and the file that was previously in "prev" position is
// deleted after rotationGracePeriod so in-flight readers can finish. This keeps at
// most two files on disk (≤ 2×maxBytes) while always providing a full window of
// seekable history.
type DiskBuffer struct {
	streamID string
	maxBytes int64 // 0 = unlimited

	writeCh   chan []byte  // pump goroutine sends slices here
	drainDone chan struct{} // closed by drainLoop when all writes are flushed to disk
	stopped   int32        // 0 = running, 1 = stopped; accessed via atomic

	stoppedMu sync.Mutex
	stoppedAt time.Time

	// indexMu guards all fields below.
	indexMu sync.RWMutex

	// Current (actively-written) file.
	filePath  string
	startTime time.Time
	index     []indexEntry
	total     int64

	// Previous file (result of the most recent rotation). Empty when no rotation has
	// occurred. Seekable for times between prevStartTime and startTime.
	prevFilePath  string
	prevStartTime time.Time
	prevIndex     []indexEntry
	prevTotal     int64
}

// NewDiskBuffer creates a buffer that writes to dir/<streamID>_<unix>.ts.
// maxBytes == 0 means unlimited; otherwise the file is rotated when it reaches maxBytes,
// keeping the previous segment available for seeking.
func NewDiskBuffer(dir, streamID string, start time.Time, maxBytes int64) (*DiskBuffer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("catchup: mkdir %s: %w", dir, err)
	}
	b := &DiskBuffer{
		streamID:  streamID,
		filePath:  newFilePath(dir, streamID, start),
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
	path := b.filePath
	b.indexMu.RUnlock()

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", path, err)
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

			if b.maxBytes > 0 && written >= b.maxBytes {
				newF := b.rotate(f)
				if newF == nil {
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

	f.Close()
	b.indexMu.Lock()
	b.total = written
	b.indexMu.Unlock()
	close(b.drainDone)
}

// rotate closes oldFile, promotes it to "previous", opens a fresh current file,
// and schedules deletion of the file that was previously in "prev" position.
// The caller must NOT hold indexMu. Returns the new file, or nil on error.
func (b *DiskBuffer) rotate(oldFile *os.File) *os.File {
	oldFile.Close()

	b.indexMu.Lock()
	evictPath := b.prevFilePath // two rotations old — safe to remove after grace

	b.prevFilePath = b.filePath
	b.prevStartTime = b.startTime
	b.prevIndex = b.index
	b.prevTotal = b.total

	newTime := time.Now()
	newPath := newFilePath(filepath.Dir(b.filePath), b.streamID, newTime)
	b.filePath = newPath
	b.startTime = newTime
	b.index = nil
	b.total = 0
	b.indexMu.Unlock()

	if evictPath != "" {
		go func() {
			time.Sleep(rotationGracePeriod)
			if err := os.Remove(evictPath); err != nil && !os.IsNotExist(err) {
				utils.WarnLog("Catchup: failed to remove evicted buffer file %s: %v", evictPath, err)
			}
		}()
	}

	f, err := os.OpenFile(newPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open rotated buffer file %s: %v", newPath, err)
		return nil
	}
	utils.InfoLog("Catchup: rotated buffer for stream %s → %s", b.streamID, newPath)
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

// Delete removes all buffer files from disk. Call only after Stop().
func (b *DiskBuffer) Delete() error {
	b.Stop()
	b.indexMu.RLock()
	cur := b.filePath
	prev := b.prevFilePath
	b.indexMu.RUnlock()
	_ = os.Remove(prev)
	return os.Remove(cur)
}

// IsStopped reports whether Stop has been called.
func (b *DiskBuffer) IsStopped() bool { return atomic.LoadInt32(&b.stopped) != 0 }

// DrainDone returns a channel closed once all queued writes are flushed to the current file.
func (b *DiskBuffer) DrainDone() <-chan struct{} { return b.drainDone }

// StoppedAt returns when Stop was called (zero if not yet stopped).
func (b *DiskBuffer) StoppedAt() time.Time {
	b.stoppedMu.Lock()
	defer b.stoppedMu.Unlock()
	return b.stoppedAt
}

// FilePath returns the path of the current (actively-written) file.
func (b *DiskBuffer) FilePath() string {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.filePath
}

// FileForTime returns the file path and byte offset that best serve wallTime t.
// If t falls within the previous file's window it returns the previous file path;
// otherwise it returns the current file. Callers serving from the previous file should
// continue into the current file (from offset 0) once the previous file reaches EOF.
func (b *DiskBuffer) FileForTime(t time.Time) (filePath string, offset int64) {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	if b.prevFilePath != "" && t.Before(b.startTime) {
		return b.prevFilePath, searchIndex(b.prevIndex, t)
	}
	return b.filePath, searchIndex(b.index, t)
}

// StreamID returns the stream ID this buffer belongs to.
func (b *DiskBuffer) StreamID() string { return b.streamID }

// StartTime returns the wall-clock start of the current file (reset on each rotation).
func (b *DiskBuffer) StartTime() time.Time {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.startTime
}

// BytesBuffered returns bytes written to the current file.
func (b *DiskBuffer) BytesBuffered() int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return b.total
}

// OffsetForTime returns the byte offset within the current file for wallTime t.
// Callers that need cross-file seeking (rewinds spanning a rotation boundary) should
// use FileForTime instead.
func (b *DiskBuffer) OffsetForTime(t time.Time) int64 {
	b.indexMu.RLock()
	defer b.indexMu.RUnlock()
	return searchIndex(b.index, t)
}

// searchIndex binary-searches idx and returns the byte offset for t.
func searchIndex(idx []indexEntry, t time.Time) int64 {
	if len(idx) == 0 {
		return 0
	}
	pos := sort.Search(len(idx), func(i int) bool {
		return idx[i].wallTime.After(t)
	})
	pos-- // first entry AFTER t → pos-1 is what we want
	if pos < 0 {
		return 0
	}
	return idx[pos].byteOffset
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
