# Proxy EPG/Channel/VOD Image URLs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop upstream provider URLs (icons, covers, `direct_source`, HLS manifest content) from reaching viewing clients unproxied, across every surface that currently leaks them.

**Architecture:** One generic authenticated proxy route (`/img?url=`) plus one helper (`proxyImageURL`) that wraps any absolute URL to point at it. Four independent rewrite mechanisms feed that helper: a JSON tree walk for `player_api.php`, an XML token-copy for `xmltv.php`, a tag-value rewrite in the existing M3U writer, and a line-oriented rewrite for HLS `.m3u8` manifest bodies (which also makes the `/img` route itself manifest-aware, so nested sub-playlists get rewritten recursively).

**Tech Stack:** Go 1.17 (stdlib only: `net/url`, `encoding/xml`, `net/http`), Gin, the existing vendored `jamesnetherton/m3u` and `tellytv/go.xtream-codes` packages. No new dependencies.

**Spec:** `docs/superpowers/specs/2026-09-09-proxy-image-urls-design.md`

## Global Constraints

- No Go toolchain is installed in this environment. Run all builds/tests via Docker:
  `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run '<TestName>' -v`
  (run from the repo root, `/mnt/user/ai/claude/stream-share`). Always pass `-mod=vendor` — the repo vendors every dependency (`vendor/modules.txt` present) and has no network access to resolve modules otherwise.
- Every new `.go` file must start with the same GPLv3 header block every existing file in this repo starts with (copy verbatim from any existing file, e.g. `pkg/server/helpers.go`).
- All new code lives in package `server` under `pkg/server/`.
- Match the codebase's existing style: `interface{}` (not `any`), no generics — the repo targets Go 1.17.
- Every task's diff must be applied on top of the current tip of branch `docs/proxy-image-urls-spec` in `/mnt/user/ai/claude/stream-share` (already rebased onto current `master`).

---

## File Structure

- **`pkg/server/image_proxy.go`** (new): `proxyImageURL`, `rewriteImageFields`, `rewriteM3U8`, `resolveM3U8URI`, `rewriteQuotedURI`, `assetProxy` (the `/img` route handler). Everything the spec's generic image/manifest proxy needs, in one file, per the spec's own file assignment.
- **`pkg/server/image_proxy_test.go`** (new): table tests for all of the above.
- **`pkg/server/xmltv_icons.go`** (new): `rewriteXMLTVIcons`. Kept separate from `image_proxy.go` per the spec ("new file"), since it's XML-specific and has no other dependency on the image-proxy file.
- **`pkg/server/xmltv_icons_test.go`** (new): tests for `rewriteXMLTVIcons`.
- **`pkg/server/proxy_handlers.go`** (modify): extract `buildUpstreamRequest`/`writeUpstreamResponse` out of `stream`; rewrite `m3u8ReverseProxy` to be manifest-aware.
- **`pkg/server/proxy_handlers_test.go`** (new): characterization test for `stream`, test for `m3u8ReverseProxy`.
- **`pkg/server/server.go`** (modify): `marshallInto` rewrites `tvg-logo` tag values.
- **`pkg/server/server_test.go`** (new): test for `marshallInto`'s `tvg-logo` rewrite.
- **`pkg/server/xtream_handlers_api.go`** (modify): wire `rewriteImageFields` into `xtreamPlayerAPI`.
- **`pkg/server/xtream_handlers_stream.go`** (modify): wire `rewriteXMLTVIcons` into `xtreamXMLTV`; wire `rewriteM3U8` into `xtreamHlsStream`/`hlsXtreamStream`, deleting their credential-swap `strings.ReplaceAll`.
- **`pkg/server/xtream_handlers_stream_test.go`** (new): test for the HLS manifest rewrite.
- **`pkg/server/routes.go`** (modify): register `GET /img`.

Two real bugs found while working out exact code (not in the spec, found by reading the actual functions): `xtreamHlsStream`/`hlsXtreamStream` and the new manifest-aware `assetProxy`/`m3u8ReverseProxy` all serve a body whose length changes after rewriting, while `mergeHttpHeader` copies the upstream response's original (now-stale) `Content-Length` header forward first. Left uncorrected, that would send a `Content-Length` that doesn't match the actual rewritten body, which can truncate the response or hang the client. Every task that serves a rewritten manifest body explicitly overwrites `Content-Length` after merging headers, to close this.

