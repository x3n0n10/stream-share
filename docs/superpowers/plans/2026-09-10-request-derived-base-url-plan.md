# Request-Derived Base URL Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop every client-facing URL this proxy generates from depending on `--hostname`/`--advertised-port` config, which collides with Docker's reserved `HOSTNAME` env var — derive protocol/host/port from the live request instead, with `PublicBaseURL` as the one remaining explicit override.

**Architecture:** One shared helper, `requestBaseURL(ctx)`, becomes the single source of truth for "what host can a client reach us at." Everything that already runs inside a live request (player_api JSON, xmltv, HLS manifests, the generic `/img` route, the login response) calls it directly. M3U playlist generation (`marshallInto`) drops host-building entirely and emits relative paths only — a new `rewritePlaylistHosts` prefixes them with `requestBaseURL(ctx)` at serve time, so one cached playlist file stays correct no matter which hostname reaches the server.

**Tech Stack:** Go 1.17 (stdlib only: `net/url`, `net`, `net/http`), Gin. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-10-request-derived-base-url-design.md`

## Global Constraints

- No Go toolchain is installed in this environment. This repo's vendored dependencies require Go **1.23**, not the `go 1.17` directive in `go.mod` (found while shipping the prior spec's CI fix — the vendored modules' own `go.mod` directives push the real floor higher). Run all builds/tests via Docker, from the repo root (`/mnt/user/ai/claude/stream-share`):
  `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run '<TestName>' -v`
  Always pass `-mod=vendor`.
- Every new `.go` file must start with the same GPLv3 header block every existing file starts with.
- All new/modified code lives in package `server` under `pkg/server/`, except the `cmd/root.go` flag-description update in Task 6.
- Match the codebase's existing style: `interface{}` (not `any`), no generics — targets Go 1.17 language level even though the build toolchain is 1.23.
- `X-Forwarded-Host`/`X-Forwarded-Proto` are trusted only when `c.ReverseProxyEnabled` is true — never unconditionally.
- **Known, deliberate limitation, not a task to fix:** `replaceURL`'s current handling of a track URI with embedded HTTP Basic Auth userinfo (`http://user:pass@host/...`) is dropped, not carried forward, by this plan's `replacePath`. There is no existing test covering it, and encoding userinfo into a purely relative value that still resolves correctly at serve time isn't worth the complexity for what appears to be an unused path (confirmed via `grep -rn "basicAuth\|oriURL.User" pkg/server/*.go pkg/server/*_test.go` — the only occurrences are the 3 lines being removed; zero test references). Flag this to your human partner if a real deployment turns out to need it.

---

## File Structure

- **`pkg/server/image_proxy.go`** (modify): `proxyImageURL` splits into `proxyImagePath` (relative) + `proxyImageURL` (now `ctx`-aware); `rewriteImageFields` and `rewriteM3U8` gain a `ctx` param; new `requestBaseURL` and `rewritePlaylistHosts`.
- **`pkg/server/image_proxy_test.go`** (modify): existing tests updated for the new signatures; new `TestRequestBaseURL`, `TestProxyImagePath`, `TestRewritePlaylistHosts`; new shared test helper `testRequestContext()`.
- **`pkg/server/xmltv_icons.go`** (modify): `rewriteXMLTVIcons` gains a `ctx` param.
- **`pkg/server/xmltv_icons_test.go`** (modify): updated for the new signature.
- **`pkg/server/server.go`** (modify): `replaceURL` → `replacePath` (relative-only); `marshallInto` calls `proxyImagePath` instead of `proxyImageURL`.
- **`pkg/server/server_test.go`** (modify): `TestMarshallIntoRewritesTvgLogo` updated for relative output.
- **`pkg/server/xtream_handlers_api.go`** (modify): `xtreamPlayerAPI` threads `ctx` into `rewriteImageFields` and rebuilds the login response's `server_info` from `requestBaseURL`; `xtreamGet`'s cached-file serve becomes read+rewrite+serve.
- **`pkg/server/xtream_handlers_api_test.go`** (new): `xtreamGet` and login-response tests — this file has no existing tests today.
- **`pkg/server/xtream_handlers_stream.go`** (modify): `xtreamXMLTV`, `xtreamHlsStream`, `hlsXtreamStream` thread `ctx` into their `rewriteXMLTVIcons`/`rewriteM3U8` calls.
- **`pkg/server/xtream_handlers_stream_test.go`** (modify): existing tests get an explicit `ctx.Request.Host`.
- **`pkg/server/proxy_handlers.go`** (modify): `getM3U` becomes read+rewrite+serve; `m3u8ReverseProxy` threads `ctx` into its `rewriteM3U8` call.
- **`pkg/server/proxy_handlers_test.go`** (modify): existing tests get an explicit `ctx.Request.Host`.
- **`cmd/root.go`** (modify): `--hostname`/`--advertised-port` flag descriptions marked deprecated.

