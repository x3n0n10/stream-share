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
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

var testImageKey = []byte("0123456789abcdef0123456789abcdef")

func testImageProxyConfig() *Config {
	return &Config{ProxyConfig: &config.ProxyConfig{
		HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
		AdvertisedPort: 8080,
	}, playlist: &m3u.Playlist{}, imageKey: testImageKey}
}

func testRequestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	return ctx
}

func TestProxyImageURL(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
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
			"http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/logo.png?x=1&y=2"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.proxyImageURL(ctx, tc.in); got != tc.want {
				t.Errorf("proxyImageURL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestProxyImagePath(t *testing.T) {
	c := testImageProxyConfig()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty input passes through unchanged", "", ""},
		{"bare relative input passes through unchanged", "logo.png", "logo.png"},
		{
			"absolute url gets wrapped, no host or scheme",
			"http://upstream.example.com/logo.png?x=1&y=2",
			c.proxyImagePath("http://upstream.example.com/logo.png?x=1&y=2"),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.proxyImagePath(tc.in); got != tc.want {
				t.Errorf("proxyImagePath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRewriteM3U8(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
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
			want: "http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/live/channel1/segment1.ts"),
		},
		{
			name: "bare absolute segment line proxied as-is",
			in:   "http://cdn.example.com/seg2.ts",
			want: "http://proxy.example.com:8080" + c.proxyImagePath("http://cdn.example.com/seg2.ts"),
		},
		{
			name: "comment line with no URI attribute passes through unchanged",
			in:   "#EXTM3U",
			want: "#EXTM3U",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := string(c.rewriteM3U8(ctx, base, []byte(tc.in)))
			if out != tc.want {
				t.Errorf("rewriteM3U8(%q) = %q, want %q", tc.in, out, tc.want)
			}
		})
	}

	t.Run("EXT-X-KEY URI attribute rewritten in place, rest of tag untouched", func(t *testing.T) {
		in := `#EXT-X-KEY:METHOD=AES-128,URI="key.bin",IV=0x00000000000000000000000000000001`
		out := string(c.rewriteM3U8(ctx, base, []byte(in)))
		wantURI := "http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/live/channel1/key.bin")
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
		ctx.Request = httptest.NewRequest(http.MethodGet, c.proxyImagePath("file:///etc/passwd"), nil)
		ctx.Request.Host = "proxy.example.com:8080"

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
		ctx.Request = httptest.NewRequest(http.MethodGet, c.proxyImagePath(upstream.URL+"/logo.png"), nil)
		ctx.Request.Host = "proxy.example.com:8080"

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
		ctx.Request = httptest.NewRequest(http.MethodGet, c.proxyImagePath(targetURL), nil)
		ctx.Request.Host = "proxy.example.com:8080"

		c.assetProxy(ctx)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		wantSegment := "http://proxy.example.com:8080" + c.proxyImagePath(upstream.URL+"/hls/channel1/segment1.ts")
		if !strings.Contains(w.Body.String(), wantSegment) {
			t.Errorf("body %q does not contain rewritten segment URI %q", w.Body.String(), wantSegment)
		}
		if strings.Contains(w.Body.String(), upstream.URL+"/hls/channel1/segment1.ts\n") {
			t.Errorf("body %q still contains the raw upstream segment URI", w.Body.String())
		}
	})

	t.Run("rewrites segment URIs against the post-redirect URL, not the requested one", func(t *testing.T) {
		finalServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("#EXTM3U\nsegment1.ts\n"))
		}))
		defer finalServer.Close()

		redirectServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, finalServer.URL+"/hls/channel1/index.m3u8", http.StatusFound)
		}))
		defer redirectServer.Close()

		c := testImageProxyConfig()
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		targetURL := redirectServer.URL + "/live/channel1.m3u8"
		ctx.Request = httptest.NewRequest(http.MethodGet, c.proxyImagePath(targetURL), nil)
		ctx.Request.Host = "proxy.example.com:8080"

		c.assetProxy(ctx)

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
		}
		wantSegment := "http://proxy.example.com:8080" + c.proxyImagePath(finalServer.URL+"/hls/channel1/segment1.ts")
		if !strings.Contains(w.Body.String(), wantSegment) {
			t.Errorf("body %q does not contain segment URI resolved against the final URL %q", w.Body.String(), wantSegment)
		}
		wrongSegment := redirectServer.URL + "/live/segment1.ts"
		if strings.Contains(w.Body.String(), url.QueryEscape(wrongSegment)) {
			t.Errorf("body %q resolved the segment against the pre-redirect URL instead of the final one", w.Body.String())
		}
	})
}

