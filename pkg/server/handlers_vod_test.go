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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
)

// TestDefaultVODExt pins the per-type defaults used when no extension can be
// resolved synchronously.
func TestDefaultVODExt(t *testing.T) {
	if got := defaultVODExt("movie"); got != ".mp4" {
		t.Fatalf("movie default = %q, want .mp4", got)
	}
	if got := defaultVODExt("series"); got != ".mkv" {
		t.Fatalf("series default = %q, want .mkv", got)
	}
}

// TestVODFallbackTitle pins the placeholder-title precedence: series metadata
// first, then the caller-supplied title, then a constant.
func TestVODFallbackTitle(t *testing.T) {
	if got := vodFallbackTitle("series", "Ep title", "My Series", 2, 4); got != "My Series — S02E04" {
		t.Fatalf("series fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "  Some Movie  ", "", 0, 0); got != "Some Movie" {
		t.Fatalf("movie fallback = %q", got)
	}
	if got := vodFallbackTitle("movie", "", "", 0, 0); got != "Unknown title" {
		t.Fatalf("empty fallback = %q", got)
	}
	// A series without episode context falls through to the plain title.
	if got := vodFallbackTitle("series", "Ep title", "My Series", 0, 0); got != "Ep title" {
		t.Fatalf("series no-context fallback = %q", got)
	}
}

// vodTestDB connects to the Postgres named by the DB_* variables, skipping
// when none is reachable (CI without a database).
func vodTestDB(t *testing.T) *database.DBManager {
	t.Helper()
	db, err := database.NewDBManager("")
	if err != nil {
		t.Skipf("no test database: %v", err)
	}
	return db
}

func TestCachedVODFile(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: vodTestDB(t)}
	dir := t.TempDir()
	file := filepath.Join(dir, "a.mp4")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	put := func(id, p string) {
		t.Helper()
		e := &types.VODCacheEntry{StreamID: id, Type: "movie", FilePath: p, Status: "ready", ExpiresAt: time.Now().Add(time.Hour)}
		if err := c.db.UpsertVODCache(e); err != nil {
			t.Fatal(err)
		}
	}
	put("c1-ok", file)
	put("c1-gone", filepath.Join(dir, "gone.mp4"))
	put("c1-dir", dir)

	if p, ok := c.cachedVODFile("c1-ok"); !ok || p != file {
		t.Fatalf("existing file = %q,%v", p, ok)
	}
	if p, ok := c.cachedVODFile("c1-gone"); ok || p != "" {
		t.Fatalf("missing file = %q,%v", p, ok)
	}
	if p, ok := c.cachedVODFile("c1-dir"); ok || p != "" {
		t.Fatalf("directory = %q,%v", p, ok)
	}
	if p, ok := c.cachedVODFile("c1-norow"); ok || p != "" {
		t.Fatalf("no row = %q,%v", p, ok)
	}
}

// A ready row whose file is gone used to answer an error from the download
// link; it must fall back to proxying the provider.
func TestTemporaryLinkFallsBackWhenCachedFileMissing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("from-provider"))
	}))
	t.Cleanup(up.Close)

	db := vodTestDB(t)
	e := &types.VODCacheEntry{StreamID: "c1-link", Type: "movie", FilePath: filepath.Join(t.TempDir(), "gone.mp4"), Status: "ready", ExpiresAt: time.Now().Add(time.Hour)}
	if err := db.UpsertVODCache(e); err != nil {
		t.Fatal(err)
	}
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: db, sessionManager: session.NewSessionManager(nil)}
	t.Cleanup(c.sessionManager.Stop)
	token, _ := c.sessionManager.GenerateTemporaryLink("u", "c1-link", "T", up.URL+"/movie/u/p/c1-link.mp4")

	r := gin.New()
	r.GET("/download/:token", c.handleTemporaryLink)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest("GET", "/download/"+token, nil))
	if w.Code != http.StatusOK || hits.Load() == 0 {
		t.Fatalf("status=%d provider hits=%d body=%q; want fallback to provider", w.Code, hits.Load(), w.Body.String())
	}
}
