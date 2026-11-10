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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestParseCatalogStorage(t *testing.T) {
	for in, want := range map[string]string{"": "memory", "memory": "memory", " Disk ": "disk", "ssd": "memory"} {
		if got := parseCatalogStorage(in); got != want {
			t.Errorf("parseCatalogStorage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMemoryCatalogRendersOnce(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	store := &memoryCatalog{data: []interface{}{map[string]interface{}{"stream_id": "1", "name": "A"}}}
	serve := func() string {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
		if err := store.serveWhole(ctx, c, "get_live_streams"); err != nil {
			t.Fatal(err)
		}
		return w.Body.String()
	}
	first := serve()
	// The memo is what the second call must serve, not a fresh render.
	store.renders[0].body = []byte("memo")
	if second := serve(); second != "memo" || first == second {
		t.Fatalf("second serve re-rendered: first %q, second %q", first, second)
	}
}

// forEachCatalogStorage runs run once per CATALOG_STORAGE value, each on an
// empty catalog cache.
func forEachCatalogStorage(t *testing.T, run func(t *testing.T, storage string)) {
	for _, storage := range []string{catalogStorageMemory, catalogStorageDisk} {
		t.Run(storage, func(t *testing.T) {
			invalidateCatalogCache()
			run(t, storage)
		})
	}
}

func TestApplyCatalogMemoryLimit(t *testing.T) {
	prev := debug.SetMemoryLimit(-1)
	t.Cleanup(func() { debug.SetMemoryLimit(prev) })

	t.Setenv("GOMEMLIMIT", "")
	applyCatalogMemoryLimit("memory")
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("memory mode changed the limit to %d", got)
	}
	t.Setenv("GOMEMLIMIT", "200MiB")
	applyCatalogMemoryLimit("disk")
	if got := debug.SetMemoryLimit(-1); got != prev {
		t.Fatalf("disk mode overrode GOMEMLIMIT: limit %d", got)
	}
	t.Setenv("GOMEMLIMIT", "")
	applyCatalogMemoryLimit("disk")
	if got := debug.SetMemoryLimit(-1); got != 90<<20 {
		t.Fatalf("disk mode limit = %d, want %d", got, 90<<20)
	}
}

func TestCatalogRunDirRemovesOnlyStaleSiblings(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	catalogRunDirMu.Lock()
	saved := catalogRunDirPath
	catalogRunDirPath = ""
	catalogRunDirMu.Unlock()
	t.Cleanup(func() {
		catalogRunDirMu.Lock()
		catalogRunDirPath = saved
		catalogRunDirMu.Unlock()
	})

	base := filepath.Join(os.Getenv("CACHE_FOLDER"), "catalog")
	for name, age := range map[string]time.Duration{"run-old": 25 * time.Hour, "run-new": time.Hour, "keep-old": 25 * time.Hour} {
		d := filepath.Join(base, name)
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-age)
		if err := os.Chtimes(d, at, at); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := catalogRunDir()
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := catalogRunDir(); again != dir {
		t.Fatalf("second call made another run dir: %s, then %s", dir, again)
	}
	for name, want := range map[string]bool{"run-old": false, "run-new": true, "keep-old": true, filepath.Base(dir): true} {
		if _, err := os.Stat(filepath.Join(base, name)); (err == nil) != want {
			t.Errorf("%s exists = %v, want %v", name, err == nil, want)
		}
	}
	if m, _ := filepath.Match(filepath.Join(base, "run-*"), dir); !m {
		t.Fatalf("run dir %s is not a run-* directory under %s", dir, base)
	}
}

// A run dir removed behind our back (cache folder cleared, sibling sweep) is
// recreated instead of failing every later disk fetch.
func TestCatalogRunDirIsRecreated(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	catalogRunDirMu.Lock()
	saved := catalogRunDirPath
	catalogRunDirPath = ""
	catalogRunDirMu.Unlock()
	t.Cleanup(func() {
		catalogRunDirMu.Lock()
		catalogRunDirPath = saved
		catalogRunDirMu.Unlock()
	})
	dir, err := catalogRunDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	again, err := catalogRunDir()
	if err != nil || again != dir {
		t.Fatalf("catalogRunDir = %q, %v; want %q", again, err, dir)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("run dir not recreated: %v", err)
	}
}