func TestRequestBaseURL(t *testing.T) {
	gin.SetMode(gin.TestMode)

	newCtx := func(host string, headers map[string]string) *gin.Context {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
		ctx.Request.Host = host
		for k, v := range headers {
			ctx.Request.Header.Set(k, v)
		}
		return ctx
	}

	t.Run("PublicBaseURL wins over everything else", func(t *testing.T) {
		c := &Config{ProxyConfig: &config.ProxyConfig{
			PublicBaseURL:       "https://public.example.com/",
			ReverseProxyEnabled: true,
		}}
		ctx := newCtx("ignored.example.com", map[string]string{
			"X-Forwarded-Host":  "also-ignored.example.com",
			"X-Forwarded-Proto": "https",
		})
		if got := c.requestBaseURL(ctx); got != "https://public.example.com" {
			t.Errorf("requestBaseURL = %q, want %q", got, "https://public.example.com")
		}
	})

	t.Run("reverse-proxy-enabled with forwarded headers wins over raw Host", func(t *testing.T) {
		c := &Config{ProxyConfig: &config.ProxyConfig{ReverseProxyEnabled: true}}
		ctx := newCtx("internal.example.com:8080", map[string]string{
			"X-Forwarded-Host":  "public.example.com",
			"X-Forwarded-Proto": "https",
		})
		if got := c.requestBaseURL(ctx); got != "https://public.example.com" {
			t.Errorf("requestBaseURL = %q, want %q", got, "https://public.example.com")
		}
	})

	t.Run("reverse-proxy-enabled but no forwarded headers falls back to raw Host", func(t *testing.T) {
		c := &Config{ProxyConfig: &config.ProxyConfig{ReverseProxyEnabled: true}}
		ctx := newCtx("direct.example.com:8080", nil)
		if got := c.requestBaseURL(ctx); got != "http://direct.example.com:8080" {
			t.Errorf("requestBaseURL = %q, want %q", got, "http://direct.example.com:8080")
		}
	})

	t.Run("reverse-proxy-enabled false ignores forwarded headers even if a client sends them", func(t *testing.T) {
		c := &Config{ProxyConfig: &config.ProxyConfig{ReverseProxyEnabled: false}}
		ctx := newCtx("direct.example.com:8080", map[string]string{
			"X-Forwarded-Host":  "spoofed.example.com",
			"X-Forwarded-Proto": "https",
		})
		if got := c.requestBaseURL(ctx); got != "http://direct.example.com:8080" {
			t.Errorf("requestBaseURL = %q, want %q", got, "http://direct.example.com:8080")
		}
	})

	t.Run("HTTPS flag selects protocol when no forwarded-proto header", func(t *testing.T) {
		c := &Config{ProxyConfig: &config.ProxyConfig{HTTPS: true}}
		ctx := newCtx("direct.example.com", nil)
		if got := c.requestBaseURL(ctx); got != "https://direct.example.com" {
			t.Errorf("requestBaseURL = %q, want %q", got, "https://direct.example.com")
		}
	})
}

