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

const gracePeriod = 60 * time.Second

// Manager owns all active and grace-period DiskBuffers.
type Manager struct {
	enabled  bool
	dir      string
	duration int // hours — controls both advertised window and disk retention
	grace    time.Duration

	mu              sync.Mutex
	buffers         map[string]*DiskBuffer   // streamID → buffer (active or in grace period)
	graceCancels    map[string]chan struct{} // streamID → cancel chan for grace goroutine
	holds           map[*DiskBuffer]int      // buffer → catchup reads holding its files (see Hold)
	upstreamCatchup map[string]bool          // streamID → has native tv_archive=1
	noArchive       map[string]time.Time     // streamID → when the provider had no recording despite tv_archive=1
}

// noArchiveFor is how long a provider's missing recording overrules its
// channel list's archive flag; after that the provider gets another chance.
const noArchiveFor = 24 * time.Hour

// New creates a Manager. If !enabled, all methods are no-ops.
func New(enabled bool, dir string, duration int) *Manager {
	if duration <= 0 {
		duration = 4
	}
	m := &Manager{
		enabled:         enabled,
		dir:             dir,
		duration:        duration,
		grace:           gracePeriod,
		buffers:         make(map[string]*DiskBuffer),
		graceCancels:    make(map[string]chan struct{}),
		holds:           make(map[*DiskBuffer]int),
		upstreamCatchup: make(map[string]bool),
		noArchive:       make(map[string]time.Time),
	}
	return m
}

// IsEnabled returns whether the catchup feature is active.
func (m *Manager) IsEnabled() bool { return m.enabled }

// SetGracePeriod sets how long a stopped buffer's files are kept (tests).
func (m *Manager) SetGracePeriod(d time.Duration) { m.grace = d }

// AdvertisedHours returns the number of hours to advertise in tv_archive_duration.
func (m *Manager) AdvertisedHours() int { return m.duration }

// maxBytes converts duration hours to a byte cap at an assumed 10 Mbps bitrate.
// Returns 0 (unlimited) when duration is 0.
func (m *Manager) maxBytes() int64 {
	if m.duration == 0 {
		return 0
	}
	// 10 Mbps = 10_000_000 bits/sec = 1_250_000 bytes/sec
	return int64(m.duration) * 3600 * 1_250_000
}

