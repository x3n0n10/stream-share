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
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// streamTransport is shared across all stream() calls so that TCP connections
// are reused across requests and the dialer pool is not recreated every time.
var streamTransport = &http.Transport{
	Proxy: http.ProxyFromEnvironment,
	DialContext: (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext,
	ForceAttemptHTTP2:     false,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   20,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ExpectContinueTimeout: 1 * time.Second,
}

// streamHTTPClient has no global Timeout; streams run as long as the client stays connected.
var streamHTTPClient = &http.Client{Transport: streamTransport}

// getM3U sends the proxified M3U file generated during bootstrap.
func (c *Config) getM3U(ctx *gin.Context) {
	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	ctx.Header("Content-Type", "application/octet-stream")
	ctx.File(c.proxyfiedM3UPath)
}

// reverseProxy forwards a track request to the upstream using Xtream creds.
func (c *Config) reverseProxy(ctx *gin.Context) {
	rpURL, err := url.Parse(c.track.URI)
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	// Always use Xtream creds for upstream query
	q := rpURL.Query()
	q.Set("username", c.XtreamUser.String())
	q.Set("password", c.XtreamPassword.String())
	rpURL.RawQuery = q.Encode()

	utils.DebugLog("-> Upstream username: %s", c.XtreamUser.String())
	utils.DebugLog("-> Final upstream URL: %s", rpURL.String())

	c.stream(ctx, rpURL)
}

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
