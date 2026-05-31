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
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

func (c *Config) xtreamApiGet(ctx *gin.Context) {
	const (
		apiGet = "apiget"
	)

	var (
		extension = ctx.Query("output")
		cacheName = apiGet + extension
	)

	xtreamM3uCacheLock.RLock()
	meta, ok := xtreamM3uCache[cacheName]
	d := time.Since(meta.Time)
	if !ok || d.Hours() >= float64(c.M3UCacheExpiration) {
		log.Printf("[stream-share] %v | %s | xtream cache API m3u file\n", time.Now().Format("2006/01/02 - 15:04:05"), ctx.ClientIP())
		xtreamM3uCacheLock.RUnlock()
		playlist, err := c.xtreamGenerateM3u(ctx, extension)
		if err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)) // nolint: errcheck
			return
		}
		if err := c.cacheXtreamM3u(playlist, cacheName); err != nil {
			ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err)) // nolint: errcheck
			return
		}
	} else {
		xtreamM3uCacheLock.RUnlock()
	}

	ctx.Header("Content-Disposition", fmt.Sprintf(`attachment; filename=%q`, c.M3UFileName))
	xtreamM3uCacheLock.RLock()
	path := xtreamM3uCache[cacheName].string
	xtreamM3uCacheLock.RUnlock()
	ctx.Header("Content-Type", "application/octet-stream")

	ctx.File(path)

}

// xtreamStream proxies a live stream through the session manager, which shares a
// single upstream connection across all viewers of the same channel and records
// the session so Discord/status endpoints see the active viewer. With a single
// viewer the multiplexed path applies full back-pressure and behaves like a
// direct proxy; additional viewers reuse the same upstream connection.
func (c *Config) xtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("-> Xtream streaming request: %s", ctx.Request.URL.Path)
	utils.DebugLog("-> Proxying to Xtream upstream: %s", oriURL.String())
	c.multiplexedStream(ctx, oriURL)
}

func (c *Config) xtreamXMLTV(ctx *gin.Context) {
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, ctx.Request.UserAgent())
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	resp, err := client.GetXMLTV()
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	ctx.Data(http.StatusOK, "application/xml", resp)
}