**Note on task granularity:** Task 2 is unusually large for a single task. It has to be — `proxyImageURL` gaining a `ctx` parameter is a Go signature change with 3 direct callers and 2 more indirect ones, all in the same package; the package cannot compile (and no test can run) until every call site is updated together. Splitting it would just mean intermediate steps that don't build, which defeats the point of a task boundary. It's still organized as a clear step sequence.

---

### Task 1: `requestBaseURL` helper

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Modify: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Produces: `func (c *Config) requestBaseURL(ctx *gin.Context) string` — used by every later task.

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/image_proxy_test.go`:

```go
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
```

No new imports needed — `net/http`, `net/http/httptest`, `github.com/gin-gonic/gin`, and `github.com/lucasduport/stream-share/pkg/config` are all already imported in this file.

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestRequestBaseURL -v`
Expected: FAIL to compile — `c.requestBaseURL undefined`.

- [ ] **Step 3: Write the implementation**

Append to `pkg/server/image_proxy.go`:

```go
// requestBaseURL returns the "scheme://host[:port]" a client should use to
// reach this server, with no trailing slash. Priority:
//  1. PublicBaseURL, if the operator set one explicitly.
//  2. X-Forwarded-Proto/X-Forwarded-Host, but only when ReverseProxyEnabled
//     is on — an unset flag means untrusted client-supplied headers are
//     ignored, since any direct client could otherwise spoof the host used
//     in URLs handed back to itself.
//  3. The live request's own Host header (already includes a non-default
//     port, e.g. under Docker port-mapping the client's Host header
//     already reflects whatever external port they connected through).
// Protocol in cases 2-3 falls back to c.HTTPS when no forwarded-proto
// header is present.
func (c *Config) requestBaseURL(ctx *gin.Context) string {
	if base := strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/"); base != "" {
		return base
	}

	protocol := "http"
	if c.HTTPS {
		protocol = "https"
	}
	host := ctx.Request.Host

	if c.ReverseProxyEnabled {
		if fh := ctx.Request.Header.Get("X-Forwarded-Host"); fh != "" {
			host = fh
		}
		if fp := ctx.Request.Header.Get("X-Forwarded-Proto"); fp != "" {
			protocol = fp
		}
	}

	return fmt.Sprintf("%s://%s", protocol, host)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestRequestBaseURL -v`
Expected: PASS (all 5 subtests).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go
git commit -m "feat: add requestBaseURL, deriving client-facing base URL from the request"
```

---

### Task 2: `proxyImagePath` + `ctx`-aware rewrite cascade

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Modify: `pkg/server/image_proxy_test.go`
- Modify: `pkg/server/xmltv_icons.go`
- Modify: `pkg/server/xmltv_icons_test.go`
- Modify: `pkg/server/xtream_handlers_api.go`
- Modify: `pkg/server/xtream_handlers_stream.go`
- Modify: `pkg/server/xtream_handlers_stream_test.go`
- Modify: `pkg/server/proxy_handlers.go`
- Modify: `pkg/server/proxy_handlers_test.go`

**Interfaces:**
- Consumes: `c.requestBaseURL(ctx) string` (Task 1).
- Produces: `func (c *Config) proxyImagePath(raw string) string` (relative, no ctx — used by Task 3's `marshallInto`); `func (c *Config) proxyImageURL(ctx *gin.Context, raw string) string` (now takes `ctx`); `func (c *Config) rewriteImageFields(ctx *gin.Context, v interface{}) interface{}`; `func (c *Config) rewriteM3U8(ctx *gin.Context, base *url.URL, body []byte) []byte`; `func (c *Config) rewriteXMLTVIcons(ctx *gin.Context, body []byte) []byte`.

This task is one atomic step for compilation purposes (see File Structure note above), organized as: update every test to the new calling convention first, then update every production call site, then verify.

- [ ] **Step 1: Update every test file to the new call signatures**

In `pkg/server/image_proxy_test.go`:

Add this helper right after `testImageProxyConfig`:

```go
func testRequestContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	ctx.Request.Host = "proxy.example.com:8080"
	return ctx
}
```

Change `TestProxyImageURL` to build a `ctx` once and pass it:

```go
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
			"http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/logo.png?x=1&y=2"),
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
```

Add a new, separate test for the relative form right after it:

```go
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
			"/img?url=" + url.QueryEscape("http://upstream.example.com/logo.png?x=1&y=2"),
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
```

Change `TestRewriteImageFields` to pass `ctx`:

```go
func TestRewriteImageFields(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
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

	got, ok := c.rewriteImageFields(ctx, input).(map[string]interface{})
	if !ok {
		t.Fatalf("rewriteImageFields did not return a map[string]interface{}")
	}
	// ... the rest of this test's body (assertions) is unchanged from today.
```

(Only the function signature line and the `c.rewriteImageFields(...)` call change; every assertion below it in the current file stays exactly as-is — do not modify the assertions.)

Change `TestRewriteM3U8` to build `ctx` once and pass it into every call:

```go
func TestRewriteM3U8(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
	base, err := url.Parse("http://upstream.example.com/live/channel1/index.m3u8")
	if err != nil {
		t.Fatalf("url.Parse base: %v", err)
	}
	// ... the `cases` table is unchanged.
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
		// ... the rest of this subtest's assertions are unchanged.
	})
}
```

In `TestAssetProxy`, add one line after each of the 4 existing `ctx.Request = httptest.NewRequest(...)` lines (in all 4 subtests: "rejects a non-http(s) scheme", "streams a plain image response unmodified", "rewrites nested segment URIs in an m3u8 manifest response", "rewrites segment URIs against the post-redirect URL, not the requested one"):

```go
		ctx.Request.Host = "proxy.example.com:8080"
