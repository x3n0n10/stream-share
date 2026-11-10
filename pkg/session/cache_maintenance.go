/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 * Copyright (C) 2026  x3n0n10
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

package session

import (
	"github.com/lucasduport/stream-share/pkg/slate"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// runCacheMaintenance reaps everything that accumulates on disk: expired temp
// links, expired and stale VOD cache files (plus their .part siblings), orphaned
// VOD files no longer referenced by any row, and stale slate clips.
func (sm *SessionManager) runCacheMaintenance() {
	if sm.db != nil {
		if count, err := sm.db.CleanupExpiredLinks(); err != nil {
			utils.ErrorLog("Failed to clean expired links: %v", err)
		} else if count > 0 {
			utils.InfoLog("Cleaned %d expired temporary links", count)
		}
		// Order matters: delete expired/stale entries' files *before* their rows,
		// then reap anything left unreferenced (e.g. failed downloads' .part files).
		sm.cleanupExpiredVODFiles()
		sm.cleanupStaleVODFiles()
		sm.reapVODOrphans()
	}
	if sm.slateStaleAge > 0 && sm.slateCacheDir != "" {
		if n, err := slate.PruneCache(sm.slateCacheDir, sm.slateStaleAge); err != nil {
			utils.WarnLog("Slate cache prune failed: %v", err)
		} else if n > 0 {
			utils.InfoLog("Pruned %d stale slate clip(s)", n)
		}
	}
}
