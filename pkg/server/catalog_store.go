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

package server

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/lucasduport/stream-share/pkg/filter"
)

// catalogStore is a cached provider catalogue read one item at a time, so a
// store need not keep the decoded list in memory.
type catalogStore interface {
	// each calls fn for every item that is a JSON object, until fn returns false.
	each(fn func(m map[string]interface{}) bool) error
}

// memoryCatalog is a decoded answer held in memory. Anything that is not a
// list has no items.
type memoryCatalog struct{ data interface{} }

func (c *memoryCatalog) each(fn func(m map[string]interface{}) bool) error {
	items, _ := c.data.([]interface{})
	for _, it := range items {
		if m, ok := it.(map[string]interface{}); ok && !fn(m) {
			break
		}
	}
	return nil
}

// catalogStoreOf views a cached value as a catalogStore: the cache may still
// hold a raw decoded list.
func catalogStoreOf(data interface{}) catalogStore {
	if s, ok := data.(catalogStore); ok {
		return s
	}
	return &memoryCatalog{data: data}
}

// keepCatalogEntry reports whether the catalogue filter keeps one stream or
// series entry, judged on its category name (looked up in names), its name and
// its adult flag.
func (c *Config) keepCatalogEntry(kind filter.Kind, m map[string]interface{}, names map[string]string) bool {
	return c.catalogFilter.Keep(kind, names[catalogString(m["category_id"])], catalogString(m["name"]), catalogFlag(m["is_adult"]))
}

// channelHarvest collects what the channel-name index is built from, item by
// item, so it never needs the whole list at once.
type channelHarvest struct{ names, epgIDs, categories map[string]string }

func newChannelHarvest() *channelHarvest {
	return &channelHarvest{names: map[string]string{}, epgIDs: map[string]string{}, categories: map[string]string{}}
}

func (h *channelHarvest) add(m map[string]interface{}) {
	id := normalizeStreamID(fmt.Sprintf("%v", m["stream_id"]))
	name := strings.TrimSpace(fmt.Sprintf("%v", m["name"]))
	if id != "" && name != "" {
		h.names[id] = name
	}
	if epgID, _ := m["epg_channel_id"].(string); strings.TrimSpace(epgID) != "" {
		h.epgIDs[id] = strings.TrimSpace(epgID)
	}
	if category := resolveCategoryName(m); category != "" {
		h.categories[id] = category
	}
}

// applyChannelHarvest refreshes the channel-name index and returns the number
// of channel names indexed.
func (c *Config) applyChannelHarvest(h *channelHarvest) int {
	c.updateAPIChannelIndex(h.names, h.epgIDs, h.categories)
	return len(h.names)
}

// catchupFlagged returns a shallow copy of m advertising catchup, and records
// the provider's own catchup flag for it in upstream. The cached entry is
// shared with other goroutines, so it is never flagged in place.
func (c *Config) catchupFlagged(m map[string]interface{}, upstream map[string]bool) map[string]interface{} {
	out := make(map[string]interface{}, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	streamID := fmt.Sprintf("%v", out["stream_id"])
	if v, exists := out["tv_archive"]; exists {
		switch vv := v.(type) {
		case json.Number:
			n, _ := vv.Int64()
			upstream[streamID] = n == 1
		case float64:
			upstream[streamID] = vv == 1
		}
	}
	out["tv_archive"] = 1
	out["tv_archive_duration"] = c.catchupManager.AdvertisedHours()
	return out
}

// inCategory reports whether an entry is in categoryID, like the provider's
// own category_id filter: by its category_id, or by its category_ids list when
// a panel files it under several.
func inCategory(m map[string]interface{}, categoryID string) bool {
	if catalogString(m["category_id"]) == categoryID {
		return true
	}
	ids, _ := m["category_ids"].([]interface{})
	for _, v := range ids {
		if catalogString(v) == categoryID {
			return true
		}
	}
	return false
}
