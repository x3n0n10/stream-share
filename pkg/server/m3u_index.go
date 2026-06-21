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

package server

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/lucasduport/stream-share/pkg/utils"
)

var (
	channelIndexMu    sync.RWMutex
	channelIndex      map[string]string
	channelIndexPath  string
	channelIndexMTime time.Time

	// apiChannelIndex maps normalized stream IDs to channel names harvested from
	// the Xtream get_live_streams API response. It is populated whenever a player
	// fetches the live stream list, so name resolution works even when the
	// proxified M3U playlist was never generated (pure Xtream API mode).
	apiChannelIndexMu sync.RWMutex
	apiChannelIndex   map[string]string
)

// updateAPIChannelIndex replaces the API-sourced name index with id→name pairs.
func (c *Config) updateAPIChannelIndex(names map[string]string) {
	if len(names) == 0 {
		return
	}
	apiChannelIndexMu.Lock()
	apiChannelIndex = names
	apiChannelIndexMu.Unlock()
	if err := c.db.UpsertStreamNames(names, "api"); err != nil {
		utils.WarnLog("stream_names: failed to persist API channel index: %v", err)
	}
}

// lookupAPIChannelName returns the channel name for a normalized stream ID.
func lookupAPIChannelName(normalizedID string) (string, bool) {
	apiChannelIndexMu.RLock()
	defer apiChannelIndexMu.RUnlock()
	if apiChannelIndex == nil {
		return "", false
	}
	name, ok := apiChannelIndex[normalizedID]
	return name, ok
}

// normalizeStreamID trims common file extensions from the last path segment
func normalizeStreamID(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	return id
}

// ensureChannelIndex parses c.proxyfiedM3UPath and builds/refreshes the channelIndex cache
func (c *Config) ensureChannelIndex() {
	m3uPath := c.proxyfiedM3UPath
	if strings.TrimSpace(m3uPath) == "" {
		return
	}
	info, err := os.Stat(m3uPath)
	if err != nil {
		return
	}

	// Fast path: unchanged
	channelIndexMu.RLock()
	same := channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime())
	channelIndexMu.RUnlock()
	if same {
		return
	}

	// Rebuild under write lock (double-check inside)
	channelIndexMu.Lock()
	defer channelIndexMu.Unlock()

	if channelIndex != nil && channelIndexPath == m3uPath && channelIndexMTime.Equal(info.ModTime()) {
		return
	}

	f, err := os.Open(m3uPath)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	lastTitle := ""
	newIndex := make(map[string]string, 4096)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#EXTINF:") {
			if idx := strings.LastIndex(line, ","); idx != -1 && idx+1 < len(line) {
				lastTitle = strings.TrimSpace(line[idx+1:])
			} else {
				lastTitle = ""
			}
			continue
		}
		if strings.HasPrefix(line, "http://") || strings.HasPrefix(line, "https://") {
			u, err := url.Parse(line)
			if err == nil {
				base := path.Base(u.Path)
				id := normalizeStreamID(base)
				if lastTitle != "" {
					newIndex[id] = lastTitle
				}
			}
			lastTitle = ""
		}
	}
	// best-effort index

	// Write to DB (best-effort, non-fatal)
	if err := c.db.UpsertStreamNames(newIndex, "m3u"); err != nil {
		utils.WarnLog("stream_names: failed to persist M3U channel index: %v", err)
	}

	channelIndex = newIndex
	channelIndexPath = m3uPath
	channelIndexMTime = info.ModTime()
}

// warmChannelIndexFromDB seeds the in-memory indices from the database on startup.
func (c *Config) warmChannelIndexFromDB() {
	bySource, err := c.db.LoadStreamNames()
	if err != nil {
		utils.WarnLog("stream_names: failed to load from DB: %v", err)
		return
	}
	if m3uNames := bySource["m3u"]; len(m3uNames) > 0 {
		channelIndexMu.Lock()
		if channelIndex == nil {
			channelIndex = m3uNames
		}
		channelIndexMu.Unlock()
	}
	if apiNames := bySource["api"]; len(apiNames) > 0 {
		apiChannelIndexMu.Lock()
		if apiChannelIndex == nil {
			apiChannelIndex = apiNames
		}
		apiChannelIndexMu.Unlock()
	}
	if vodNames := bySource["vod"]; len(vodNames) > 0 {
		vodNameMu.Lock()
		for id, name := range vodNames {
			if _, exists := vodNameIndex[id]; !exists {
				vodNameIndex[id] = name
			}
		}
		vodNameMu.Unlock()
	}
	total := len(bySource["m3u"]) + len(bySource["api"]) + len(bySource["vod"])
	if total > 0 {
		utils.InfoLog("stream_names: loaded %d names from DB (m3u=%d, api=%d, vod=%d)",
			total, len(bySource["m3u"]), len(bySource["api"]), len(bySource["vod"]))
	}
}

// getChannelNameByID returns the channel name for a given stream ID if known.
// It prefers the proxified M3U index and falls back to names harvested from the
// Xtream get_live_streams API, so resolution works in both M3U and API modes.
func (c *Config) getChannelNameByID(streamID string) (string, bool) {
	id := normalizeStreamID(streamID)

	c.ensureChannelIndex()
	channelIndexMu.RLock()
	if channelIndex != nil {
		if name, ok := channelIndex[id]; ok {
			channelIndexMu.RUnlock()
			return name, true
		}
	}
	channelIndexMu.RUnlock()

	return lookupAPIChannelName(id)
}

// streamLabel formats a stream for logging as "Name (Stream <id>)", falling back
// to "Stream <id>" when no name is known. It resolves live channel names and any
// already-cached VOD titles without network I/O. The id is reported without its
// file extension for readability.
func (c *Config) streamLabel(streamID string) string {
	id := normalizeStreamID(streamID)
	if name, ok := c.resolveStreamName(streamID); ok && strings.TrimSpace(name) != "" {
		return fmt.Sprintf("%s (Stream %s)", strings.TrimSpace(name), id)
	}
	return fmt.Sprintf("Stream %s", id)
}