func TestRewritePlaylistHosts(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()

	t.Run("track URI and tvg-logo both get the base prefixed", func(t *testing.T) {
		in := "#EXTM3U\n" +
			`#EXTINF:-1 tvg-id="ch1" tvg-logo="/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png", Channel One` + "\n" +
			"/anti/user/pass/0/stream1\n"

		out := rewriteHostsString(t, c, ctx, in)

		wantLogo := `tvg-logo="http://proxy.example.com:8080/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png"`
		if !strings.Contains(out, wantLogo) {
			t.Errorf("output %q does not contain prefixed tvg-logo %q", out, wantLogo)
		}
		wantTrack := "http://proxy.example.com:8080/anti/user/pass/0/stream1"
		if !strings.Contains(out, wantTrack) {
			t.Errorf("output %q does not contain prefixed track URI %q", out, wantTrack)
		}
		if !strings.Contains(out, "#EXTM3U") {
			t.Errorf("output %q lost the #EXTM3U header", out)
		}
	})

	t.Run("EXTINF line with no tvg-logo attribute is left alone apart from its track line", func(t *testing.T) {
		in := `#EXTINF:-1 tvg-id="ch2", Channel Two` + "\n" + "/anti/user/pass/1/stream2\n"
		out := rewriteHostsString(t, c, ctx, in)
		if !strings.HasPrefix(out, `#EXTINF:-1 tvg-id="ch2", Channel Two`) {
			t.Errorf("output %q changed the untouched EXTINF line", out)
		}
		if !strings.Contains(out, "http://proxy.example.com:8080/anti/user/pass/1/stream2") {
			t.Errorf("output %q does not contain prefixed track URI", out)
		}
	})

	t.Run("provider-absolute track and logo (streamed get.php) are mapped onto the proxy", func(t *testing.T) {
		in := `#EXTINF:-1 tvg-logo="http://provider.example.com/l.png", Ch` + "\n" +
			"http://provider.example.com:8080/live/localuser/localpass/12345.ts\n"
		out := rewriteHostsString(t, c, ctx, in)
		if !strings.Contains(out, `tvg-logo="http://proxy.example.com:8080`+c.proxyImagePath("http://provider.example.com/l.png")+`"`) {
			t.Errorf("logo not proxied: %q", out)
		}
		if !strings.Contains(out, "\nhttp://proxy.example.com:8080/live/localuser/localpass/12345.ts") {
			t.Errorf("track still points at the provider: %q", out)
		}
		if strings.Contains(out, "provider.example.com:8080") {
			t.Errorf("provider host leaked: %q", out)
		}
	})

	t.Run("a non-# line not starting with / is left alone", func(t *testing.T) {
		in := "not-a-path-or-comment"
		out := rewriteHostsString(t, c, ctx, in)
		if out != in {
			t.Errorf("output = %q, want unchanged %q", out, in)
		}
	})
}

// legacyRewritePlaylistHosts is the pre-streaming implementation, kept as the
// golden oracle for rewritePlaylistHostsTo.
func (c *Config) legacyRewritePlaylistHosts(ctx *gin.Context, body []byte) []byte {
	base := c.requestBaseURL(ctx)
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(trimmed, "#EXTINF") && strings.Contains(trimmed, "tvg-logo="):
			lines[i] = rewriteQuotedURI(trimmed, "tvg-logo=", func(raw string) string {
				if strings.HasPrefix(raw, "/") {
					return base + raw
				}
				if isHTTPURL(raw) {
					return base + c.proxyImagePath(raw)
				}
				return raw
			})
		case strings.HasPrefix(trimmed, "#"):
		case strings.HasPrefix(trimmed, "/"):
			lines[i] = base + trimmed
		case isHTTPURL(trimmed):
			if u, err := url.Parse(trimmed); err == nil {
				lines[i] = base + customEnd + u.EscapedPath()
			}
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

func TestRewritePlaylistHostsToMatchesBytes(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()

	base := []string{
		"#EXTM3U\n" +
			`#EXTINF:-1 tvg-id="ch1" tvg-logo="/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png", Channel One` + "\n" +
			"/anti/user/pass/0/stream1\n",
		`#EXTINF:-1 tvg-id="ch2", Channel Two` + "\n" + "/anti/user/pass/1/stream2\n",
		`#EXTINF:-1 tvg-logo="http://provider.example.com/l.png", Ch` + "\n" +
			"http://provider.example.com:8080/live/localuser/localpass/12345.ts\n",
		"not-a-path-or-comment",
		"",
		"\n\n",
	}
	long := `#EXTINF:-1 tvg-logo="/img?x=1" tvg-name="` + strings.Repeat("a", 100000) + `", Long` + "\r\n" +
		"http://provider.example.com/live/u/p/9.ts\r\n"
	inputs := append([]string{}, base...)
	for _, in := range base {
		inputs = append(inputs, strings.TrimSuffix(in, "\n"))
	}
	inputs = append(inputs, long, strings.TrimSuffix(long, "\r\n"))

	for i, in := range inputs {
		var out bytes.Buffer
		if err := c.rewritePlaylistHostsTo(ctx, strings.NewReader(in), &out); err != nil {
			t.Fatalf("input %d: %v", i, err)
		}
		want := c.legacyRewritePlaylistHosts(ctx, []byte(in))
		if !bytes.Equal(out.Bytes(), want) {
			t.Errorf("input %d: streamed output differs from legacy (len %d vs %d)", i, out.Len(), len(want))
		}
	}
}

func rewriteHostsString(t *testing.T, c *Config, ctx *gin.Context, in string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.rewritePlaylistHostsTo(ctx, strings.NewReader(in), &buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func newImgRouter(c *Config) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	c.routes(router.Group("/"))
	return router
}

func countingUpstream(t *testing.T) (*httptest.Server, *int32) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fake-png-bytes"))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func getImg(router *gin.Engine, target string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

func TestAssetProxyRejectsUnsignedURL(t *testing.T) {
	up, hits := countingUpstream(t)
	w := getImg(newImgRouter(testImageProxyConfig()), "/img?url="+url.QueryEscape(up.URL))
	if w.Code != http.StatusForbidden || atomic.LoadInt32(hits) != 0 {
		t.Fatalf("status = %d, upstream hits = %d; want 403 and 0", w.Code, *hits)
	}
}

func TestAssetProxyRejectsSignatureOfAnotherURL(t *testing.T) {
	up, hits := countingUpstream(t)
	c := testImageProxyConfig()
	w := getImg(newImgRouter(c), "/img?url="+url.QueryEscape(up.URL)+"&sig="+c.imageSignature("http://a.example/x.png"))
	if w.Code != http.StatusForbidden || atomic.LoadInt32(hits) != 0 {
		t.Fatalf("status = %d, upstream hits = %d; want 403 and 0", w.Code, *hits)
	}
}

func TestAssetProxyServesSignedURL(t *testing.T) {
	up, _ := countingUpstream(t)
	c := testImageProxyConfig()
	w := getImg(newImgRouter(c), c.proxyImagePath(up.URL+"/logo.png?a=1&b=%20é"))
	if w.Code != http.StatusOK || w.Body.String() != "fake-png-bytes" {
		t.Fatalf("status = %d, body = %q; want 200 and the upstream body", w.Code, w.Body.String())
	}
}

func TestProxyImagePathSignsWithCustomEndpoint(t *testing.T) {
	up, _ := countingUpstream(t)
	c := testImageProxyConfig()
	c.CustomEndpoint = "providerA"
	path := c.proxyImagePath(up.URL + "/logo.png")
	if !strings.HasPrefix(path, "/providerA/img?url=") {
		t.Fatalf("path = %q, want /providerA/img?url= prefix", path)
	}
	if w := getImg(newImgRouter(c), path); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
}

func TestRewriteM3U8SignsChildURLs(t *testing.T) {
	c := testImageProxyConfig()
	base, _ := url.Parse("http://upstream.example.com/hls/index.m3u8")
	out := string(c.rewriteM3U8(testRequestContext(), base, []byte("#EXTM3U\nseg1.ts\nsub/list.m3u8\n")))
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "/img?url=") {
			continue
		}
		n++
		u, err := url.Parse(line)
		if err != nil || u.Query().Get("sig") != c.imageSignature(u.Query().Get("url")) {
			t.Errorf("line %q has no valid signature", line)
		}
	}
	if n != 2 {
		t.Fatalf("found %d /img lines in %q, want 2", n, out)
	}
}

