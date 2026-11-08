/*
 * stream-share is a project to efficiently share the use of an IPTV service.
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
	"net/url"
	"strings"
	"testing"

	"github.com/jamesnetherton/m3u"
)

// TestTrackManifestLinksFetchThroughImg guards the trackProxy path: the
// per-request track Config must sign with the server's key, or /img (main
// Config) rejects every HLS child link of an M3U-mode track.
func TestTrackManifestLinksFetchThroughImg(t *testing.T) {
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\nseg.ts\n"))
			return
		}
		_, _ = w.Write([]byte("segment-bytes"))
	}))
	defer up.Close()

	c := testImageProxyConfig()
	c.XtreamUser, c.XtreamPassword, c.M3UFileName = "u", "p", "playlist.m3u"
	c.endpointAntiColision = "anti"
	c.playlist = &m3u.Playlist{Tracks: []m3u.Track{{URI: up.URL + "/live.m3u8"}}}
	router := newImgRouter(c)

	w := getImg(router, "/anti/u/p/0/live.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: status = %d, body=%s", w.Code, w.Body.String())
	}
	var link string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.Contains(line, "/img?url=") {
			link = line
		}
	}
	if link == "" {
		t.Fatalf("no /img line in %q", w.Body.String())
	}
	u, _ := url.Parse(link)
	if w := getImg(router, u.RequestURI()); w.Code != http.StatusOK || w.Body.String() != "segment-bytes" {
		t.Fatalf("segment: status = %d, body = %q", w.Code, w.Body.String())
	}
}
