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
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

// TestImgRouteRequiresNoAuth exercises the actual registered route + full
// middleware chain (via c.routes, the same call production uses), not the
// handler directly — the /img route's earlier requirement of
// username/password credentials that generated URLs never carried went
// undetected because every prior test called c.assetProxy(ctx) directly,
// bypassing the middleware. This test would have caught it.
func TestImgRouteRequiresNoAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("fake-png-bytes"))
	}))
	defer upstream.Close()

	c := &Config{
		ProxyConfig: &config.ProxyConfig{},
		playlist:    &m3u.Playlist{},
	}

	router := gin.New()
	c.routes(router.Group("/"))

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/img?url="+url.QueryEscape(upstream.URL+"/logo.png"), nil)
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (no auth required, no username/password supplied); body=%s", w.Code, w.Body.String())
	}
	if w.Body.String() != "fake-png-bytes" {
		t.Errorf("body = %q, want %q", w.Body.String(), "fake-png-bytes")
	}
}