// StartBuffer creates a new DiskBuffer for streamID, replacing any existing one.
// Returns nil if not enabled or on error.
func (m *Manager) StartBuffer(streamID string) *DiskBuffer {
	if !m.enabled {
		return nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Cancel any pending grace-period deletion for this stream
	if cancel, ok := m.graceCancels[streamID]; ok {
		close(cancel)
		delete(m.graceCancels, streamID)
	}

	// Remove and clean up any existing buffer; a held one goes on its last release.
	if existing, ok := m.buffers[streamID]; ok {
		existing.Stop()
		if m.holds[existing] == 0 {
			_ = existing.Delete()
		}
		delete(m.buffers, streamID)
	}

	buf, err := NewDiskBuffer(m.dir, streamID, time.Now(), m.maxBytes())
	if err != nil {
		utils.ErrorLog("Catchup: failed to create buffer for stream %s: %v", streamID, err)
		return nil
	}
	m.buffers[streamID] = buf
	utils.DebugLog("Catchup: started buffer for stream %s at %s", streamID, buf.FilePath())
	return buf
}

// GetBuffer returns the buffer for streamID (active or in grace period), or nil if none.
func (m *Manager) GetBuffer(streamID string) *DiskBuffer {
	if !m.enabled {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, buf := m.lookup(streamID)
	return buf
}

// lookup finds the buffer for streamID with or without an extension, and
// returns its bare id. The caller holds m.mu.
func (m *Manager) lookup(streamID string) (bare string, buf *DiskBuffer) {
	bare = strings.TrimSuffix(streamID, filepath.Ext(streamID))
	return bare, m.buffers[bare]
}

// Hold returns the buffer for streamID (active or in grace period, with or
// without an extension) and keeps its files until release (idempotent) is
// called: the one owner of when a buffer's files may go. It returns nil and a
// no-op release when there is none.
func (m *Manager) Hold(streamID string) (buf *DiskBuffer, release func()) {
	if !m.enabled {
		return nil, func() {}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	bare, buf := m.lookup(streamID)
	if buf == nil {
		return nil, func() {}
	}
	m.holds[buf]++
	if cancel, ok := m.graceCancels[bare]; ok { // the last release re-arms it
		close(cancel)
		delete(m.graceCancels, bare)
	}
	var once sync.Once
	return buf, func() { once.Do(func() { m.release(bare, buf) }) }
}

func (m *Manager) release(streamID string, buf *DiskBuffer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.holds[buf]--; m.holds[buf] > 0 {
		return
	}
	delete(m.holds, buf)
	switch {
	case m.buffers[streamID] != buf: // replaced (or cleaned up) while held
		deleteFiles(buf)
	case buf.Stopped():
		m.armGrace(streamID, buf)
	}
}

// StopBuffer stops writing to the buffer but keeps its files for the grace
// period, so a player's following timeshift requests still find them. They
// go a grace after the later of the stop and the last held read's release.
func (m *Manager) StopBuffer(streamID string) {
	if !m.enabled {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	buf, ok := m.buffers[streamID]
	if !ok {
		return
	}
	buf.Stop()
	if m.holds[buf] == 0 { // else the last release arms it
		m.armGrace(streamID, buf)
	}
}

// armGrace (re)starts the grace after which buf's files are deleted, unless
// StartBuffer replaced buf; Hold cancels it. The caller holds m.mu.
func (m *Manager) armGrace(streamID string, buf *DiskBuffer) {
	if cancel, ok := m.graceCancels[streamID]; ok {
		close(cancel)
	}
	cancel := make(chan struct{})
	m.graceCancels[streamID] = cancel

	go func() {
		select {
		case <-time.After(m.grace):
		case <-cancel:
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		if m.graceCancels[streamID] == cancel {
			delete(m.graceCancels, streamID)
		}
		if current, exists := m.buffers[streamID]; exists && current == buf {
			deleteFiles(buf)
			delete(m.buffers, streamID)
			utils.DebugLog("Catchup: grace period expired, deleted buffer for stream %s", streamID)
		}
	}()
}

// Cleanup stops and deletes all buffers. Call on server shutdown.
func (m *Manager) Cleanup() {
	if !m.enabled {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, cancel := range m.graceCancels {
		close(cancel)
	}
	m.graceCancels = make(map[string]chan struct{})

	for streamID, buf := range m.buffers {
		buf.Stop()
		deleteFiles(buf)
		delete(m.buffers, streamID)
	}
	for buf := range m.holds { // replaced buffers still held
		deleteFiles(buf)
	}
	utils.InfoLog("Catchup: all buffers cleaned up")
}

// deleteFiles stops buf and removes its files, logging a failure.
func deleteFiles(buf *DiskBuffer) {
	if err := buf.Delete(); err != nil && !os.IsNotExist(err) {
		utils.WarnLog("Catchup: failed to delete buffer file %s: %v", buf.FilePath(), err)
	}
}

// CleanupOldFiles removes stale *.ts buffer files left from a previous run.
func (m *Manager) CleanupOldFiles() {
	if !m.enabled {
		return
	}

	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to read buffer dir %s: %v", m.dir, err)
		}
		return
	}

	count := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".ts") {
			continue
		}
		p := filepath.Join(m.dir, e.Name())
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			utils.WarnLog("Catchup: failed to remove stale file %s: %v", p, err)
		} else {
			count++
		}
	}
	if count > 0 {
		utils.InfoLog("Catchup: removed %d stale buffer file(s) from %s", count, m.dir)
	}
}

// SetUpstreamCatchup stores which stream IDs have native tv_archive support.
// Keys should be bare stream IDs (no extension).
func (m *Manager) SetUpstreamCatchup(supported map[string]bool) {
	if !m.enabled {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.upstreamCatchup = supported
}

// HasUpstreamCatchup returns true if the stream has native upstream catchup support.
// id may include an extension (e.g. "12345.ts"); the extension is stripped before lookup.
func (m *Manager) HasUpstreamCatchup(id string) bool {
	if !m.enabled {
		return true // safe default: always proxy upstream when catchup is disabled
	}
	bare := strings.TrimSuffix(id, filepath.Ext(id))
	m.mu.Lock()
	defer m.mu.Unlock()
	if at, ok := m.noArchive[bare]; ok {
		if time.Since(at) < noArchiveFor {
			return false
		}
		delete(m.noArchive, bare)
	}
	return m.upstreamCatchup[bare]
}

// MarkNoUpstreamArchive records that the provider had no recording for a
// channel its list flags as archived. For noArchiveFor the channel counts as
// unarchived: watching it records a local buffer and catchup reads that.
func (m *Manager) MarkNoUpstreamArchive(id string) {
	if !m.enabled {
		return
	}
	bare := strings.TrimSuffix(id, filepath.Ext(id))
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.upstreamCatchup[bare] {
		return
	}
	if _, ok := m.noArchive[bare]; !ok {
		utils.InfoLog("Catchup: provider has no recording for stream %s although its channel list says archived; recording it locally for %s", bare, noArchiveFor)
	}
	m.noArchive[bare] = time.Now()
}
