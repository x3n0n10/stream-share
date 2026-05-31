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
	"strings"
	"sync"
	"time"
)

const indexSampleBytes = 1 * 1024 * 1024 // record a time→offset entry every 1MB

type indexEntry struct {
	wallTime   time.Time
	byteOffset int64
}

// DiskBuffer is an append-only file that records a live stream for catchup playback.
// One goroutine writes via Write(); any number of goroutines may open independent
// read handles via FilePath() and read concurrently — Linux guarantees safe
// concurrent read/append on separate file descriptors.
type DiskBuffer struct {
	filePath  string
	startTime time.Time
	maxBytes  int64 // 0 = unlimited; cap on (total - baseOffset)

	mu         sync.RWMutex
	f          *os.File     // write handle; nil after Stop()
	total      int64        // bytes appended to the file so far
	baseOffset int64        // earliest byte offset readers may seek to
	index      []indexEntry // time→offset samples, protected by mu
	stopped    bool
}

// NewDiskBuffer opens (or creates) the buffer file for streamID in dir.
// maxBytes limits how much data is retained (baseOffset advances when exceeded).
// 0 means unlimited.
func NewDiskBuffer(dir, streamID string, start time.Time, maxBytes int64) (*DiskBuffer, error) {
	safe := sanitizeStreamID(streamID)
	name := fmt.Sprintf("%s_%d.ts", safe, start.Unix())
	p := filepath.Join(dir, name)

	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("catchup: create buffer file %s: %w", p, err)
	}

	return &DiskBuffer{
		filePath:  p,
		startTime: start,
		maxBytes:  maxBytes,
		f:         f,
	}, nil
}

// sanitizeStreamID strips characters unsafe for file names.
func sanitizeStreamID(id string) string {
	id = strings.ReplaceAll(id, "/", "_")
	id = strings.ReplaceAll(id, "..", "_")
	id = strings.ReplaceAll(id, ".", "_")
	return id
}

// Write appends p to the buffer file and updates the time index.
// Must be called from a single goroutine (the stream pump).
func (b *DiskBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.stopped || b.f == nil {
		return 0, fmt.Errorf("catchup: buffer stopped")
	}

	n, err := b.f.Write(p)
	if n > 0 {
		// Record an index entry every indexSampleBytes.
		prevSamples := b.total / indexSampleBytes
		b.total += int64(n)
		newSamples := b.total / indexSampleBytes
		if newSamples > prevSamples {
			b.index = append(b.index, indexEntry{
				wallTime:   time.Now(),
				byteOffset: b.total - int64(n),
			})
		}

		// Advance baseOffset to enforce the size cap.
		if b.maxBytes > 0 && b.total-b.baseOffset > b.maxBytes {
			b.baseOffset = b.total - b.maxBytes
		}
	}
	return n, err
}

// BytesBuffered returns the total number of bytes written so far.
func (b *DiskBuffer) BytesBuffered() int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.total
}

// StartTime returns when buffering began.
func (b *DiskBuffer) StartTime() time.Time {
	return b.startTime
}

// OffsetForTime returns the best byte offset for the given wall-clock time.
// The result is clamped to baseOffset (oldest readable position).
// Returns baseOffset if t is before or at startTime.
func (b *DiskBuffer) OffsetForTime(t time.Time) int64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if !t.After(b.startTime) || len(b.index) == 0 {
		return b.baseOffset
	}

	// Find the last index entry whose wallTime is <= t.
	i := sort.Search(len(b.index), func(i int) bool {
		return b.index[i].wallTime.After(t)
	})
	var offset int64
	if i == 0 {
		offset = 0
	} else {
		offset = b.index[i-1].byteOffset
	}

	if offset < b.baseOffset {
		offset = b.baseOffset
	}
	return offset
}

// FilePath returns the path to the buffer file so readers can open their own handle.
func (b *DiskBuffer) FilePath() string {
	return b.filePath
}

// IsStopped reports whether the write side has been closed.
func (b *DiskBuffer) IsStopped() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.stopped
}

// Stop closes the write handle. Readers may continue via their own open handles.
func (b *DiskBuffer) Stop() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.stopped {
		return
	}
	b.stopped = true
	if b.f != nil {
		_ = b.f.Close()
		b.f = nil
	}
}

// Delete removes the buffer file from disk. On Linux, existing reader file
// handles remain valid until closed (the inode is kept alive by the kernel).
func (b *DiskBuffer) Delete() error {
	b.Stop()
	err := os.Remove(b.filePath)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
