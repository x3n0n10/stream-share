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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func resetHlsRedirectCache() {
	hlsChannelsRedirectURLLock.Lock()
	hlsChannelsRedirectURL = map[string]url.URL{}
	hlsChannelsRedirectURLLock.Unlock()
}

// TestXtreamApiGetRewritesHostPerRequest mirrors
// TestXtreamGetRewritesHostPerRequest in xtream_handlers_api_test.go: the
// same cached file (populated via cacheXtreamM3u/marshallInto, containing
// relative track URIs and tvg-logo values) is served through xtreamApiGet
// to two different requesting hosts, and each response must carry its own
// host prefix with no leakage from the other.
func TestXtreamApiGetRewritesHostPerRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dir := t.TempDir()
	cachedPath := filepath.Join(dir, "cached.m3u")
	cachedBody := "#EXTM3U\n" +
		`#EXTINF:-1 tvg-logo="/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png", Channel One` + "\n" +
		"/anti/user/pass/0/stream1\n"
	if err := os.WriteFile(cachedPath, []byte(cachedBody), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := &Config{ProxyConfig: &config.ProxyConfig{
		M3UFileName:        "playlist.m3u",
		M3UCacheExpiration: 24,
	}}

	xtreamM3uCacheLock.Lock()
	xtreamM3uCache["apiget"] = cacheMeta{cachedPath, time.Now()}
	xtreamM3uCacheLock.Unlock()
	defer func() {
		xtreamM3uCacheLock.Lock()
		delete(xtreamM3uCache, "apiget")
		xtreamM3uCacheLock.Unlock()
	}()

	call := func(host string) string {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/xtream/apiget", nil)
		ctx.Request.Host = host
		c.xtreamApiGet(ctx)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		return w.Body.String()
	}

	bodyA := call("host-a.example.com")
	bodyB := call("host-b.example.com:9090")

	if !strings.Contains(bodyA, "http://host-a.example.com/anti/user/pass/0/stream1") {
		t.Errorf("bodyA %q does not use host-a", bodyA)
	}
	if !strings.Contains(bodyB, "http://host-b.example.com:9090/anti/user/pass/0/stream1") {
		t.Errorf("bodyB %q does not use host-b", bodyB)
	}
	if strings.Contains(bodyA, "host-b") || strings.Contains(bodyB, "host-a") {
		t.Errorf("the same cached file leaked the other request's host: bodyA=%q bodyB=%q", bodyA, bodyB)
	}
}

func TestXtreamHlsStreamRewritesManifestHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetHlsRedirectCache()
	defer resetHlsRedirectCache()

	segmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("/hlsr/tok1/upstreamuser/upstreampass/chan1/hash1/seg1.ts\n"))
	}))
	defer segmentServer.Close()
	segURL, err := url.Parse(segmentServer.URL)
	if err != nil {
		t.Fatalf("url.Parse segmentServer: %v", err)
	}

	firstHop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, segmentServer.URL+"/hls/tok1/chan1_1", http.StatusFound)
	}))
	defer firstHop.Close()
	firstHopURL, err := url.Parse(firstHop.URL)
	if err != nil {
		t.Fatalf("url.Parse firstHop: %v", err)
	}

	hlsChannelsRedirectURLLock.Lock()
	hlsChannelsRedirectURL["chan1.m3u8"] = *firstHopURL
	hlsChannelsRedirectURLLock.Unlock()

	c := &Config{ProxyConfig: &config.ProxyConfig{
		HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
		AdvertisedPort: 8080,
		XtreamUser:     "upstreamuser",
		XtreamPassword: "upstreampass",
		User:           "localuser",
		Password:       "localpass",
	}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/hls/tok1/chan1_1", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	ctx.Params = gin.Params{{Key: "token", Value: "tok1"}, {Key: "chunk", Value: "chan1_1"}, {Key: "id", Value: "chan1"}}

	c.xtreamHlsStream(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	wantSegment := "http://proxy.example.com:8080/img?url=" + url.QueryEscape(segURL.Scheme+"://"+segURL.Host+"/hlsr/tok1/upstreamuser/upstreampass/chan1/hash1/seg1.ts")
	if !strings.Contains(w.Body.String(), wantSegment) {
		t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
	}
	if strings.Contains(w.Body.String(), "/localuser/localpass/") {
		t.Errorf("body %q still contains a credential-swapped path instead of a proxied URL", w.Body.String())
	}
}

func TestHlsXtreamStreamRewritesManifestHost(t *testing.T) {
	gin.SetMode(gin.TestMode)
	resetHlsRedirectCache()
	defer resetHlsRedirectCache()

	segmentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("segment1.ts\n"))
	}))
	defer segmentServer.Close()
	segURL, err := url.Parse(segmentServer.URL)
	if err != nil {
		t.Fatalf("url.Parse segmentServer: %v", err)
	}

	firstHop := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, segmentServer.URL+"/media.m3u8", http.StatusFound)
	}))
	defer firstHop.Close()
	firstHopURL, err := url.Parse(firstHop.URL)
	if err != nil {
		t.Fatalf("url.Parse firstHop: %v", err)
	}

	c := &Config{ProxyConfig: &config.ProxyConfig{
		HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
		AdvertisedPort: 8080,
	}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/hlsr/tok1/u/p/chan1/hash1/seg1.ts", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	ctx.Params = gin.Params{{Key: "id", Value: ""}}

	c.hlsXtreamStream(ctx, firstHopURL)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	wantSegment := "http://proxy.example.com:8080/img?url=" + url.QueryEscape(segURL.Scheme+"://"+segURL.Host+"/segment1.ts")
	if !strings.Contains(w.Body.String(), wantSegment) {
		t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
	}
}
