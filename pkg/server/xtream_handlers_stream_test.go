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
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func resetHlsRedirectCache() {
	hlsChannelsRedirectURLLock.Lock()
	hlsChannelsRedirectURL = map[string]url.URL{}
	hlsChannelsRedirectURLLock.Unlock()
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