Also found while implementing (not a functional problem, just a testing-accuracy correction to the spec's own wording): Go's `encoding/xml` `Decoder`/`Encoder` token round-trip does **not** preserve self-closing tags — `<icon src="x"/>` round-trips as `<icon src="x"></icon>`. This is semantically identical XML and not a regression, but it means the spec's testing claim "a document with no icon elements comes back byte-identical" only holds if that document *also* has no other self-closing elements anywhere. Task 3's test below is written against the verified real behavior, not the spec's slightly-imprecise phrasing.

---

### Task 1: `proxyImageURL` helper

**Files:**
- Create: `pkg/server/image_proxy.go`
- Test: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Produces: `func (c *Config) proxyImageURL(raw string) string` — used by every later task.

- [ ] **Step 1: Write the failing test**

`pkg/server/image_proxy_test.go`:

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
	"net/url"
	"testing"

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestProxyImageURL -v`
Expected: FAIL — `c.proxyImageURL undefined`.

- [ ] **Step 3: Write the implementation**

`pkg/server/image_proxy.go`:

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
	"fmt"
	"net/url"
	"strings"
)

// proxyImageURL rewrites an absolute image/asset URL to point at this
// proxy's generic asset route instead of the upstream provider, so
// clients never resolve icons/covers/manifests directly against the
// provider. Empty, malformed, or relative values are returned unchanged.
func (c *Config) proxyImageURL(raw string) string {
	if _, err := url.ParseRequestURI(raw); err != nil {
		return raw
	}
	protocol := "http"
	if c.HTTPS {
		protocol = "https"
	}
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	return fmt.Sprintf("%s://%s:%d%s/img?url=%s",
		protocol, c.HostConfig.Hostname, c.AdvertisedPort, customEnd,
		url.QueryEscape(raw))
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestProxyImageURL -v`
Expected: PASS (all 3 subtests).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go
git commit -m "feat: add proxyImageURL helper for the generic image/asset proxy"
```

---

### Task 2: `rewriteImageFields` (player_api JSON tree walk, `direct_source` strip)

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Modify: `pkg/server/xtream_handlers_api.go:183` (right before the `if config.CacheFolder != ""` debug block)
- Test: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Consumes: `c.proxyImageURL(string) string` (Task 1).
- Produces: `func (c *Config) rewriteImageFields(v interface{}) interface{}`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/image_proxy_test.go`:

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteImageFields -v`
Expected: FAIL — `c.rewriteImageFields undefined`.

- [ ] **Step 3: Write the implementation**

Append to `pkg/server/image_proxy.go`:

```go
// rewriteImageFields walks a JSON-decoded player_api response (nested
// map[string]interface{} / []interface{}) and rewrites every known
// upstream-image field through proxyImageURL, in place, at any nesting
// depth. direct_source is deleted rather than rewritten: a player that
// uses it bypasses c.sessionManager's multiplexing entirely, and hiding
// the URL wouldn't restore that, so it's dropped to force a fall back to
// the session-managed stream_id play URL instead.
func (c *Config) rewriteImageFields(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		for key, fieldVal := range val {
			switch key {
			case "stream_icon", "cover", "movie_image":
				if s, ok := fieldVal.(string); ok {
					val[key] = c.proxyImageURL(s)
				}
			case "backdrop_path":
				if arr, ok := fieldVal.([]interface{}); ok {
					for i, item := range arr {
						if s, ok := item.(string); ok {
							arr[i] = c.proxyImageURL(s)
						}
					}
				}
			case "direct_source":
				delete(val, key)
			default:
				val[key] = c.rewriteImageFields(fieldVal)
			}
		}
		return val
	case []interface{}:
		for i, item := range val {
			val[i] = c.rewriteImageFields(item)
		}
		return val
	default:
		return v
	}
}
```

Then in `pkg/server/xtream_handlers_api.go`, insert one line right before the existing debug-cache block (currently at line 191):

```go
	processedResp = c.rewriteImageFields(processedResp)

	if config.CacheFolder != "" && utils.IsDebugLogEnabled() {
```

(This goes after the existing `if action == "get_live_streams" { ... }` block and before `ctx.JSON(http.StatusOK, processedResp)`; it runs for every action, matching the spec: `rewriteImageFields` only ever touches the specific known keys, so running it unconditionally is safe.)

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteImageFields -v`
Expected: PASS.

Also run a full package build to catch the `xtream_handlers_api.go` wiring: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go build -mod=vendor ./...`
Expected: builds cleanly.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go pkg/server/xtream_handlers_api.go
git commit -m "feat: rewrite player_api image fields, strip direct_source"
```

---

### Task 3: `rewriteXMLTVIcons` (xmltv.php)

**Files:**
- Create: `pkg/server/xmltv_icons.go`
- Modify: `pkg/server/xtream_handlers_stream.go:105` (the `ctx.Data` call inside `xtreamXMLTV`)
- Test: `pkg/server/xmltv_icons_test.go`

**Interfaces:**
- Consumes: `c.proxyImageURL(string) string` (Task 1).
- Produces: `func (c *Config) rewriteXMLTVIcons(body []byte) []byte`.

- [ ] **Step 1: Write the failing test**

`pkg/server/xmltv_icons_test.go`:

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
	"net/url"
	"strings"
	"testing"
)

