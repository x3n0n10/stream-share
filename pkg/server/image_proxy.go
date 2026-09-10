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
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
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
	base := target
	if resp.Request != nil && resp.Request.URL != nil {
		base = resp.Request.URL
	}
	body = c.rewriteM3U8(base, body)
	mergeHttpHeader(ctx.Writer.Header(), resp.Header)
	// The rewritten body is a different length than upstream's; overwrite
	// the Content-Length mergeHttpHeader just copied from upstream, or the
	// client gets a length that doesn't match the actual body.
	ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(body)))
	ctx.Data(resp.StatusCode, contentType, body)
}

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
