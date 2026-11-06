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
	"strings"

	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	catalogStorageMemory = "memory"
	catalogStorageDisk   = "disk"
)

// parseCatalogStorage normalises the CATALOG_STORAGE setting; anything but
// disk falls back to memory, with a warning when it was not a known value.
func parseCatalogStorage(s string) string {
	switch v := strings.ToLower(strings.TrimSpace(s)); v {
	case catalogStorageDisk, catalogStorageMemory:
		return v
	case "":
	default:
		utils.WarnLog("unknown CATALOG_STORAGE %q: using %s", s, catalogStorageMemory)
	}
	return catalogStorageMemory
}

func (c *Config) diskCatalogs() bool { return c.CatalogStorage == catalogStorageDisk }
