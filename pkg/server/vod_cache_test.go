/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

// A player request and a Discord download of the same movie must land on one
// cache file, whatever the probe setting.
func TestVODResolverSameFileForPlayerAndDownload(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	prov := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, ".mp4") {
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer prov.Close()
	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: prov.URL, VODExtProbeEnabled: true}}

	// (a) explicit extension: identical in both modes.
	upA, destA, _ := c.resolveVODTarget("movie", "", "10.mkv", false)
	upB, destB, _ := c.resolveVODTarget("movie", "", "10.mkv", true)
	if upA != upB || destA != destB || !strings.HasSuffix(destA, "10.mkv") {
		t.Fatalf("explicit ext differs: %s %s / %s %s", upA, destA, upB, destB)
	}

	// (b) probe finds .mp4; once the catalogue knows it, the request path agrees.
	_, probed, _ := c.resolveVODTarget("movie", ".mkv", "20", true)
	if !strings.HasSuffix(probed, "20.mp4") {
		t.Fatalf("probe picked %s, want .mp4", probed)
	}
	vodExtCache.Store("movie\x0020", extEntry{ext: ".mp4"})
	t.Cleanup(func() { vodExtCache.Delete("movie\x0020") })
	if _, d, _ := c.resolveVODTarget("movie", ".mkv", "20", false); d != probed {
		t.Fatalf("request path %s != download %s", d, probed)
	}

	// (c) nothing known, probing off: per-type default in both modes.
	c.VODExtProbeEnabled = false
	for _, probe := range []bool{false, true} {
		if _, d, _ := c.resolveVODTarget("series", "", "30", probe); !strings.HasSuffix(d, "30.mkv") {
			t.Fatalf("series default probe=%v: %s", probe, d)
		}
		if _, d, _ := c.resolveVODTarget("movie", "", "31", probe); !strings.HasSuffix(d, "31.mp4") {
			t.Fatalf("movie default probe=%v: %s", probe, d)
		}
	}
}
