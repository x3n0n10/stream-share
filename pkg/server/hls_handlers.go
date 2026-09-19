/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
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
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// HLS helpers and handlers
var hlsChannelsRedirectURL map[string]url.URL = map[string]url.URL{}
var hlsChannelsRedirectURLLock = sync.RWMutex{}

func (c *Config) xtreamHlsStream(ctx *gin.Context) {
	chunk := ctx.Param("chunk")
	s := strings.Split(chunk, "_")
	if len(s) != 2 {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("HSL malformed chunk")))
		return
	}
	channel := s[0]

	redirURL, err := getHlsRedirectURL(channel)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}

	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", fmt.Sprintf("%s://%s/hls/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), ctx.Param("chunk")), nil)
	if reqErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr))
		return
	}

	mergeHttpHeader(req.Header, ctx.Request.Header)

	resp, doErr := client.Do(req)
	if doErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusFound {
		loc, locErr := resp.Location()
		if locErr != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr))
			return
		}
		id := ctx.Param("id")
		if strings.Contains(loc.String(), id) {
			hlsChannelsRedirectURLLock.Lock()
			hlsChannelsRedirectURL[id] = *loc
			hlsChannelsRedirectURLLock.Unlock()
			hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
			if hlsReqErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr))
				return
			}
			mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
			hlsResp, hlsDoErr := client.Do(hlsReq)
			if hlsDoErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr))
				return
			}
			defer func() { _ = hlsResp.Body.Close() }()

			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			b = c.rewriteM3U8(ctx, loc, b)
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			// The rewritten body is a different length than upstream's; overwrite
			// the Content-Length mergeHttpHeader just copied from upstream.
			ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(b)))
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), b)
			return
		}
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("unable to HLS stream")))
		return
	}

	utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
	ctx.Status(resp.StatusCode)
}

func (c *Config) hlsXtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("HLS stream request with URL: %s", oriURL.String())
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", oriURL.String(), nil)
	if reqErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr))
		return
	}
	mergeHttpHeader(req.Header, ctx.Request.Header)
	resp, doErr := client.Do(req)
	if doErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(doErr))
		return
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusFound {
		loc, locErr := resp.Location()
		if locErr != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(locErr))
			return
		}
		id := ctx.Param("id")
		if strings.Contains(loc.String(), id) {
			hlsChannelsRedirectURLLock.Lock()
			hlsChannelsRedirectURL[id] = *loc
			hlsChannelsRedirectURLLock.Unlock()
			hlsReq, hlsReqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", loc.String(), nil)
			if hlsReqErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsReqErr))
				return
			}
			mergeHttpHeader(hlsReq.Header, ctx.Request.Header)
			hlsResp, hlsDoErr := client.Do(hlsReq)
			if hlsDoErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(hlsDoErr))
				return
			}
			defer func() { _ = hlsResp.Body.Close() }()

			b, readErr := io.ReadAll(hlsResp.Body)
			if readErr != nil {
				_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(readErr))
				return
			}
			b = c.rewriteM3U8(ctx, loc, b)
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			// The rewritten body is a different length than upstream's; overwrite
			// the Content-Length mergeHttpHeader just copied from upstream.
			ctx.Writer.Header().Set("Content-Length", strconv.Itoa(len(b)))
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), b)
			return
		}
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(errors.New("unable to HLS stream")))
		return
	}

	utils.DebugLog("HLS stream response status: %d", resp.StatusCode)
	ctx.Status(resp.StatusCode)
}

func (c *Config) xtreamHlsrStream(ctx *gin.Context) {
	channel := ctx.Param("channel")
	redirURL, err := getHlsRedirectURL(channel)
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	nextURL, parseErr := url.Parse(fmt.Sprintf("%s://%s/hlsr/%s/%s/%s/%s/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), c.XtreamUser, c.XtreamPassword, ctx.Param("channel"), ctx.Param("hash"), ctx.Param("chunk")))
	if parseErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(parseErr))
		return
	}
	c.hlsXtreamStream(ctx, nextURL)
}

// Restore helper used by HLS handlers
func getHlsRedirectURL(channel string) (*url.URL, error) {
	hlsChannelsRedirectURLLock.RLock()
	defer hlsChannelsRedirectURLLock.RUnlock()
	u, ok := hlsChannelsRedirectURL[channel+".m3u8"]
	if !ok {
		return nil, utils.PrintErrorAndReturn(errors.New("HSL redirect url not found"))
	}
	return &u, nil
}
