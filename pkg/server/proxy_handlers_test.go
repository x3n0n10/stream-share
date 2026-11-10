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
		track:    track,
		imageKey: testImageKey,
	}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/anti/upstreamuser/upstreampass/0/playlist.m3u8", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	ctx.Params = gin.Params{{Key: "id", Value: "playlist.m3u8"}}

	c.m3u8ReverseProxy(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	wantSegment := "http://proxy.example.com:8080" + c.proxyImagePath(upstream.URL+"/segment1.ts")
	if !strings.Contains(w.Body.String(), wantSegment) {
		t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
	}
}

func TestM3U8ReverseProxyRewritesAgainstPostRedirectURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
	}))
	defer finalServer.Close()

	redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, finalServer.URL+"/playlist.m3u8", http.StatusFound)
	}))
	defer redirectServer.Close()

	track := &m3u.Track{URI: redirectServer.URL + "/playlist.m3u8"}
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
			AdvertisedPort: 8080,
			XtreamUser:     "upstreamuser",
			XtreamPassword: "upstreampass",
		},
		track:    track,
		imageKey: testImageKey,
	}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/anti/upstreamuser/upstreampass/0/playlist.m3u8", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	ctx.Params = gin.Params{{Key: "id", Value: "playlist.m3u8"}}

	c.m3u8ReverseProxy(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	wantSegment := "http://proxy.example.com:8080" + c.proxyImagePath(finalServer.URL+"/segment1.ts")
	if !strings.Contains(w.Body.String(), wantSegment) {
		t.Errorf("body %q does not contain segment URI resolved against the final URL %q", w.Body.String(), wantSegment)
	}
	wrongSegment := url.QueryEscape(redirectServer.URL + "/segment1.ts")
	if strings.Contains(w.Body.String(), wrongSegment) {
		t.Errorf("body %q resolved the segment against the pre-redirect URL instead of the final one", w.Body.String())
	}
}

func hostileClientRequest() *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/x", nil)
	for k, v := range map[string]string{"Cookie": "a=b", "Authorization": "Basic x", "X-Forwarded-For": "1.2.3.4", "Range": "bytes=100-", "If-Range": `"e"`, "User-Agent": "Player/1"} {
		r.Header.Set(k, v)
	}
	return r
}

func TestUpstreamRequestForwardsOnlyAllowlistedHeaders(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = hostileClientRequest()
	u, _ := url.Parse("http://provider.example/live/u/p/1.m3u8")
	req, err := (&Config{}).buildUpstreamRequest(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Range") != "bytes=100-" || req.Header.Get("If-Range") != `"e"` {
		t.Fatalf("Range/If-Range not forwarded: %v", req.Header)
	}
	if req.Header.Get("User-Agent") == "Player/1" || req.Header.Get("User-Agent") == "" {
		t.Fatalf("User-Agent = %q, want IPTV UA", req.Header.Get("User-Agent"))
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Forwarded-For"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s leaked upstream", h)
		}
	}
}

func TestVODUpstreamRequestUsesAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	u, _ := url.Parse("http://provider.example/movie/u/p/1.mp4")
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = hostileClientRequest()
	req, err := (&Config{}).buildUpstreamRequest(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	if req.Header.Get("Range") != "bytes=100-" || req.Header.Get("If-Range") != `"e"` {
		t.Fatalf("Range/If-Range not forwarded: %v", req.Header)
	}
	for _, h := range []string{"Cookie", "Authorization", "X-Forwarded-For"} {
		if req.Header.Get(h) != "" {
			t.Errorf("%s leaked upstream", h)
		}
	}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/x", nil)
	req, _ = (&Config{}).buildUpstreamRequest(ctx, u)
	if req.Header.Get("Accept") != "*/*" || req.Header.Get("Accept-Language") == "" || req.Header.Get("Range") != "" ||
		req.Header.Get("Accept-Encoding") != "identity" || req.Header.Get("Connection") != "keep-alive" {
		t.Fatalf("defaults wrong: %v", req.Header)
	}
}

func TestHLSUpstreamUsesAllowlist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var got []http.Header
	var up *httptest.Server
	up = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Clone())
		if len(got) == 1 {
			http.Redirect(w, r, up.URL+"/chan1/index.m3u8", http.StatusFound)
		}
	}))
	t.Cleanup(up.Close)
	upURL, perr := url.Parse(up.URL)
	if perr != nil {
		t.Fatal(perr)
	}
	hlsChannelsRedirectURLLock.Lock()
	hlsChannelsRedirectURL["chan1.m3u8"] = *upURL
	hlsChannelsRedirectURLLock.Unlock()
	t.Cleanup(resetHlsRedirectCache)

	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = hostileClientRequest()
	ctx.Params = gin.Params{{Key: "token", Value: "tok"}, {Key: "chunk", Value: "chan1_1.ts"}, {Key: "id", Value: "chan1"}}
	(&Config{}).xtreamHlsStream(ctx)

	if len(got) != 2 {
		t.Fatalf("provider saw %d requests, want 2 (initial + post-302)", len(got))
	}
	for i, h := range got {
		if h.Get("Cookie") != "" || h.Get("Authorization") != "" || h.Get("X-Forwarded-For") != "" || h.Get("User-Agent") == "Player/1" || h.Get("User-Agent") == "" {
			t.Fatalf("request %d leaked client headers: %v", i, h)
		}
	}
}
