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
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"

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

// applyCatalogMemoryLimit gives disk mode a soft heap limit so the GC returns
// the memory a catalogue download or render briefly used; an explicit
// GOMEMLIMIT wins.
func applyCatalogMemoryLimit(storage string) {
	if storage == catalogStorageDisk && os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(90 << 20)
		utils.InfoLog("Catalogue storage: disk; GOMEMLIMIT set to 90 MiB (override with GOMEMLIMIT)")
	}
}

var (
	catalogRunDirMu   sync.Mutex
	catalogRunDirPath string
)

// catalogRunDir returns this process's directory for catalogue files, made on
// first use. Directories of earlier runs left by a crash are removed once a
// day old; a younger one may belong to another instance sharing the folder.
// A failure is returned and retried on the next call.
func catalogRunDir() (string, error) {
	catalogRunDirMu.Lock()
	defer catalogRunDirMu.Unlock()
	if catalogRunDirPath != "" {
		return catalogRunDirPath, nil
	}
	base := utils.CatalogCacheDir()
	if err := os.MkdirAll(base, 0o700); err != nil {
		return "", err
	}
	old, _ := filepath.Glob(filepath.Join(base, "run-*"))
	for _, d := range old {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() && time.Since(fi.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(d)
		}
	}
	dir, err := os.MkdirTemp(base, "run-")
	if err != nil {
		return "", err
	}
	catalogRunDirPath = dir
	return dir, nil
}