func (c *Config) xtreamStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamLive(ctx *gin.Context) {
	id := ctx.Param("id")
	rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamPlay(ctx *gin.Context) {
	token := ctx.Param("token")
	t := ctx.Param("type")
	rpURL, err := url.Parse(fmt.Sprintf("%s/play/%s/%s", c.XtreamBaseURL, token, t))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamTimeshift(ctx *gin.Context) {
	duration := ctx.Param("duration")
	start := ctx.Param("start")
	id := ctx.Param("id")
	utils.DebugLog("Timeshift request: duration=%s, start=%s, id=%s", duration, start, id)

	// Use the upstream provider when catchup is disabled or the channel has native support.
	if c.catchupManager == nil || !c.catchupManager.IsEnabled() || c.catchupManager.HasUpstreamCatchup(id) {
		rpURL, err := url.Parse(fmt.Sprintf("%s/timeshift/%s/%s/%s/%s/%s",
			c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, duration, start, id))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		c.stream(ctx, rpURL)
		return
	}

	startUnix, err := strconv.ParseInt(start, 10, 64)
	var requestedTime time.Time
	if err != nil {
		// TiviMate uses YYYY-MM-DD:HH-MM instead of a Unix timestamp.
		t, parseErr := time.Parse("2006-01-02:15-04", start)
		if parseErr != nil {
			utils.ErrorLog("Timeshift: invalid start timestamp %q (tried Unix and YYYY-MM-DD:HH-MM): %v", start, err)
			ctx.AbortWithStatus(http.StatusBadRequest)
			return
		}
		requestedTime = t
	} else {
		requestedTime = time.Unix(startUnix, 0)
	}

	buf := c.catchupManager.GetBuffer(id)
	if buf == nil {
		// Channel not currently buffered (never watched in this session).
		utils.InfoLog("Timeshift: no local buffer for stream %s (channel not watched)", id)
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}

	offset := buf.OffsetForTime(requestedTime)
	utils.DebugLog("Timeshift: serving stream %s from local buffer at byte offset %d", id, offset)
	c.serveFromCatchupBuffer(ctx, buf, offset)
}

// serveFromCatchupBuffer streams a live buffer file to the client starting at startOffset.
// When the reader catches up to the current write position and the stream is still live,
// it polls until more data arrives. When the stream has stopped, it returns after the
// last byte to signal end-of-stream to the player.
func (c *Config) serveFromCatchupBuffer(ctx *gin.Context, buf *catchup.DiskBuffer, startOffset int64) {
	f, err := os.Open(buf.FilePath())
	if err != nil {
		utils.ErrorLog("Catchup serve: failed to open buffer file: %v", err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		utils.ErrorLog("Catchup serve: failed to seek to offset %d: %v", startOffset, err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.Header("Content-Type", "video/mp2t")
	ctx.Header("Cache-Control", "no-cache")
	ctx.Status(http.StatusOK)

	readBuf := make([]byte, 64*1024)
	clientGone := ctx.Request.Context().Done()

	for {
		select {
		case <-clientGone:
			return
		default:
		}

		n, rerr := f.Read(readBuf)
		if n > 0 {
			if _, werr := ctx.Writer.Write(readBuf[:n]); werr != nil {
				return // client disconnected
			}
			if fl, ok := ctx.Writer.(http.Flusher); ok {
				fl.Flush()
			}
		}
		if rerr == io.EOF {
			if buf.IsStopped() {
				// No more data will ever be written; we're done.
				return
			}
			// The live stream is still running; wait briefly for more data.
			select {
			case <-clientGone:
				return
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		if rerr != nil {
			utils.DebugLog("Catchup serve: read error: %v", rerr)
			return
		}
	}
}

func (c *Config) xtreamStreamMovieWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	// Normalize DB key: cached entries are stored by bare stream_id without extension
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if c.sessionManager != nil {
		username := ctx.GetString("username")
		if username == "" {
			username = ctx.Param("username")
		}
		if username != "" {
			movieTitle := idRaw
			if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
				movieTitle = name
			}
			c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			// If file exists and is ready, serve locally; if DB status is stale, serve as complete.
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				_ = c.db.TouchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached movie for %s from %s", idRaw, entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", idRaw)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Not cached yet: auto-start 7-day caching in background
		// Use the extension from the request if present; only fall back to the
		// M3U cache lookup and then a hardcoded default when the client omitted it.
		basePath := "movie"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mp4"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "stream-share-cache")
		}
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		// Insert pending entry
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel() // another goroutine owns this download; discard our cancel
		} else {
			go func() {
				defer c.inProgressDownloads.Delete(idRaw)
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly: lets the IPTV server handle Content-Length, Content-Range,
		// and Range seeks natively. This avoids avformat errors (MP4 moov at EOF) and seek loops.
		// Background caching continues; once complete, future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		downloadCancel() // viewer disconnected; free the provider connection
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamMovie(ctx *gin.Context) {
	if utils.GetEnvOrDefault("USE_VOD_CACHING", "false") == "true" {
		c.xtreamStreamMovieWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.xtreamStream(ctx, rpURL)
	}
}

func (c *Config) xtreamStreamSeriesWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	if c.sessionManager != nil {
		username := ctx.GetString("username")
		if username == "" {
			username = ctx.Param("username")
		}
		if username != "" {
			seriesTitle := idRaw
			if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
				seriesTitle = name
			}
			c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				_ = c.db.TouchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached episode for %s from %s", idRaw, entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", idRaw)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Not cached yet: auto-start 7-day caching in background
		basePath := "series"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mkv"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "stream-share-cache")
		}
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				defer c.inProgressDownloads.Delete(idRaw)
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues; once complete, future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		downloadCancel() // viewer disconnected; free the provider connection
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamSeries(ctx *gin.Context) {
	if utils.GetEnvOrDefault("USE_VOD_CACHING", "false") == "true" {
		c.xtreamStreamSeriesWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		c.xtreamStream(ctx, rpURL)
	}
}

// Direct handlers using proxy credentials
func (c *Config) xtreamProxyCredentialsStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	utils.DebugLog("Direct stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	rpURL, err := url.Parse(fmt.Sprintf("%s/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsLiveStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	utils.DebugLog("Direct live stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	rpURL, err := url.Parse(fmt.Sprintf("%s/live/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsMovieStreamHandler(ctx *gin.Context) {
	if utils.GetEnvOrDefault("USE_VOD_CACHING", "false") == "true" {
		c.xtreamProxyCredentialsMovieStreamHandlerWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			utils.ErrorLog("Failed to parse upstream URL: %v", err)
			ctx.AbortWithStatus(500)
			return
		}
		utils.DebugLog("Movie streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.stream(ctx, rpURL)
	}
}

func (c *Config) xtreamProxyCredentialsMovieStreamHandlerWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Direct movie stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	if c.sessionManager != nil {
		username := ctx.GetString("username")
		if username == "" {
			username = ctx.Param("username")
		}
		if username != "" {
			movieTitle := idRaw
			if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
				movieTitle = name
			}
			c.sessionManager.RegisterVODView(username, idRaw, "movie", movieTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				_ = c.db.TouchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached movie (proxy creds path) for %s from %s", idRaw, entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", idRaw)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		// Auto-start caching
		basePath := "movie"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mp4"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "stream-share-cache")
		}
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "movie", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record movie cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				defer c.inProgressDownloads.Delete(idRaw)
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues; once complete, future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		downloadCancel() // viewer disconnected; free the provider connection
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/movie/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.stream(ctx, rpURL)
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandler(ctx *gin.Context) {
	if utils.GetEnvOrDefault("USE_VOD_CACHING", "false") == "true" {
		c.xtreamProxyCredentialsSeriesStreamHandlerWithCache(ctx)
	} else {
		id := ctx.Param("id")
		rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
		if err != nil {
			utils.ErrorLog("Failed to parse upstream URL: %v", err)
			ctx.AbortWithStatus(500)
			return
		}
		utils.DebugLog("Series streaming request - using Xtream credentials for upstream: %s", rpURL.String())
		c.stream(ctx, rpURL)
	}
}

func (c *Config) xtreamProxyCredentialsSeriesStreamHandlerWithCache(ctx *gin.Context) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	// Reject IDs containing path separators or dot-dot to prevent path traversal.
	if strings.Contains(idRaw, "/") || strings.Contains(idRaw, "..") {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Direct series stream request with proxy credentials: username=%s, id=%s", ctx.Param("username"), id)
	if c.sessionManager != nil {
		username := ctx.GetString("username")
		if username == "" {
			username = ctx.Param("username")
		}
		if username != "" {
			seriesTitle := idRaw
			if name, ok := c.getChannelNameByID(idRaw); ok && strings.TrimSpace(name) != "" {
				seriesTitle = name
			}
			c.sessionManager.RegisterVODView(username, idRaw, "series", seriesTitle)
			defer c.sessionManager.UnregisterVODView(username, idRaw)
		}
	}
	if c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				var ct string
				if ext := strings.ToLower(path.Ext(entry.FilePath)); ext == ".ts" {
					ct = "video/mp2t"
				} else if ext == ".mkv" {
					ct = "video/x-matroska"
				} else {
					ct = "video/mp4"
				}
				_ = c.db.TouchVODCache(idRaw)
				if strings.ToLower(entry.Status) == "ready" {
					utils.InfoLog("Serving cached episode (proxy creds path) for %s from %s", idRaw, entry.FilePath)
					serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
					return
				}
				// Final file exists but DB status is stale (rename completed before DB update).
				utils.InfoLog("Serving complete cached file for %s (DB status pending update)", idRaw)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}
		basePath := "series"
		resolvedExt := path.Ext(id)
		if resolvedExt == "" {
			resolvedExt = c.findVODExtensionInCache(basePath, idRaw)
		}
		finalID := idRaw
		if resolvedExt == "" {
			resolvedExt = ".mkv"
		}
		finalID += resolvedExt
		upstream := fmt.Sprintf("%s/%s/%s/%s/%s", c.XtreamBaseURL, basePath, c.XtreamUser, c.XtreamPassword, finalID)
		cacheDir := strings.TrimSpace(os.Getenv("CACHE_FOLDER"))
		if cacheDir == "" {
			cacheDir = filepath.Join(os.TempDir(), "stream-share-cache")
		}
		_ = os.MkdirAll(cacheDir, 0o755)
		dest := filepath.Join(cacheDir, idRaw+resolvedExt)
		expires := time.Now().Add(7 * 24 * time.Hour)
		if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: "series", FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
			utils.ErrorLog("Failed to record series cache entry for %s: %v", idRaw, err)
		}
		downloadCtx, downloadCancel := context.WithCancel(context.Background())
		if _, loaded := c.inProgressDownloads.LoadOrStore(idRaw, downloadCancel); loaded {
			downloadCancel()
		} else {
			go func() {
				defer c.inProgressDownloads.Delete(idRaw)
				c.fetchToFile(downloadCtx, upstream, dest, idRaw, expires)
			}()
		}
		// Proxy to upstream directly for proper headers and native Range-seek support.
		// Background caching continues; once complete, future requests serve from the local file.
		upstreamURL, upstreamErr := url.Parse(upstream)
		if upstreamErr != nil {
			downloadCancel()
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(upstreamErr))
			return
		}
		c.stream(ctx, upstreamURL)
		downloadCancel() // viewer disconnected; free the provider connection
		return
	}
	rpURL, err := url.Parse(fmt.Sprintf("%s/series/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, id))
	if err != nil {
		utils.ErrorLog("Failed to parse upstream URL: %v", err)
		ctx.AbortWithStatus(500)
		return
	}
	c.stream(ctx, rpURL)
}

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

	req, reqErr := http.NewRequestWithContext(ctx.Request.Context(), "GET", fmt.Sprintf("%s://%s/hls/%s/%s", redirURL.Scheme, redirURL.Host, ctx.Param("token"), ctx.Param("chunk")), nil)
	if reqErr != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(reqErr))
		return
	}

	mergeHttpHeader(req.Header, ctx.Request.Header)

	resp, doErr := http.DefaultClient.Do(req)
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
			hlsResp, hlsDoErr := http.DefaultClient.Do(hlsReq)
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
			body := string(b)
			body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
			utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
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
			body := string(b)
			body = strings.ReplaceAll(body, "/"+c.XtreamUser.String()+"/"+c.XtreamPassword.String()+"/", "/"+c.User.String()+"/"+c.Password.String()+"/")
			utils.DebugLog("HLS stream response modified to use proxy credentials for client URLs")
			mergeHttpHeader(ctx.Writer.Header(), hlsResp.Header)
			ctx.Data(http.StatusOK, hlsResp.Header.Get("Content-Type"), []byte(body))
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