```

In `pkg/server/xmltv_icons_test.go`, add imports and build one shared `ctx`:

```go
import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRewriteXMLTVIcons(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
```

Then change every one of the 6 subtests' `c.rewriteXMLTVIcons([]byte(in))` calls to `c.rewriteXMLTVIcons(ctx, []byte(in))` — the rest of each subtest body is unchanged.

In `pkg/server/xtream_handlers_stream_test.go`, add one line after each of the 2 existing `ctx.Request = httptest.NewRequest(...)` lines (in `TestXtreamHlsStreamRewritesManifestHost` and `TestHlsXtreamStreamRewritesManifestHost`):

```go
	ctx.Request.Host = "proxy.example.com:8080"
```

In `pkg/server/proxy_handlers_test.go`, add the same one line after the `ctx.Request = httptest.NewRequest(...)` line in `TestM3U8ReverseProxyRewritesSegmentURIs` and in `TestM3U8ReverseProxyRewritesAgainstPostRedirectURL` (NOT in `TestStreamProxiesUpstreamBody` — that one doesn't call any of these functions and is unaffected).

- [ ] **Step 2: Run the build to confirm it fails to compile**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go build -mod=vendor ./... && docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go vet -mod=vendor ./pkg/server/...`
Expected: FAIL — the test files now call functions with signatures that don't exist yet in production code. This confirms the test updates are wired to the new calling convention before you touch production code.

- [ ] **Step 3: Update production code**

In `pkg/server/image_proxy.go`, replace the existing `proxyImageURL` function with:

```go
// proxyImagePath returns the relative /img?url= path for raw, with no
// scheme or host — for callers with no live request (see marshallInto).
// Empty, malformed, or relative values are returned unchanged.
func (c *Config) proxyImagePath(raw string) string {
	if _, err := url.ParseRequestURI(raw); err != nil {
		return raw
	}
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	return fmt.Sprintf("%s/img?url=%s", customEnd, url.QueryEscape(raw))
}

// proxyImageURL rewrites an absolute image/asset URL to point at this
// proxy's generic asset route instead of the upstream provider, so
// clients never resolve icons/covers/manifests directly against the
// provider.
func (c *Config) proxyImageURL(ctx *gin.Context, raw string) string {
	path := c.proxyImagePath(raw)
	if path == raw {
		return raw // proxyImagePath left it untouched (empty/malformed/relative)
	}
	return c.requestBaseURL(ctx) + path
}
```

Replace `rewriteImageFields`:

```go
// rewriteImageFields walks a JSON-decoded player_api response (nested
// map[string]interface{} / []interface{}) and rewrites every known
// upstream-image field through proxyImageURL, in place, at any nesting
// depth. direct_source is deleted rather than rewritten: a player that
// uses it bypasses c.sessionManager's multiplexing entirely, and hiding
// the URL wouldn't restore that, so it's dropped to force a fall back to
// the session-managed stream_id play URL instead.
func (c *Config) rewriteImageFields(ctx *gin.Context, v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		for key, fieldVal := range val {
			switch key {
			case "stream_icon", "cover", "movie_image":
				if s, ok := fieldVal.(string); ok {
					val[key] = c.proxyImageURL(ctx, s)
				}
			case "backdrop_path":
				if arr, ok := fieldVal.([]interface{}); ok {
					for i, item := range arr {
						if s, ok := item.(string); ok {
							arr[i] = c.proxyImageURL(ctx, s)
						}
					}
				}
			case "direct_source":
				delete(val, key)
			default:
				val[key] = c.rewriteImageFields(ctx, fieldVal)
			}
		}
		return val
	case []interface{}:
		for i, item := range val {
			val[i] = c.rewriteImageFields(ctx, item)
		}
		return val
	default:
		return v
	}
}
```

Replace `rewriteM3U8`:

```go
// rewriteM3U8 rewrites every segment, key, and sub-playlist URI in an HLS
// manifest body to point through this proxy instead of the upstream
// provider. Each URI is resolved against base (the manifest's own fetch
// URL) before being handed to proxyImageURL, so a manifest-relative URI
// and an absolute one are handled identically.
func (c *Config) rewriteM3U8(ctx *gin.Context, base *url.URL, body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#") && !strings.Contains(trimmed, "URI="):
			// comments/tags without a URI attribute pass through unchanged
		case strings.Contains(trimmed, "URI="):
			// #EXT-X-KEY / #EXT-X-MAP: rewrite the quoted URI= attribute in place
			lines[i] = rewriteQuotedURI(trimmed, "URI=", func(raw string) string {
				return c.proxyImageURL(ctx, resolveM3U8URI(base, raw))
			})
		default:
			// a bare line is itself a segment or sub-playlist URI
			lines[i] = c.proxyImageURL(ctx, resolveM3U8URI(base, trimmed))
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
```

In `assetProxy`, change the one call:

```go
	body = c.rewriteM3U8(ctx, base, body)
```

In `pkg/server/xmltv_icons.go`, add the gin import and update the function signature:

```go
import (
	"bytes"
	"encoding/xml"
	"io"

	"github.com/gin-gonic/gin"
)
```

```go
func (c *Config) rewriteXMLTVIcons(ctx *gin.Context, body []byte) []byte {
```

(the function body is unchanged except its one `c.proxyImageURL(attr.Value)` call, which becomes `c.proxyImageURL(ctx, attr.Value)`).

In `pkg/server/xtream_handlers_api.go`, change the `xtreamPlayerAPI` call:

```go
	processedResp = c.rewriteImageFields(ctx, processedResp)
```

In `pkg/server/xtream_handlers_stream.go`, change `xtreamXMLTV`'s call:

```go
	ctx.Data(http.StatusOK, "application/xml", c.rewriteXMLTVIcons(ctx, resp))
```

And change both occurrences (in `xtreamHlsStream` and `hlsXtreamStream`) of:

```go
			b = c.rewriteM3U8(loc, b)
```

to:

```go
			b = c.rewriteM3U8(ctx, loc, b)
```

In `pkg/server/proxy_handlers.go`, change `m3u8ReverseProxy`'s call:

```go
	body = c.rewriteM3U8(ctx, base, body)
```

- [ ] **Step 4: Run the full test suite to verify everything compiles and passes**

Run:
```
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go build -mod=vendor ./...
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -v
```
Expected: build succeeds, every test passes — including the 4 `TestAssetProxy` subtests, both `xtream_handlers_stream_test.go` tests, and both `proxy_handlers_test.go` tests you added a `ctx.Request.Host` line to, which would otherwise now fail (they'd derive `http://example.com/...` — `httptest.NewRequest`'s default host for a relative target — instead of the `http://proxy.example.com:8080/...` their assertions expect).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go pkg/server/xmltv_icons.go pkg/server/xmltv_icons_test.go pkg/server/xtream_handlers_api.go pkg/server/xtream_handlers_stream.go pkg/server/xtream_handlers_stream_test.go pkg/server/proxy_handlers.go pkg/server/proxy_handlers_test.go
git commit -m "refactor: derive image/manifest proxy URLs from the request instead of static config"
```

---

### Task 3: `replacePath` (relative M3U track URLs) + `marshallInto`

**Files:**
- Modify: `pkg/server/server.go`
- Modify: `pkg/server/server_test.go`

**Interfaces:**
- Consumes: `c.proxyImagePath(raw string) string` (Task 2).
- Produces: `func (c *Config) replacePath(uri string, trackIndex int, xtream bool) (string, error)` — replaces `replaceURL`, used by `marshallInto` (this task) and nowhere else.

- [ ] **Step 1: Write the failing test**

Replace `pkg/server/server_test.go`'s `TestMarshallIntoRewritesTvgLogo` body with:

```go
func TestMarshallIntoRewritesTvgLogo(t *testing.T) {
	c := &Config{
		ProxyConfig: &config.ProxyConfig{},
		playlist: &m3u.Playlist{
			Tracks: []m3u.Track{
				{
					Name:   "Channel One",
					Length: -1,
					URI:    "http://upstream.example.com/stream1",
					Tags: []m3u.Tag{
						{Name: "tvg-id", Value: "ch1"},
						{Name: "tvg-logo", Value: "http://upstream.example.com/logo1.png"},
					},
				},
			},
		},
	}

	f, err := os.CreateTemp(t.TempDir(), "playlist-*.m3u")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := c.marshallInto(f, true); err != nil {
		t.Fatalf("marshallInto: %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	wantLogo := "/img?url=" + url.QueryEscape("http://upstream.example.com/logo1.png")
	if !strings.Contains(out, wantLogo) {
		t.Errorf("output %q does not contain relative rewritten tvg-logo %q", out, wantLogo)
	}
	if strings.Contains(out, `"http://upstream.example.com/logo1.png"`) {
		t.Errorf("output %q still contains the raw upstream logo URL", out)
	}
	if strings.Contains(out, "://") && strings.Contains(out, "tvg-logo=") {
		// crude guard: tvg-logo's value should never contain a scheme once
		// rewritten — it must stay relative for rewritePlaylistHosts to prefix.
		if idx := strings.Index(out, "tvg-logo="); idx != -1 && strings.Contains(out[idx:idx+200], "://") {
			t.Errorf("output %q has an absolute tvg-logo value, want relative", out)
		}
	}
	if !strings.Contains(out, `tvg-id="ch1"`) {
		t.Errorf("output %q lost the untouched tvg-id tag", out)
	}
	if !strings.Contains(out, "\n/stream1\n") {
		t.Errorf("output %q does not contain the relative track URI /stream1", out)
	}
}
```

`HostConfig`/`AdvertisedPort` are dropped from the test `Config` entirely — this test's whole point after this change is that `marshallInto` no longer needs them.

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestMarshallIntoRewritesTvgLogo -v`
Expected: FAIL — output still contains an absolute `http://...` tvg-logo value and the old-style track URL, not the relative forms.

- [ ] **Step 3: Write the implementation**

In `pkg/server/server.go`, replace the entire `replaceURL` function (currently starting `// ReplaceURL replace original playlist url by proxy url` through its closing `}`) with:

```go
// replacePath rewrites a track URI into the relative path this proxy will
// serve it at — no scheme or host, since the same cached playlist may be
// served to clients reaching this server via different hostnames; the
// host gets prefixed at serve time by rewritePlaylistHosts.
//
// Known limitation: a track URI with embedded HTTP Basic Auth credentials
// (rare; e.g. a raw M3U-provider URI shaped like http://user:pass@host/...)
// previously carried those credentials into the generated proxy URL. That
// is not preserved here — there is no existing test covering it, and
// encoding it into a purely relative value that still resolves correctly
// at serve time isn't worth the complexity for what appears to be an
// unused path.
func (c *Config) replacePath(uri string, trackIndex int, xtream bool) (string, error) {
	oriURL, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = fmt.Sprintf("/%s", customEnd)
	}

	uriPath := oriURL.EscapedPath()
	if xtream {
		// Xtream get.php mode: replace provider creds with local creds in path
		uriPath = strings.ReplaceAll(uriPath, c.XtreamUser.PathEscape(), c.User.PathEscape())
		uriPath = strings.ReplaceAll(uriPath, c.XtreamPassword.PathEscape(), c.Password.PathEscape())
	} else {
		// M3U proxified path
		uriPath = path.Join(
			"/",
			c.endpointAntiColision,
			c.User.PathEscape(),
			c.Password.PathEscape(),
			fmt.Sprintf("%d", trackIndex),
			path.Base(uriPath),
		)
	}

	return customEnd + uriPath, nil
}
```

In `marshallInto`, change the two calls:

```go
		tagValue := track.Tags[i].Value
		if track.Tags[i].Name == "tvg-logo" {
			tagValue = c.proxyImagePath(tagValue)
		}
```

and:

```go
		uri, err := c.replacePath(track.URI, i-ret, xtream)
```

(both are the only two lines that change inside `marshallInto`; everything else in the function body is unchanged.)

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestMarshallIntoRewritesTvgLogo -v`
Expected: PASS.

Also run the full build, since `replaceURL` → `replacePath` is a rename with exactly one call site (inside `marshallInto`) — confirm nothing else in the package still references the old name:

```
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go build -mod=vendor ./...
```
Expected: builds cleanly.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/server.go pkg/server/server_test.go
git commit -m "refactor: emit relative M3U track/tvg-logo paths instead of static-host absolute URLs"
```

---

### Task 4: `rewritePlaylistHosts`

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Modify: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Consumes: `c.requestBaseURL(ctx) string` (Task 1), `rewriteQuotedURI` (already exists, from the prior spec's implementation).
- Produces: `func (c *Config) rewritePlaylistHosts(ctx *gin.Context, body []byte) []byte` — used by Task 5.

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/image_proxy_test.go`:

```go
func TestRewritePlaylistHosts(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()

	t.Run("track URI and tvg-logo both get the base prefixed", func(t *testing.T) {
		in := "#EXTM3U\n" +
			`#EXTINF:-1 tvg-id="ch1" tvg-logo="/img?url=http%3A%2F%2Fupstream.example.com%2Flogo.png", Channel One` + "\n" +
			"/anti/user/pass/0/stream1\n"

		out := string(c.rewritePlaylistHosts(ctx, []byte(in)))

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
		out := string(c.rewritePlaylistHosts(ctx, []byte(in)))
		if !strings.HasPrefix(out, `#EXTINF:-1 tvg-id="ch2", Channel Two`) {
			t.Errorf("output %q changed the untouched EXTINF line", out)
		}
		if !strings.Contains(out, "http://proxy.example.com:8080/anti/user/pass/1/stream2") {
			t.Errorf("output %q does not contain prefixed track URI", out)
		}
	})

	t.Run("a non-# line not starting with / is left alone", func(t *testing.T) {
		in := "not-a-path-or-comment"
		out := string(c.rewritePlaylistHosts(ctx, []byte(in)))
		if out != in {
			t.Errorf("output = %q, want unchanged %q", out, in)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestRewritePlaylistHosts -v`
Expected: FAIL to compile — `c.rewritePlaylistHosts undefined`.

- [ ] **Step 3: Write the implementation**

Append to `pkg/server/image_proxy.go`:

```go
// rewritePlaylistHosts prepends the current request's base URL to every
// relative track URI and tvg-logo value in an M3U body generated by
// marshallInto. marshallInto never emits an absolute URL, so any value
// starting with "/" is always ours to prefix — no resolution needed, unlike
// rewriteM3U8's cross-origin HLS case.
func (c *Config) rewritePlaylistHosts(ctx *gin.Context, body []byte) []byte {
	base := c.requestBaseURL(ctx)
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(trimmed, "#EXTINF") && strings.Contains(trimmed, "tvg-logo="):
			lines[i] = rewriteQuotedURI(trimmed, "tvg-logo=", func(raw string) string {
				if strings.HasPrefix(raw, "/") {
					return base + raw
				}
				return raw
			})
		case !strings.HasPrefix(trimmed, "#") && strings.HasPrefix(trimmed, "/"):
			lines[i] = base + trimmed
		}
	}
	return []byte(strings.Join(lines, "\n"))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestRewritePlaylistHosts -v`
Expected: PASS (all 3 subtests).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go
git commit -m "feat: add rewritePlaylistHosts for serve-time M3U host prefixing"
```

---

### Task 5: Wire `rewritePlaylistHosts` into `getM3U` and `xtreamGet`

**Files:**
- Modify: `pkg/server/proxy_handlers.go`
- Modify: `pkg/server/xtream_handlers_api.go`
- Create: `pkg/server/xtream_handlers_api_test.go`

**Interfaces:**
- Consumes: `c.rewritePlaylistHosts(ctx, body) []byte` (Task 4).

- [ ] **Step 1: Write the failing test**

`pkg/server/xtream_handlers_api_test.go` (new file — `xtream_handlers_api.go` has no existing test file):

```go
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

func TestXtreamGetRewritesHostPerRequest(t *testing.T) {
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
		XtreamBaseURL:      "http://upstream.example.com",
		XtreamUser:         "xu",
		XtreamPassword:     "xp",
	}}

	upstreamQ := url.Values{}
	upstreamQ.Set("username", c.XtreamUser.String())
	upstreamQ.Set("password", c.XtreamPassword.String())
	m3uURL, err := url.Parse(c.XtreamBaseURL + "/get.php?" + upstreamQ.Encode())
	if err != nil {
		t.Fatalf("url.Parse: %v", err)
	}
	xtreamM3uCacheLock.Lock()
	xtreamM3uCache[m3uURL.String()] = cacheMeta{cachedPath, time.Now()}
	xtreamM3uCacheLock.Unlock()

	call := func(host string) string {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/get.php", nil)
		ctx.Request.Host = host
		c.xtreamGet(ctx)
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
```

This is the direct regression test for the bug that motivated this whole plan: one cached M3U file, two requests with different `Host` headers, each gets a correctly-prefixed result — proving the fix works, not just that it compiles.

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestXtreamGetRewritesHostPerRequest -v`
Expected: FAIL — `bodyA`/`bodyB` both contain the raw relative path (no host prefix at all), since `xtreamGet` still calls `ctx.File(path)` unmodified.

- [ ] **Step 3: Write the implementation**

In `pkg/server/proxy_handlers.go`, add `"os"` to the import block, then replace `getM3U`:

```go
// getM3U sends the proxified M3U file generated during bootstrap, with
// track URIs and tvg-logo values prefixed for the current request's host.
func (c *Config) getM3U(ctx *gin.Context) {
	body, err := os.ReadFile(c.proxyfiedM3UPath)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	ctx.Data(http.StatusOK, "application/octet-stream", c.rewritePlaylistHosts(ctx, body))
}
```

In `pkg/server/xtream_handlers_api.go`, add `"os"` to the import block, then in `xtreamGet`, replace the tail — from the `ctx.Header("Content-Disposition", ...)` line through the final `ctx.File(path)` line — with:

```go
	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	xtreamM3uCacheLock.RLock()
	path := xtreamM3uCache[m3uURL.String()].string
	xtreamM3uCacheLock.RUnlock()

	body, err := os.ReadFile(path)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	ctx.Data(http.StatusOK, "application/octet-stream", c.rewritePlaylistHosts(ctx, body))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run TestXtreamGetRewritesHostPerRequest -v`
Expected: PASS.

Then run the full test suite and build, since both `getM3U` and `xtreamGet` changed:

```
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go build -mod=vendor ./...
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -v
```
Expected: all pass.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/proxy_handlers.go pkg/server/xtream_handlers_api.go pkg/server/xtream_handlers_api_test.go
git commit -m "fix: serve cached M3U files with the requesting client's own host prefixed"
```

---

### Task 6: Login response uses `requestBaseURL`; deprecate `--hostname`/`--advertised-port`

**Files:**
- Modify: `pkg/server/xtream_handlers_api.go`
- Modify: `pkg/server/xtream_handlers_api_test.go`
- Modify: `cmd/root.go`

**Interfaces:**
- Consumes: `c.requestBaseURL(ctx) string` (Task 1).

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/xtream_handlers_api_test.go` (add `"encoding/json"` to its imports):

```go
func TestXtreamPlayerAPILoginUsesRequestHost(t *testing.T) {
	gin.SetMode(gin.TestMode)

	c := &Config{ProxyConfig: &config.ProxyConfig{HostConfig: &config.HostConfiguration{Port: 8080}}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	ctx.Request.Host = "viewer.example.com:9090"

	c.xtreamPlayerAPI(ctx, url.Values{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		ServerInfo struct {
			URL  string `json:"url"`
			Port string `json:"port"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.ServerInfo.URL != "http://viewer.example.com" {
		t.Errorf("server_info.url = %q, want %q", resp.ServerInfo.URL, "http://viewer.example.com")
	}
	if resp.ServerInfo.Port != "9090" {
		t.Errorf("server_info.port = %q, want %q", resp.ServerInfo.Port, "9090")
	}
}

func TestXtreamPlayerAPILoginFallsBackToListenPortWhenHostHasNone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{HostConfig: &config.HostConfiguration{Port: 8080}}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	ctx.Request.Host = "viewer.example.com"

	c.xtreamPlayerAPI(ctx, url.Values{})

	var resp struct {
		ServerInfo struct {
			Port string `json:"port"`
		} `json:"server_info"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if resp.ServerInfo.Port != "8080" {
		t.Errorf("server_info.port = %q, want fallback listen port %q", resp.ServerInfo.Port, "8080")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run 'TestXtreamPlayerAPILoginUsesRequestHost|TestXtreamPlayerAPILoginFallsBackToListenPortWhenHostHasNone' -v`
Expected: FAIL — `server_info.url`/`.port` come back empty or wrong (built from unset `c.HostConfig.Hostname`/`c.AdvertisedPort`, not the request).

- [ ] **Step 3: Write the implementation**

In `pkg/server/xtream_handlers_api.go`, add `"net"` to the import block, then replace the body of the `if strings.TrimSpace(action) == ""` block (everything from `protocol := "http"` through the `loginResp := map[string]interface{}{...}` literal's `"server_info"` entry) with:

```go
	if strings.TrimSpace(action) == "" {
		base := c.requestBaseURL(ctx)
		protocol, hostPort, _ := strings.Cut(base, "://")
		host, portStr, err := net.SplitHostPort(hostPort)
		if err != nil {
			// hostPort had no ":port" (client connected on the scheme's
			// default port, which the Host header omits) — fall back to the
			// real listen port, the only value guaranteed to exist.
			host = hostPort
			portStr = strconv.Itoa(c.HostConfig.Port)
		}
		now := time.Now()
		nowUnix := strconv.FormatInt(now.Unix(), 10)
		expDate := strconv.FormatInt(now.Add(365*24*time.Hour).Unix(), 10)

		loginResp := map[string]interface{}{
			"user_info": map[string]interface{}{
				"username":               c.User.String(),
				"password":               c.Password.String(),
				"message":                "",
				"auth":                   "1",
				"status":                 "Active",
				"exp_date":               expDate,
				"is_trial":               "0",
				"active_cons":            "0",
				"created_at":             nowUnix,
				"max_connections":        "1",
				"allowed_output_formats": []string{"m3u8", "ts"},
			},
			"server_info": map[string]interface{}{
				"url":             fmt.Sprintf("%s://%s", protocol, host),
				"port":            portStr,
				"https_port":      portStr,
				"server_protocol": protocol,
				"rtmp_port":       portStr,
				"timezone":        "UTC",
				"timestamp_now":   nowUnix,
				"time_now":        now.UTC().Format("2006-01-02 15:04:05"),
			},
		}
```

(the rest of the block — the `utils.InfoLog`/debug-cache-write/`ctx.JSON` lines — is unchanged.)

In `cmd/root.go`, update the two flag descriptions:

```go
	rootCmd.Flags().Int("advertised-port", 0, "Deprecated, no longer used for generated URLs (derived from the request); kept for backward compatibility")
```

```go
	rootCmd.Flags().String("hostname", "", "Deprecated, no longer used for generated URLs (derived from the request); kept for backward compatibility")
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./pkg/server/... -run 'TestXtreamPlayerAPILoginUsesRequestHost|TestXtreamPlayerAPILoginFallsBackToListenPortWhenHostHasNone' -v`
Expected: PASS (both tests).

Then run the full build and test suite one final time — this is the last task:

```
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go build -mod=vendor ./...
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.23 go test -mod=vendor ./... -v
```
Expected: build succeeds, every package's tests pass.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/xtream_handlers_api.go pkg/server/xtream_handlers_api_test.go cmd/root.go
git commit -m "fix: derive Xtream login response server_info from the request; deprecate --hostname/--advertised-port"
```

---

## Self-Review

**Spec coverage:**
- §1 `requestBaseURL` → Task 1.
- §2 `proxyImagePath`/`proxyImageURL` → Task 2.
- §3 relative M3U generation (`replacePath`, `marshallInto`) + `rewritePlaylistHosts` + serve-time wiring (`getM3U`, `xtreamGet`) → Tasks 3, 4, 5.
- §4 Xtream login response → Task 6.
- §5 `--hostname`/`--advertised-port` deprecation → Task 6 (flag descriptions) + a natural consequence of Tasks 2/3/6 (nothing left reads `HostConfig.Hostname`/`AdvertisedPort` for URL generation after this plan — confirmed by re-checking every call site found during spec research: `handlers_vod.go` and the Discord-bot-API-URL block in `server.go:294-313` are both explicitly out of scope per the spec, and `HostConfig.Port`'s listen-bind usage at `server.go:482-483` is untouched).
- Error handling (PublicBaseURL used as a raw prefix, forwarded headers trusted only when opted in, empty-Host degrade, file-read error handling) → covered inline in Tasks 1, 5.
- Testing section → one test (or more) per named function/behavior, in the task that introduces it, including the cross-client regression test the spec specifically calls for (Task 5).
- Out of scope (`handlers_vod.go` unification, Approach C memoization, TLS-termination detection, outright flag removal) → correctly not implemented; no task touches any of these.

**Placeholder scan:** no TBD/TODO, no "add error handling" hand-waves, no "similar to Task N" without the actual code — every step shows full code, including the exact existing-test edits (with clear before/after framing where only part of a function changes).

**Type consistency:** `requestBaseURL(ctx *gin.Context) string` (Task 1) is called with a matching signature everywhere in Tasks 2, 4, 5, 6. `proxyImagePath(raw string) string` (Task 2) is called identically by Task 3's `marshallInto`. `rewritePlaylistHosts(ctx *gin.Context, body []byte) []byte` (Task 4) is called identically by both Task 5 call sites. `replacePath(uri string, trackIndex int, xtream bool) (string, error)` (Task 3) matches its one call site inside `marshallInto`, same as the old `replaceURL` it replaces.
