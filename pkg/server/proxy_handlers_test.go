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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestStreamProxiesUpstreamBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("segment-bytes"))
	}))
	defer upstream.Close()

	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	upstreamURL, err := url.Parse(upstream.URL + "/live/channel1/1.ts")
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/live/channel1/1.ts", nil)

	c.stream(ctx, upstreamURL)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body, err := io.ReadAll(w.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(body) != "segment-bytes" {
		t.Errorf("body = %q, want %q", string(body), "segment-bytes")
	}
	if got := w.Header().Get("Content-Type"); got != "video/mp2t" {
		t.Errorf("Content-Type = %q, want %q", got, "video/mp2t")
	}
}

func TestM3U8ReverseProxyRewritesSegmentURIs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
	}))
	defer upstream.Close()

	track := &m3u.Track{URI: upstream.URL + "/playlist.m3u8"}
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
			AdvertisedPort: 8080,
			XtreamUser:     "upstreamuser",
			XtreamPassword: "upstreampass",
		},
		track: track,
	}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/anti/upstreamuser/upstreampass/0/playlist.m3u8", nil)
	ctx.Params = gin.Params{{Key: "id", Value: "playlist.m3u8"}}

	c.m3u8ReverseProxy(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	wantSegment := "http://proxy.example.com:8080/img?url=" + url.QueryEscape(upstream.URL+"/segment1.ts")
	if !strings.Contains(w.Body.String(), wantSegment) {
		t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
	}
}
