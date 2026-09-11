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
	"net/url"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

// TestImgRouteRequiresRecentAuth exercises the actual registered route +
// full middleware chain (via c.routes, the same call production uses), not
// the handlers directly — the /img route's earlier requirement of
// username/password credentials that generated URLs never carried went
// undetected because every prior test called c.assetProxy(ctx) directly,
// bypassing the middleware. This also verifies the current gate:
// requireRecentAuth, which trusts a client IP only after it has
// authenticated successfully against another endpoint.
func TestImgRouteRequiresRecentAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-png-bytes"))
	}))
	defer upstream.Close()

	m3uFile, err := os.CreateTemp(t.TempDir(), "playlist-*.m3u")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	if _, err := m3uFile.WriteString("#EXTM3U\n"); err != nil {
		t.Fatalf("WriteString: %v", err)
	}
	_ = m3uFile.Close()

	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			User:        "testuser",
			Password:    "testpass",
			M3UFileName: "playlist.m3u",
		},
		playlist:         &m3u.Playlist{},
		proxyfiedM3UPath: m3uFile.Name(),
	}

	router := gin.New()
	c.routes(router.Group("/"))

	imgURL := "/img?url=" + url.QueryEscape(upstream.URL+"/logo.png")

	t.Run("rejects a client that never authenticated", func(t *testing.T) {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, imgURL, nil)
		req.RemoteAddr = "203.0.113.1:1234"
		router.ServeHTTP(w, req)

		if w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403; body=%s", w.Code, w.Body.String())
		}
	})

	t.Run("allows a client after it authenticates against another endpoint", func(t *testing.T) {
		clientAddr := "203.0.113.2:1234"

		authReq := httptest.NewRequest(http.MethodGet, "/playlist.m3u?username=testuser&password=testpass", nil)
		authReq.RemoteAddr = clientAddr
		authW := httptest.NewRecorder()
		router.ServeHTTP(authW, authReq)
		if authW.Code != http.StatusOK {
			t.Fatalf("authenticating request: status = %d, want 200; body=%s", authW.Code, authW.Body.String())
		}

		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, imgURL, nil)
		req.RemoteAddr = clientAddr
		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
		}
		if w.Body.String() != "fake-png-bytes" {
			t.Errorf("body = %q, want %q", w.Body.String(), "fake-png-bytes")
		}
	})
}
