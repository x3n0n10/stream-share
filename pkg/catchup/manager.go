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
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

// bufferGracePeriod is how long a stopped buffer's file is kept on disk before
// deletion. This allows timeshift requests that arrive just after the live stream
// ends (e.g. TiviMate closing the live connection before opening a rewind) to
// still find and read the buffer.
const bufferGracePeriod = 60 * time.Second

// estimatedBitsBytesPerHour is used to derive maxBytes from duration in hours.
// 10 Mbps is a reasonable upper bound for HD IPTV; users can lower CATCHUP_DURATION
// to reduce disk usage.
const estimatedBytesPerHour = 10 * 1024 * 1024 / 8 * 3600 // ~4.5 GB/h

// Manager tracks active DiskBuffers and which stream IDs have native upstream catchup.
type Manager struct {
	dir      string
	duration int // hours: controls both advertised window and disk retention cap
	enabled  bool

	mu              sync.RWMutex
	buffers         map[string]*DiskBuffer
	upstreamCatchup map[string]bool // stream_id (no extension) → true if native tv_archive=1

	stopChan chan struct{}
}

// New creates a Manager. When enabled is false all methods are safe no-ops.
func New(enabled bool, dir string, duration int) *Manager {
	if duration <= 0 {
		duration = 4
	}
	m := &Manager{
		enabled:         enabled,
		dir:             dir,
		duration:        duration,
		buffers:         make(map[string]*DiskBuffer),
		upstreamCatchup: make(map[string]bool),
		stopChan:        make(chan struct{}),
	}
	if enabled {
		go m.cleanupGoroutine()
	}
	return m
}

// cleanupGoroutine periodically removes stopped buffers whose grace period has elapsed.
func (m *Manager) cleanupGoroutine() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-m.stopChan:
			return
		case <-ticker.C:
			m.cleanupStoppedBuffers()
		}
	}
}

func (m *Manager) cleanupStoppedBuffers() {
	threshold := time.Now().Add(-bufferGracePeriod)

	m.mu.Lock()
	var toDelete []string
	for id, buf := range m.buffers {
		if t := buf.StoppedAt(); !t.IsZero() && t.Before(threshold) {
			toDelete = append(toDelete, id)
		}
	}
	for _, id := range toDelete {
		buf := m.buffers[id]
		delete(m.buffers, id)
		if err := buf.Delete(); err != nil {
			utils.WarnLog("Catchup: grace-period cleanup failed for stream %s: %v", id, err)
		} else {
			utils.InfoLog("Catchup: grace-period cleanup deleted buffer for stream %s", id)
		}
	}
	m.mu.Unlock()
}

// IsEnabled reports whether local catchup buffering is active.
func (m *Manager) IsEnabled() bool {
	return m != nil && m.enabled
}

// AdvertisedHours returns the number of hours to advertise to clients via tv_archive_duration.
func (m *Manager) AdvertisedHours() int {
	if m == nil {
		return 0
	}
	return m.duration
}

// StartBuffer creates a new DiskBuffer for streamID, stopping and deleting any
// previous buffer for the same ID first. Returns nil when catchup is disabled.
func (m *Manager) StartBuffer(streamID string) *DiskBuffer {
	if !m.IsEnabled() {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o755); err != nil {
		utils.ErrorLog("Catchup: failed to create buffer dir %s: %v", m.dir, err)
		return nil
	}

	maxBytes := int64(m.duration) * estimatedBytesPerHour

	m.mu.Lock()
	defer m.mu.Unlock()

	// Remove previous buffer for this stream if present.
	if old, ok := m.buffers[streamID]; ok {
		_ = old.Delete()
		delete(m.buffers, streamID)
	}

	buf, err := NewDiskBuffer(m.dir, streamID, time.Now(), maxBytes)
	if err != nil {
		utils.ErrorLog("Catchup: failed to start buffer for stream %s: %v", streamID, err)
		return nil
	}
	m.buffers[streamID] = buf
	utils.InfoLog("Catchup: started buffer for stream %s at %s", streamID, buf.FilePath())
	return buf
}

// GetBuffer returns the active DiskBuffer for streamID, or nil if none exists.
func (m *Manager) GetBuffer(streamID string) *DiskBuffer {
	if !m.IsEnabled() {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.buffers[streamID]
}

// StopBuffer stops writing to the buffer for streamID but keeps the file on disk
// for bufferGracePeriod so that timeshift requests arriving just after the live
// stream ends can still read from it. The cleanup goroutine deletes it later.
func (m *Manager) StopBuffer(streamID string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	buf, ok := m.buffers[streamID]
	m.mu.RUnlock()

	if ok {
		buf.Stop()
		utils.InfoLog("Catchup: stopped writing buffer for stream %s (file kept for %s grace period)", streamID, bufferGracePeriod)
	}
}

// DeleteBuffer stops and immediately removes the buffer file for streamID.
// Safe to call while rewind readers hold open handles (Linux unlink semantics).
func (m *Manager) DeleteBuffer(streamID string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	buf, ok := m.buffers[streamID]
	if ok {
		delete(m.buffers, streamID)
	}
	m.mu.Unlock()

	if ok {
		if err := buf.Delete(); err != nil {
			utils.WarnLog("Catchup: failed to delete buffer for stream %s: %v", streamID, err)
		} else {
			utils.InfoLog("Catchup: deleted buffer for stream %s", streamID)
		}
	}
}

// SetUpstreamCatchup replaces the map of stream IDs that have native upstream catchup support.
// Keys are stream_id values (numeric string, no file extension).
func (m *Manager) SetUpstreamCatchup(supported map[string]bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstreamCatchup = supported
}

// HasUpstreamCatchup reports whether the upstream provider natively supports catchup
// for the given id (which may include a file extension like ".ts").
func (m *Manager) HasUpstreamCatchup(id string) bool {
	if m == nil {
		return false
	}
	// Normalize: strip extension to match the stream_id key format.
	bare := id
	if dot := strings.LastIndex(id, "."); dot >= 0 {
		bare = id[:dot]
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.upstreamCatchup[bare]
}

// Cleanup stops the background goroutine and deletes all active buffers.
// Called on server shutdown.
func (m *Manager) Cleanup() {
	if m == nil {
		return
	}
	// Stop the cleanup goroutine.
	select {
	case <-m.stopChan:
	default:
		close(m.stopChan)
	}

	m.mu.Lock()
	buffers := m.buffers
	m.buffers = make(map[string]*DiskBuffer)
	m.mu.Unlock()

	for id, buf := range buffers {
		if err := buf.Delete(); err != nil {
			utils.WarnLog("Catchup: cleanup failed to delete buffer for %s: %v", id, err)
		}
	}
}

// CleanupOldFiles removes stale buffer files (*.ts) from the buffer directory
// that were created before this process started (i.e., from a prior run).
func (m *Manager) CleanupOldFiles() {
	if m == nil || m.dir == "" {
		return
	}
	processStart := time.Now()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			utils.WarnLog("Catchup: could not read buffer dir %s: %v", m.dir, err)
		}
		return
	}
	removed := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(processStart) {
			p := filepath.Join(m.dir, e.Name())
			if rerr := os.Remove(p); rerr == nil {
				removed++
			}
		}
	}
	if removed > 0 {
		utils.InfoLog("Catchup: removed %d stale buffer file(s) from %s", removed, m.dir)
	}
}