func TestLoadImageSigningKey(t *testing.T) {
	dir := t.TempDir()
	key := loadImageSigningKey(dir)
	path := filepath.Join(dir, imageSigningKeyFile)
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 || fi.Size() != imageSigningKeySize || len(key) != imageSigningKeySize {
		t.Fatalf("stat = %v, %v; want a 0600 file of %d bytes", fi, err, imageSigningKeySize)
	}
	if !bytes.Equal(loadImageSigningKey(dir), key) {
		t.Error("second load returned a different key")
	}
	if err := os.WriteFile(path, []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if k := loadImageSigningKey(dir); len(k) != imageSigningKeySize {
		t.Errorf("short key file not replaced: got %d bytes", len(k))
	}
	if b, _ := os.ReadFile(path); len(b) != imageSigningKeySize {
		t.Errorf("file holds %d bytes, want %d", len(b), imageSigningKeySize)
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	unreadable := t.TempDir()
	kp := filepath.Join(unreadable, imageSigningKeyFile)
	good := bytes.Repeat([]byte("k"), imageSigningKeySize)
	if err := os.WriteFile(kp, good, 0o000); err != nil {
		t.Fatal(err)
	}
	if k := loadImageSigningKey(unreadable); len(k) != imageSigningKeySize || bytes.Equal(k, good) {
		t.Errorf("unreadable key file: want a fresh 32-byte in-memory key")
	}
	if err := os.Chmod(kp, 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(kp); !bytes.Equal(b, good) {
		t.Error("unreadable key file was overwritten")
	}

	ro := t.TempDir()
	if err := os.Chmod(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	if k := loadImageSigningKey(ro); len(k) != imageSigningKeySize {
		t.Errorf("read-only dir: got %d bytes, want %d", len(k), imageSigningKeySize)
	}
	if _, err := os.Stat(filepath.Join(ro, imageSigningKeyFile)); err == nil {
		t.Error("key file written into a read-only dir")
	}
}