func TestRewriteXMLTVIcons(t *testing.T) {
	c := testImageProxyConfig()

	t.Run("channel-level icon src rewritten", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/ch1.png"/></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/ch1.png")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten channel icon %q", out, want)
		}
	})

	t.Run("programme-level icon src rewritten", func(t *testing.T) {
		in := `<tv><programme channel="1"><icon src="http://upstream.example.com/prog1.png"/></programme></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/prog1.png")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten programme icon %q", out, want)
		}
	})

	t.Run("entity-escaped query string survives", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/a?b=1&amp;c=2"/></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/a?b=1&c=2")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain the correctly round-tripped rewritten icon %q", out, want)
		}
	})

	t.Run("url element left untouched", func(t *testing.T) {
		in := `<tv><channel id="1"><url>http://upstream.example.com/info</url></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		if !strings.Contains(out, "http://upstream.example.com/info") {
			t.Errorf("output %q should still contain the untouched <url> content", out)
		}
	})

	// NOTE: encoding/xml's Decoder/Encoder token round-trip does not
	// preserve self-closing tags (<x/> becomes <x></x>), so byte-identity
	// only holds for a document with no self-closing elements at all, not
	// just "no icon elements" as the design doc loosely put it.
	t.Run("document with no self-closing elements round-trips byte-identical", func(t *testing.T) {
		in := "<tv>\n  <channel id=\"1\">\n    <display-name>Channel One</display-name>\n  </channel>\n</tv>\n"
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		if out != in {
			t.Errorf("output = %q, want byte-identical %q", out, in)
		}
	})

	t.Run("malformed XML falls back to the original bytes", func(t *testing.T) {
		in := `<tv><channel id="1">`
		out := c.rewriteXMLTVIcons([]byte(in))
		if string(out) != in {
			t.Errorf("output = %q, want unmodified original %q", string(out), in)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteXMLTVIcons -v`
Expected: FAIL — `c.rewriteXMLTVIcons undefined`.

- [ ] **Step 3: Write the implementation**

`pkg/server/xmltv_icons.go`:

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
	"bytes"
	"encoding/xml"
	"io"
)

// rewriteXMLTVIcons rewrites the src attribute of every <icon> element in
// an XMLTV document — legal on both <channel> and <programme> per the
// XMLTV DTD, so no distinction is made between them — to point through
// this proxy instead of the upstream provider. Implemented as a token-
// stream copy so entity-escaped characters and untouched document
// structure round-trip correctly; <url> elements are a separate tag and
// are left untouched. Falls back to the original bytes unchanged if the
// document fails to decode, so a non-XML upstream error body doesn't
// error the whole EPG response.
func (c *Config) rewriteXMLTVIcons(body []byte) []byte {
	dec := xml.NewDecoder(bytes.NewReader(body))
	var out bytes.Buffer
	enc := xml.NewEncoder(&out)

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return body
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "icon" {
			for i, attr := range se.Attr {
				if attr.Name.Local == "src" {
					se.Attr[i].Value = c.proxyImageURL(attr.Value)
				}
			}
			tok = se
		}
		if err := enc.EncodeToken(tok); err != nil {
			return body
		}
	}
	if err := enc.Flush(); err != nil {
		return body
	}
	return out.Bytes()
}
```

Then in `pkg/server/xtream_handlers_stream.go`, change the last line of `xtreamXMLTV` (currently `ctx.Data(http.StatusOK, "application/xml", resp)`) to:

```go
	ctx.Data(http.StatusOK, "application/xml", c.rewriteXMLTVIcons(resp))
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteXMLTVIcons -v`
Expected: PASS (all 6 subtests).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/xmltv_icons.go pkg/server/xmltv_icons_test.go pkg/server/xtream_handlers_stream.go
git commit -m "feat: rewrite channel and programme icon src in xmltv.php output"
```

---

### Task 4: `marshallInto` rewrites `tvg-logo`

**Files:**
- Modify: `pkg/server/server.go:854-860` (inside `marshallInto`'s tag-writing loop)
- Test: `pkg/server/server_test.go`

**Interfaces:**
- Consumes: `c.proxyImageURL(string) string` (Task 1).

- [ ] **Step 1: Write the failing test**

`pkg/server/server_test.go`:

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
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestMarshallIntoRewritesTvgLogo(t *testing.T) {
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			HostConfig:     &config.HostConfiguration{Hostname: "proxy.example.com"},
			AdvertisedPort: 8080,
		},
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
	defer f.Close()

	if err := c.marshallInto(f, true); err != nil {
		t.Fatalf("marshallInto: %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	wantLogo := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/logo1.png")
	if !strings.Contains(out, wantLogo) {
		t.Errorf("output %q does not contain rewritten tvg-logo %q", out, wantLogo)
	}
	if strings.Contains(out, `"http://upstream.example.com/logo1.png"`) {
		t.Errorf("output %q still contains the raw upstream logo URL", out)
	}
	if !strings.Contains(out, `tvg-id="ch1"`) {
		t.Errorf("output %q lost the untouched tvg-id tag", out)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestMarshallIntoRewritesTvgLogo -v`
Expected: FAIL — output still contains the raw upstream logo URL.

- [ ] **Step 3: Write the implementation**

In `pkg/server/server.go`, inside `marshallInto`, change:

```go
		for i := range track.Tags {
			if i == len(track.Tags)-1 {
				fmt.Fprintf(&buffer, "%s=%q", track.Tags[i].Name, track.Tags[i].Value)
				continue
			}
			fmt.Fprintf(&buffer, "%s=%q ", track.Tags[i].Name, track.Tags[i].Value)
		}
```

to:

```go
		for i := range track.Tags {
			tagValue := track.Tags[i].Value
			if track.Tags[i].Name == "tvg-logo" {
				tagValue = c.proxyImageURL(tagValue)
			}
			if i == len(track.Tags)-1 {
				fmt.Fprintf(&buffer, "%s=%q", track.Tags[i].Name, tagValue)
				continue
			}
			fmt.Fprintf(&buffer, "%s=%q ", track.Tags[i].Name, tagValue)
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestMarshallIntoRewritesTvgLogo -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/server.go pkg/server/server_test.go
git commit -m "feat: rewrite tvg-logo through the image proxy in generated M3U playlists"
```

---

### Task 5: `rewriteM3U8`, `resolveM3U8URI`, `rewriteQuotedURI`

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Test: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Consumes: `c.proxyImageURL(string) string` (Task 1).
- Produces: `func (c *Config) rewriteM3U8(base *url.URL, body []byte) []byte` — used by Tasks 7, 8, 9.

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/image_proxy_test.go`:

```go
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
```

This requires `"strings"` to be imported in `image_proxy_test.go` — add it to the import block alongside `net/url` and `testing`.

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteM3U8 -v`
Expected: FAIL — `c.rewriteM3U8 undefined`.

- [ ] **Step 3: Write the implementation**

Append to `pkg/server/image_proxy.go`:

```go
// rewriteM3U8 rewrites every segment, key, and sub-playlist URI in an HLS
// manifest body to point through this proxy instead of the upstream
// provider. Each URI is resolved against base (the manifest's own fetch
// URL) before being handed to proxyImageURL, so a manifest-relative URI
// and an absolute one are handled identically.
func (c *Config) rewriteM3U8(base *url.URL, body []byte) []byte {
	lines := strings.Split(string(body), "\n")
	for i, line := range lines {
		trimmed := strings.TrimRight(line, "\r")
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#") && !strings.Contains(trimmed, "URI="):
			// comments/tags without a URI attribute pass through unchanged
		case strings.Contains(trimmed, "URI="):
			// #EXT-X-KEY / #EXT-X-MAP: rewrite the quoted URI= attribute in place
			lines[i] = rewriteQuotedURI(trimmed, "URI=", func(raw string) string {
				return c.proxyImageURL(resolveM3U8URI(base, raw))
			})
		default:
			// a bare line is itself a segment or sub-playlist URI
			lines[i] = c.proxyImageURL(resolveM3U8URI(base, trimmed))
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// resolveM3U8URI resolves a manifest URI (relative or absolute) against
// base, the manifest's own fetch URL. Left as-is if raw fails to parse;
// proxyImageURL's own ParseRequestURI check will no-op it too.
func resolveM3U8URI(base *url.URL, raw string) string {
	ref, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return base.ResolveReference(ref).String()
}

// rewriteQuotedURI finds the first key="..." attribute in line (e.g.
// "URI=") and replaces its quoted value via rewrite, leaving the rest of
// the line untouched. Returns line unchanged if the attribute isn't found
// or its quote is unterminated.
func rewriteQuotedURI(line, key string, rewrite func(string) string) string {
	idx := strings.Index(line, key)
	if idx == -1 {
		return line
	}
	start := idx + len(key)
	if start >= len(line) || line[start] != '"' {
		return line
	}
	end := strings.IndexByte(line[start+1:], '"')
	if end == -1 {
		return line
	}
	end += start + 1
	value := line[start+1 : end]
	return line[:start+1] + rewrite(value) + line[end:]
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestRewriteM3U8 -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go
git commit -m "feat: add rewriteM3U8 for rewriting HLS manifest body content"
```

---

### Task 6: Refactor `stream` into `buildUpstreamRequest` + `writeUpstreamResponse`

Pure refactor, no behavior change — needed so `assetProxy` (Task 7) can inspect the upstream response before deciding whether to stream it raw or buffer-and-rewrite it as an HLS manifest, without duplicating `stream`'s request-building/header-normalization logic.

**Files:**
- Modify: `pkg/server/proxy_handlers.go`
- Test: `pkg/server/proxy_handlers_test.go`

**Interfaces:**
- Produces: `func (c *Config) buildUpstreamRequest(ctx *gin.Context, oriURL *url.URL) (*http.Request, error)` and `func writeUpstreamResponse(ctx *gin.Context, resp *http.Response)` — both used by Task 7; `writeUpstreamResponse` also used by Task 9.

- [ ] **Step 1: Write a characterization test for current `stream` behavior**

`pkg/server/proxy_handlers_test.go`:

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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/gin-gonic/gin"
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
```

- [ ] **Step 2: Run test to verify it passes against current, unrefactored code**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestStreamProxiesUpstreamBody -v`
Expected: PASS. (This confirms the test correctly captures `stream`'s existing behavior before we touch it.)

- [ ] **Step 3: Refactor `stream` in `pkg/server/proxy_handlers.go`**

Replace the entire existing `stream` function with:

```go
// buildUpstreamRequest constructs the outbound request to oriURL, using the
// strict VOD header whitelist for VOD-shaped paths and a minimal
// passthrough header set otherwise.
func (c *Config) buildUpstreamRequest(ctx *gin.Context, oriURL *url.URL) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx.Request.Context(), "GET", oriURL.String(), nil)
	if err != nil {
		return nil, err
	}
	if isVODPath(oriURL.Path) {
		req.Header = prepareVODHeaders(ctx)
	} else {
		mergeHttpHeader(req.Header, ctx.Request.Header)
		req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
		req.Header.Del("Accept-Encoding")
		req.Header.Set("Accept-Encoding", "identity")
		if req.Header.Get("Accept") == "" {
			req.Header.Set("Accept", "*/*")
		}
		if req.Header.Get("Connection") == "" {
			req.Header.Set("Connection", "keep-alive")
		}
	}
	return req, nil
}

// writeUpstreamResponse copies resp's headers/status to ctx and streams its
// body to the client with flushes, normalizing a Range-less 206 to 200.
func writeUpstreamResponse(ctx *gin.Context, resp *http.Response) {
	if resp.StatusCode == 461 {
		utils.DebugLog("Upstream returned 461 (often blocks HEAD/Range or unexpected headers)")
	}

	mergeHttpHeader(ctx.Writer.Header(), resp.Header)
	status := resp.StatusCode
	// If the client did not send a Range header but upstream returned 206, the player
	// will get confused (it expected 200) and immediately drop the connection.
	// Normalize to 200 and convert Content-Range into Content-Length.
	if status == http.StatusPartialContent && ctx.Request.Header.Get("Range") == "" {
		status = http.StatusOK
		if cr := ctx.Writer.Header().Get("Content-Range"); cr != "" {
			if idx := strings.LastIndex(cr, "/"); idx >= 0 {
				if total := strings.TrimSpace(cr[idx+1:]); total != "*" && total != "" {
					ctx.Writer.Header().Set("Content-Length", total)
				}
			}
		}
		ctx.Writer.Header().Del("Content-Range")
	}
	ctx.Status(status)

	w := ctx.Writer
	buf := make([]byte, 64*1024)

	for {
		select {
		case <-ctx.Request.Context().Done():
			utils.DebugLog("Client cancelled stream for URL: %s", ctx.Request.URL)
			return
		default:
		}

		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				utils.DebugLog("Client write error: %v", werr)
				return
			}
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		if rerr != nil {
			if rerr != io.EOF {
				utils.DebugLog("Upstream read error: %v", rerr)
			}
			return
		}
	}
}

// stream proxies the content from upstream to the client, preserving status
// and most headers, while normalizing VOD header sets for stricter providers.
func (c *Config) stream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("-> Streaming request URL: %s", ctx.Request.URL)
	utils.DebugLog("-> Proxying to upstream URL: %s", oriURL.String())

	req, err := c.buildUpstreamRequest(ctx, oriURL)
	if err != nil {
		utils.ErrorLog("Failed to create request: %v", err)
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}

	resp, err := streamHTTPClient.Do(req)
	if err != nil {
		utils.DebugLog("-> Upstream request error: %v", err)
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	utils.DebugLog("-> Upstream response status: %d", resp.StatusCode)
	writeUpstreamResponse(ctx, resp)
}
```

This is a pure extraction: every line of behavior is unchanged, just split across three functions instead of one.

- [ ] **Step 4: Run test to verify it still passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestStreamProxiesUpstreamBody -v`
Expected: PASS (identical result to Step 2 — proves the refactor is behavior-preserving).

Also run the full test suite to make sure nothing else regressed: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -v`
Expected: all PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/proxy_handlers.go pkg/server/proxy_handlers_test.go
git commit -m "refactor: split stream into buildUpstreamRequest and writeUpstreamResponse"
```

---

### Task 7: `assetProxy` handler + route registration

**Files:**
- Modify: `pkg/server/image_proxy.go`
- Modify: `pkg/server/routes.go`
- Test: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Consumes: `c.buildUpstreamRequest` and `writeUpstreamResponse` (Task 6), `c.rewriteM3U8` (Task 5).
- Produces: `func (c *Config) assetProxy(ctx *gin.Context)`, registered as `GET /img`.

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/image_proxy_test.go` (needs `"net/http"`, `"net/http/httptest"`, and `"github.com/gin-gonic/gin"` added to its imports):

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestAssetProxy -v`
Expected: FAIL — `c.assetProxy undefined`.

- [ ] **Step 3: Write the implementation**

Append to `pkg/server/image_proxy.go` (add `"io"`, `"net/http"`, and `"strconv"` to its imports):

```go
// assetProxy fetches an arbitrary http(s) URL on behalf of an
// authenticated client and streams it back, so upstream image/manifest
// URLs never reach the client directly. There is deliberately no host
// allowlist: the route sits behind c.authenticate, so only an already-
// authenticated viewer can use it, at the known cost that such a viewer
// could make the server fetch other http(s) URLs too.
//
// A response whose Content-Type or requested path indicates an HLS
// manifest is buffered and run through rewriteM3U8 instead of streamed
// raw, so nested sub-playlist/segment/key URIs get pointed back at this
// same route — recursively, since a rewritten sub-playlist is fetched
// through /img again and re-enters this same check.
func (c *Config) assetProxy(ctx *gin.Context) {
	target, err := url.Parse(ctx.Query("url"))
	if err != nil || (target.Scheme != "http" && target.Scheme != "https") {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	req, err := c.buildUpstreamRequest(ctx, target)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	resp, err := streamHTTPClient.Do(req)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	contentType := resp.Header.Get("Content-Type")
	isManifest := strings.Contains(contentType, "mpegurl") || strings.HasSuffix(strings.ToLower(target.Path), ".m3u8")
	if !isManifest {
		writeUpstreamResponse(ctx, resp)
		return
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	body = c.rewriteM3U8(target, body)
	mergeHttpHeader(ctx.Writer.Header(), resp.Header)
	// The rewritten body is a different length than upstream's; overwrite
	// the Content-Length mergeHttpHeader just copied from upstream, or the
	// client gets a length that doesn't match the actual body.
	ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	ctx.Data(resp.StatusCode, contentType, body)
}
```

`image_proxy.go` needs `"github.com/gin-gonic/gin"` and `"github.com/lucasduport/stream-share/pkg/utils"` added to its imports for this (for `*gin.Context` and `utils.PrintErrorAndReturn`).

Then in `pkg/server/routes.go`, inside `func (c *Config) routes(r *gin.RouterGroup)`, register the route right after grouping and before the Xtream/M3U branch:

```go
func (c *Config) routes(r *gin.RouterGroup) {
	r = r.Group(c.CustomEndpoint)

	r.GET("/img", c.authenticate, c.assetProxy)

	// Xtream service endpoints
	if c.XtreamBaseURL != "" {
```

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestAssetProxy -v`
Expected: PASS (all 3 subtests).

Also run the full build: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go build -mod=vendor ./...`
Expected: builds cleanly (confirms the `routes.go` wiring compiles).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/image_proxy.go pkg/server/image_proxy_test.go pkg/server/routes.go
git commit -m "feat: add GET /img generic asset proxy, manifest-aware for HLS"
```

---

### Task 8: Wire `rewriteM3U8` into `xtreamHlsStream` / `hlsXtreamStream`

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (two near-identical blocks, inside `xtreamHlsStream` and `hlsXtreamStream`)
- Test: `pkg/server/xtream_handlers_stream_test.go`

**Interfaces:**
- Consumes: `c.rewriteM3U8` (Task 5).

- [ ] **Step 1: Write the failing test**

`pkg/server/xtream_handlers_stream_test.go`:

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
	ctx.Params = gin.Params{{Key: "token", Value: "tok1"}, {Key: "chunk", Value: "chan1_1"}}

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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run 'TestXtreamHlsStreamRewritesManifestHost|TestHlsXtreamStreamRewritesManifestHost' -v`
Expected: FAIL — the response body still contains the raw upstream segment path (`/hlsr/tok1/upstreamuser/upstreampass/...` / `segment1.ts` unrewritten), not the `/img?url=` form.

- [ ] **Step 3: Write the implementation**

In `pkg/server/xtream_handlers_stream.go`, this exact block appears twice — once inside `xtreamHlsStream`, once inside `hlsXtreamStream`. Apply the identical edit both places:

Replace:

```go
			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			body := string(b)
			body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
			utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
			return
```

with:

```go
			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			b = c.rewriteM3U8(loc, b)
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			// The rewritten body is a different length than upstream's; overwrite
			// the Content-Length mergeHttpHeader just copied from upstream.
			ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(b)))
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), b)
			return
```

(`strconv` is already imported in this file.) This is a net deletion of the old `strings.ReplaceAll`/`utils.DebugLog` lines, not an addition on top of them — the credential-path swap is fully superseded by `rewriteM3U8`, which replaces the entire line (host included) rather than just a path segment.

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run 'TestXtreamHlsStreamRewritesManifestHost|TestHlsXtreamStreamRewritesManifestHost' -v`
Expected: PASS (both tests).

- [ ] **Step 5: Commit**

```bash
git add pkg/server/xtream_handlers_stream.go pkg/server/xtream_handlers_stream_test.go
git commit -m "fix: rewrite HLS manifest URIs through the image proxy instead of swapping credentials"
```

---

### Task 9: Wire `rewriteM3U8` into `m3u8ReverseProxy`

**Files:**
- Modify: `pkg/server/proxy_handlers.go`
- Test: `pkg/server/proxy_handlers_test.go`

**Interfaces:**
- Consumes: `c.buildUpstreamRequest`, `writeUpstreamResponse` (Task 6, unused here but kept consistent with the rest of the file), `c.rewriteM3U8` (Task 5).

- [ ] **Step 1: Write the failing test**

Append to `pkg/server/proxy_handlers_test.go` (add `"strings"`, `"github.com/jamesnetherton/m3u"`, and `"github.com/lucasduport/stream-share/pkg/config"` to its imports):

```go
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
```

- [ ] **Step 2: Run test to verify it fails**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestM3U8ReverseProxyRewritesSegmentURIs -v`
Expected: FAIL — body contains the raw `segment1.ts` line, not a rewritten `/img?url=` link.

- [ ] **Step 3: Write the implementation**

In `pkg/server/proxy_handlers.go`, replace `m3u8ReverseProxy`'s body:

```go
// m3u8ReverseProxy forwards HLS index/chunk requests to upstream using Xtream creds.
func (c *Config) m3u8ReverseProxy(ctx *gin.Context) {
	id := ctx.Param("id")
	rpURL, err := url.Parse(strings.ReplaceAll(c.track.URI, path.Base(c.track.URI), id))
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	q := rpURL.Query()
	q.Set("username", c.XtreamUser.String())
	q.Set("password", c.XtreamPassword.String())
	rpURL.RawQuery = q.Encode()

	utils.DebugLog("-> Upstream username: %s", c.XtreamUser.String())
	utils.DebugLog("-> Final upstream URL: %s", rpURL.String())

	req, err := c.buildUpstreamRequest(ctx, rpURL)
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	resp, err := streamHTTPClient.Do(req)
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	body = c.rewriteM3U8(rpURL, body)
	contentType := resp.Header.Get("Content-Type")
	mergeHttpHeader(ctx.Writer.Header(), resp.Header)
	// The rewritten body is a different length than upstream's; overwrite
	// the Content-Length mergeHttpHeader just copied from upstream.
	ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	ctx.Data(resp.StatusCode, contentType, body)
}
```

`pkg/server/proxy_handlers.go` needs `"strconv"` added to its imports for this (`"io"`, `"net/url"`, `"path"`, `"strings"` are already imported).

- [ ] **Step 4: Run test to verify it passes**

Run: `docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -run TestM3U8ReverseProxyRewritesSegmentURIs -v`
Expected: PASS.

Then run the full package test suite and build one more time to confirm everything from Tasks 1–9 is consistent together:

```
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go build -mod=vendor ./...
docker run --rm -v "$PWD":/workspace -w /workspace golang:1.22 go test -mod=vendor ./pkg/server/... -v
```

Expected: build succeeds, all tests PASS.

- [ ] **Step 5: Commit**

```bash
git add pkg/server/proxy_handlers.go pkg/server/proxy_handlers_test.go
git commit -m "fix: rewrite HLS manifest URIs in the non-Xtream m3u8 track proxy"
```

---

## Self-Review

**Spec coverage:**
- Section 1 (generic `/img` route) → Task 7.
- Section 2 (`proxyImageURL` helper) → Task 1.
- Section 3, `marshallInto`/`tvg-logo` → Task 4. `xtreamPlayerAPI`/`rewriteImageFields` + `direct_source` strip → Task 2. `xtreamXMLTV`/`rewriteXMLTVIcons` → Task 3.
- Section 4 (HLS manifest rewriting: `rewriteM3U8`, manifest-aware `assetProxy`, `xtreamHlsStream`/`hlsXtreamStream`, `m3u8ReverseProxy`) → Tasks 5, 7, 8, 9.
- Error handling (malformed/relative URLs left as-is, `/img` errors via existing `stream` error handling, `rewriteXMLTVIcons` fallback, `rewriteM3U8` fallback) → all covered by the implementations in Tasks 1, 3, 5, 7 as written (each returns the original value/bytes on parse failure, matching the spec).
- Testing section → one test per named function, in the task that introduces it.
- Out of scope (host allowlist, `youtube_trailer`, xmltv `<url>`, non-Xtream raw-M3U tag passthrough, xmltv caching) → correctly not implemented; no task touches any of these.

**Placeholder scan:** no TBD/TODO, no "add error handling" hand-waves, no "similar to Task N" — every step has full code.

**Type consistency:** `proxyImageURL(raw string) string` (Task 1) is called identically in Tasks 2–5. `rewriteImageFields(v interface{}) interface{}` (Task 2) matches its use in Task 2's own wiring. `rewriteM3U8(base *url.URL, body []byte) []byte` (Task 5) is called with the same two-argument shape in Tasks 7, 8, 9. `buildUpstreamRequest(ctx *gin.Context, oriURL *url.URL) (*http.Request, error)` and `writeUpstreamResponse(ctx *gin.Context, resp *http.Response)` (Task 6) are used with matching signatures in Task 7; `writeUpstreamResponse` is unused by Tasks 8/9 by design (they need to buffer-and-rewrite unconditionally, not conditionally like `assetProxy`), which is consistent with the spec calling those two call sites out separately from the generic route.
