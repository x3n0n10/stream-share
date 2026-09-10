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

func testImageProxyConfig() *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{
		HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
		AdvertisedPort: 8080,
	}}
}

func TestProxyImageURL(t *testing.T) {
	c := testImageProxyConfig()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty input passes through unchanged", "", ""},
		{"bare relative input passes through unchanged", "logo.png", "logo.png"},
		{
			"absolute url gets wrapped and query-escaped",
			"http://upstream.example.com/logo.png?x=1&y=2",
			"http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/logo.png?x=1&y=2"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.proxyImageURL(tc.in); got != tc.want {
				t.Errorf("proxyImageURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteImageFields(t *testing.T) {
	c := testImageProxyConfig()
	input := map[string]interface{}{
		"movie_image":   "http://upstream.example.com/cover.jpg",
		"direct_source": "http://upstream.example.com/stream.mp4",
		"backdrop_path": []interface{}{
			"http://upstream.example.com/bd1.jpg",
			"http://upstream.example.com/bd2.jpg",
		},
		"name": "Show One",
		"episodes": map[string]interface{}{
			"1": []interface{}{
				map[string]interface{}{
					"id": "101",
					"info": map[string]interface{}{
						"movie_image": "http://upstream.example.com/ep101.jpg",
					},
				},
			},
		},
	}

	got, ok := c.rewriteImageFields(input).(map[string]interface{})
	if !ok {
		t.Fatalf("rewriteImageFields did not return a map[string]interface{}")
	}

	wantMovieImage := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/cover.jpg")
	if got["movie_image"] != wantMovieImage {
		t.Errorf("movie_image = %v, want %v", got["movie_image"], wantMovieImage)
	}
	if _, exists := got["direct_source"]; exists {
		t.Errorf("direct_source should be stripped, still present: %v", got["direct_source"])
	}
	bd, ok := got["backdrop_path"].([]interface{})
	if !ok || len(bd) != 2 {
		t.Fatalf("backdrop_path missing or wrong shape: %v", got["backdrop_path"])
	}
	wantBd0 := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/bd1.jpg")
	if bd[0] != wantBd0 {
		t.Errorf("backdrop_path[0] = %v, want %v", bd[0], wantBd0)
	}
	if got["name"] != "Show One" {
		t.Errorf("unrelated field name changed: %v", got["name"])
	}

	episodes, ok := got["episodes"].(map[string]interface{})
	if !ok {
		t.Fatalf("episodes missing or wrong shape: %v", got["episodes"])
	}
	season1, ok := episodes["1"].([]interface{})
	if !ok || len(season1) != 1 {
		t.Fatalf("episodes[1] missing or wrong shape: %v", episodes["1"])
	}
	ep, ok := season1[0].(map[string]interface{})
	if !ok {
		t.Fatalf("episode 0 wrong shape: %v", season1[0])
	}
	epInfo, ok := ep["info"].(map[string]interface{})
	if !ok {
		t.Fatalf("episode info wrong shape: %v", ep["info"])
	}
	wantEpImage := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/ep101.jpg")
	if epInfo["movie_image"] != wantEpImage {
		t.Errorf("nested episode movie_image = %v, want %v", epInfo["movie_image"], wantEpImage)
	}
}

func TestRewriteM3U8(t *testing.T) {
	c := testImageProxyConfig()
	base, err := url.Parse("http://upstream.example.com/live/channel1/index.m3u8")
	if err != nil {
		t.Fatalf("url.Parse base: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "bare relative segment line resolved against base and proxied",
			in:   "segment1.ts",
			want: "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/live/channel1/segment1.ts"),
		},
		{
			name: "bare absolute segment line proxied as-is",
			in:   "http://cdn.example.com/seg2.ts",
			want: "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://cdn.example.com/seg2.ts"),
		},
		{
			name: "comment line with no URI attribute passes through unchanged",
			in:   "#EXTM3U",
			want: "#EXTM3U",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(c.rewriteM3U8(base, []byte(tc.in)))
			if out != tc.want {
				t.Errorf("rewriteM3U8(%q) = %q, want %q", tc.in, out, tc.want)
			}
		})
	}

	t.Run("EXT-X-KEY URI attribute rewritten in place, rest of tag untouched", func(t *testing.T) {
		in := `#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000001`
		out := string(c.rewriteM3U8(base, []byte(in)))
		wantURI := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/live/channel1/key.bin")
		if !strings.Contains(out, wantURI) {
			t.Errorf("output %q does not contain rewritten key URI %q", out, wantURI)
		}
		if !strings.HasPrefix(out, `#EXT-X-KEY:METHOD=AES-128,URI="`) {
			t.Errorf("output %q lost the METHOD attribute prefix", out)
		}
		if !strings.HasSuffix(out, `",IV=0x00000000000000000000000000000001`) {
			t.Errorf("output %q lost the IV attribute suffix", out)
		}
	})
}

func TestAssetProxy(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("rejects a non-http(s) scheme", func(t *testing.T) {
		c := testImageProxyConfig()
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/img?url="+url.QueryEscape("file:///etc/passwd"), nil)

		c.assetProxy(ctx)

		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("streams a plain image response unmodified", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "image/png")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("fake-png-bytes"))
		}))
		defer upstream.Close()

		c := testImageProxyConfig()
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/img?url="+url.QueryEscape(upstream.URL+"/logo.png"), nil)

		c.assetProxy(ctx)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if w.Body.String() != "fake-png-bytes" {
			t.Errorf("body = %q, want %q", w.Body.String(), "fake-png-bytes")
		}
	})

	t.Run("rewrites nested segment URIs in an m3u8 manifest response", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
		}))
		defer upstream.Close()

		c := testImageProxyConfig()
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		targetURL := upstream.URL + "/hls/channel1/index.m3u8"
		ctx.Request = httptest.NewRequest(http.MethodGet, "/img?url="+url.QueryEscape(targetURL), nil)

		c.assetProxy(ctx)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		wantSegment := "http://proxy.example.com:8080/img?url=" + url.QueryEscape(upstream.URL+"/hls/channel1/segment1.ts")
		if !strings.Contains(w.Body.String(), wantSegment) {
			t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
		}
		if strings.Contains(w.Body.String(), upstream.URL+"/hls/channel1/segment1.ts\n") {
			t.Errorf("body %q still contains the raw upstream segment URI", w.Body.String())
		}
	})
}
