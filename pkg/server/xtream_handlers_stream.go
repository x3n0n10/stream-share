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
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
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

	xtreamM3uCacheLock.RLock()
	path := xtreamM3uCache[cacheName].string
	xtreamM3uCacheLock.RUnlock()
	c.streamPlaylist(ctx, path)
}

// xtreamStream proxies a live stream through the session manager, which shares a
// single upstream connection across all viewers of the same channel and records
// the session so Discord/status endpoints see the active viewer. With a single
// viewer the multiplexed path applies full back-pressure and behaves like a
// direct proxy; additional viewers reuse the same upstream connection.
func (c *Config) xtreamStream(ctx *gin.Context, oriURL *url.URL) {
	utils.DebugLog("-> Xtream streaming request: %s", c.redactRequestURI(ctx.Request.URL, ctx.FullPath()))
	utils.DebugLog("-> Proxying to Xtream upstream: %s", utils.MaskURL(oriURL.String()))
	c.multiplexedStream(ctx, oriURL)
}

// rejectFilteredLive answers 404 for a live channel the catalogue filter hides
// from players, so it cannot be played by id either. It only knows ids from
// the cached live catalogue and lets unknown ones through.
func (c *Config) rejectFilteredLive(ctx *gin.Context, id string) bool {
	if c.catalogFilter == nil || !c.liveStreamFiltered(id) {
		return false
	}
	ctx.AbortWithStatus(http.StatusNotFound)
	return true
}

func (c *Config) xtreamStreamHandler(ctx *gin.Context) {
	id := ctx.Param("id")
	if c.rejectFilteredLive(ctx, id) {
		return
	}
	rpURL, err := url.Parse(c.providerStreamURL("", id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	c.xtreamStream(ctx, rpURL)
}

func (c *Config) xtreamStreamLive(ctx *gin.Context) {
	id := ctx.Param("id")
	if c.rejectFilteredLive(ctx, id) {
		return
	}
	rpURL, err := url.Parse(c.providerStreamURL("live", id))
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
	idRaw := ctx.Param("id")

	// Proxy upstream if catchup is disabled or the channel has native upstream support.
	if c.catchupManager == nil || !c.catchupManager.IsEnabled() || c.catchupManager.HasUpstreamCatchup(idRaw) {
		rpURL, err := url.Parse(c.providerStreamURL("timeshift", duration, start, idRaw))
		if err != nil {
			_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
			return
		}
		c.stream(ctx, rpURL)
		return
	}

	startTime, err := parseTimeshiftStart(start)
	if err != nil {
		utils.WarnLog("Catchup: unparseable start param %q: %v", start, err)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	utils.DebugLog("Catchup: timeshift start raw=%q parsed=%s (unix=%d)", start, startTime.Format(time.RFC3339), startTime.Unix())

	// A client is actively reading the catchup buffer (e.g. resuming after a
	// pause) — cancel any pending pause-grace stop so recording continues
	// uninterrupted and there's no gap once the buffer catches up to live.
	if c.sessionManager != nil {
		c.sessionManager.NotifyCatchupActivity(idRaw)
	}

	buf := c.catchupManager.GetBuffer(idRaw)
	if buf == nil {
		utils.DebugLog("Catchup: no buffer for %s (never watched or already cleaned up)", c.streamLabel(idRaw))
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	utils.DebugLog("Catchup: buffer for %s started at %s (unix=%d), buffered=%d bytes", c.streamLabel(idRaw), buf.StartTime().Format(time.RFC3339), buf.StartTime().Unix(), buf.BytesBuffered())

	// Set response headers before the first write so they reach the client.
	ctx.Header("Content-Type", "video/mp2t")
	ctx.Header("Cache-Control", "no-cache")

	offset := buf.OffsetForTime(startTime)
	utils.DebugLog("Catchup: serving %s from local buffer at offset %d / %d bytes (start raw=%q parsed=%s)", c.streamLabel(idRaw), offset, buf.BytesBuffered(), start, startTime.Format(time.RFC3339))
	c.serveFromCatchupBuffer(ctx, buf, startTime)

	// If the client disconnected or the handler aborted, we're done.
	select {
	case <-ctx.Request.Context().Done():
		return
	default:
	}
	if ctx.IsAborted() {
		return
	}

	// Buffer exhausted — transition seamlessly to the live upstream so TiviMate
	// catches up and continues watching without reconnecting on its own.
	liveURL, err := url.Parse(c.providerStreamURL("live", idRaw))
	if err != nil {
		return
	}
	utils.DebugLog("Catchup: buffer exhausted for %s, transitioning to live upstream", c.streamLabel(idRaw))
	c.stream(ctx, liveURL)
}

// parseTimeshiftStart parses a timeshift start parameter.
// TiviMate sends "YYYY-MM-DD:HH-MM" in local time; time.Local is used, which Go
// initialises from the TZ environment variable at startup. Set TZ in the container
// (e.g. TZ=Europe/Amsterdam) so that local timestamps are interpreted correctly.
// Unix timestamp integers are also accepted and are always timezone-independent.
func parseTimeshiftStart(start string) (time.Time, error) {
	if n, err := strconv.ParseInt(start, 10, 64); err == nil {
		return time.Unix(n, 0), nil
	}
	return time.ParseInLocation("2006-01-02:15-04", start, time.Local)
}

// alignToTSPacket seeks f to startOffset then scans forward (up to 3 packet-widths) for
// two consecutive MPEG-TS sync bytes (0x47) exactly 188 bytes apart, confirming a packet
// boundary. f is left positioned at the aligned offset. If no alignment is found, f is
// repositioned at startOffset.
func alignToTSPacket(f *os.File, startOffset int64) {
	const syncByte = byte(0x47)
	const pktSize = 188

	if _, err := f.Seek(startOffset, io.SeekStart); err != nil {
		return
	}
	search := make([]byte, pktSize*3)
	n, _ := f.Read(search)
	search = search[:n]
	for i := 0; i+pktSize < len(search); i++ {
		if search[i] == syncByte && search[i+pktSize] == syncByte {
			_, _ = f.Seek(startOffset+int64(i), io.SeekStart)
			return
		}
	}
	// No boundary found; restore original position.
	_, _ = f.Seek(startOffset, io.SeekStart)
}

// serveFromCatchupBuffer serves a timeshift request from the local disk buffer.
// It resolves the correct file for startTime (which may be the previous file after a
// rotation), serves that file to completion, then seamlessly continues from the current
// file when necessary — so rewinds that span a rotation boundary play through without
// interruption.
func (c *Config) serveFromCatchupBuffer(ctx *gin.Context, buf *catchup.DiskBuffer, startTime time.Time) {
	filePath, startOffset := buf.FileForTime(startTime)
	currentPath := buf.FilePath()

	if filePath != currentPath {
		// Serving from the previous (fully-written) file. A closed drainDone signals
		// "no more data to wait for" — the prev file is complete.
		closedCh := make(chan struct{})
		close(closedCh)
		c.streamFileSegment(ctx, filePath, startOffset, closedCh)
		if ctx.Request.Context().Err() != nil || ctx.IsAborted() {
			return
		}
		// Seamlessly continue from the beginning of the current file.
		filePath = buf.FilePath() // re-read in case another rotation happened
		startOffset = 0
	}

	c.streamFileSegment(ctx, filePath, startOffset, buf.DrainDone())
}

// streamFileSegment serves bytes from filePath starting at startOffset, flushing to the
// client as they arrive. It blocks until drainDone closes (all data written), polling
// every 200 ms when EOF is hit before that point.
func (c *Config) streamFileSegment(ctx *gin.Context, filePath string, startOffset int64, drainDone <-chan struct{}) {
	f, err := os.Open(filePath)
	if err != nil {
		utils.ErrorLog("Catchup: failed to open buffer file %s: %v", filePath, err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	alignToTSPacket(f, startOffset)

	readBuf := make([]byte, 64*1024)
	clientGone := ctx.Request.Context().Done()
	pollTimer := time.NewTimer(200 * time.Millisecond)
	defer pollTimer.Stop()
	for {
		select {
		case <-clientGone:
			return
		default:
		}
		n, rerr := f.Read(readBuf)
		if n > 0 {
			if _, werr := ctx.Writer.Write(readBuf[:n]); werr != nil {
				return
			}
			if flusher, ok := ctx.Writer.(http.Flusher); ok {
				flusher.Flush()
			}
		}
		if rerr == io.EOF {
			if !pollTimer.Stop() {
				select {
				case <-pollTimer.C:
				default:
				}
			}
			pollTimer.Reset(200 * time.Millisecond)
			select {
			case <-drainDone:
				// All writes are on disk. Flush any bytes written between our last
				// read and drain completion, then exit.
				drainDone = nil // nil channel blocks forever — prevent re-entry
				for {
					n2, err2 := f.Read(readBuf)
					if n2 > 0 {
						if _, werr := ctx.Writer.Write(readBuf[:n2]); werr != nil {
							return
						}
						if flusher, ok := ctx.Writer.(http.Flusher); ok {
							flusher.Flush()
						}
					}
					if err2 != nil {
						return
					}
				}
			case <-clientGone:
				return
			case <-pollTimer.C:
			}
			continue
		}
		if rerr != nil {
			return
		}
	}
}

// streamVODWithCache serves a movie or series request, transparently caching
// it to local disk on first access. When a ready cache entry exists it serves
// the local file; otherwise it starts (or joins) the background download and
// serves the player from the file as it is written, so the download is the
// only provider connection for it (see serveVODDownload). When caching is
// disabled or no database is configured the request is proxied directly to
// upstream via c.stream.
//
// A synthetic VOD view is always registered with the session manager so that
// /status and watch history reflect the playback regardless of the serving
// path (cache hit, cache-miss proxy, or disabled-cache proxy).
//
// basePath is "movie" or "series"; defaultExt is the extension used when none
// can be resolved from the request or the M3U catalogue (".mp4" for movies,
// ".mkv" for series).
func (c *Config) streamVODWithCache(ctx *gin.Context, basePath, defaultExt string) {
	id := ctx.Param("id")
	idRaw := strings.TrimSuffix(id, path.Ext(id))
	if !isValidStreamID(idRaw) {
		utils.ErrorLog("Rejected stream ID with path traversal characters: %q", idRaw)
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	defer c.registerVODView(c.resolveRequestUsername(ctx), idRaw, basePath, idRaw)()

	if c.VODCacheEnabled && c.db != nil {
		if entry, err := c.db.GetVODCache(idRaw); err == nil && entry != nil {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				ct := contentTypeForPath(entry.FilePath)
				c.touchVODCache(idRaw)
				// Only the opening read gets an INFO line; seeks are DEBUG.
				logServe := utils.DebugLog
				if rng := ctx.GetHeader("Range"); rng == "" || strings.HasPrefix(rng, "bytes=0-") {
					logServe = utils.InfoLog
				}
				logServe("Serving cached %s for %s from %s", basePath, c.vodLabel(idRaw), entry.FilePath)
				serveLocalFileRange(ctx, entry.FilePath, ct, "", false)
				return
			}
		}

		// Not cached yet: auto-start 7-day caching in the background (or join
		// the download already running) and serve the player from it. A
		// second, direct provider connection for the same file would make a
		// one-connection provider cut the download, and vice versa.
		upstream, dest, _ := c.resolveVODTarget(basePath, defaultExt, id, false)
		expires := time.Now().Add(7 * 24 * time.Hour)
		dl := c.startBackgroundDownload(upstream, dest, idRaw, basePath, expires, func() {
			if err := c.db.UpsertVODCache(&types.VODCacheEntry{StreamID: idRaw, Type: basePath, FilePath: dest, Status: "downloading", ExpiresAt: expires, CreatedAt: time.Now()}); err != nil {
				utils.ErrorLog("Failed to record %s cache entry for %s: %v", basePath, idRaw, err)
			}
		})
		c.serveVODDownload(ctx, dl, vodCachedLabel(idRaw), "", false)
		return
	}

	// Caching disabled or no database: proxy directly to upstream. VOD is
	// seekable and each viewer may be at a different position, so the direct
	// proxy (c.stream) is used instead of multiplexing — sharing a single
	// linear upstream connection across viewers does not work for VOD.
	rpURL, err := url.Parse(c.providerStreamURL(basePath, id))
	if err != nil {
		_ = ctx.AbortWithError(http.StatusInternalServerError, utils.PrintErrorAndReturn(err))
		return
	}
	utils.DebugLog("VOD %s streaming request - proxying to upstream: %s", basePath, utils.MaskURL(rpURL.String()))
	c.stream(ctx, rpURL)
}

func (c *Config) xtreamStreamMovie(ctx *gin.Context) {
	c.streamVODWithCache(ctx, "movie", ".mp4")
}

func (c *Config) xtreamStreamSeries(ctx *gin.Context) {
	c.streamVODWithCache(ctx, "series", ".mkv")
}

// classifyStreamType infers the Xtream stream type ("movie", "series", "live",
// "timeshift") from the segments of a URL path, returning fallback when none of
// the known segments are present.
func classifyStreamType(p, fallback string) string {
	switch {
	case strings.Contains(p, "/movie/"):
		return "movie"
	case strings.Contains(p, "/series/"):
		return "series"
	case strings.Contains(p, "/live/"):
		return "live"
	case strings.Contains(p, "/timeshift/"):
		return "timeshift"
	default:
		return fallback
	}
}

// multiplexedStream proxies a stream while sharing a single upstream connection
// across multiple clients for the same content using the SessionManager.
func (c *Config) multiplexedStream(ctx *gin.Context, targetURL *url.URL) {
	username := c.resolveRequestUsername(ctx)

	// Extract stream ID and type
	streamID := path.Base(targetURL.Path)
	// Normalize stream id for cache lookup (strip extension if present)
	streamIDRaw := strings.TrimSuffix(streamID, path.Ext(streamID))
	streamType := classifyStreamType(targetURL.Path, "unknown")
	// Fallback: check the incoming request path for type hints. The generic
	// /:user/:pass/:id route maps to live streams in the Xtream protocol.
	if streamType == "unknown" {
		streamType = classifyStreamType(ctx.Request.URL.Path, "live")
	}

	// Title from query parameter, name resolution (live index or lazy VOD
	// get_vod_info), or fallback to stream ID. Resolving here — before
	// RequestStream takes streamLock — both stores the title on the session and
	// warms the VOD cache so later locked log lookups resolve without network I/O.
	streamTitle := targetURL.Query().Get("title")
	if streamTitle == "" {
		if name, ok := c.resolveTitleAtStart(streamIDRaw, streamType); ok && strings.TrimSpace(name) != "" {
			streamTitle = name
		} else {
			streamTitle = streamID
		}
	}

	utils.DebugLog("Multiplexed stream request: user=%s, id=%s, type=%s, title=%s, upstream=%s",
		username, streamID, streamType, streamTitle, targetURL.String())

	if c.serveCachedVOD(ctx, username, streamIDRaw, streamType, streamTitle) {
		return
	}

	if c.sessionManager == nil {
		utils.ErrorLog("Multiplex: sessionManager is NIL, falling back to direct streaming")
		c.stream(ctx, targetURL)
		return
	}

	label := c.streamLabel(streamID)
	dataChan, doneChan, clientKey, ok := c.joinStreamSession(ctx, username, streamID, streamType, streamTitle, targetURL, label)
	if !ok {
		return
	}

	// Set content-type and disable intermediary buffering
	setNoBufferingHeaders(ctx, contentTypeForPath(targetURL.Path))

	// Stream data to the client
	utils.DebugLog("Starting multiplexed stream for user %s (%s)", username, label)
	relayToClient(ctx, dataChan, doneChan, username, label)

	// Clean up after streaming is done
	utils.DebugLog("Stream ended for user %s (%s)", username, label)
	c.sessionManager.RemoveClient(streamID, username, clientKey)
}

// serveCachedVOD serves a VOD from disk when it is cached locally, to avoid an
// upstream connection, and reports whether it did. It registers a synthetic
// VOD view so /status and watch history reflect this playback — without it,
// cached VOD served through the multiplexed path (e.g. the generic
// /:user/:pass/:id route) would be invisible.
func (c *Config) serveCachedVOD(ctx *gin.Context, username, streamIDRaw, streamType, streamTitle string) bool {
	if c.db != nil && (streamType == "movie" || streamType == "series") {
		if entry, err := c.db.GetVODCache(streamIDRaw); err == nil && entry != nil && entry.Status == "ready" {
			if fi, statErr := os.Stat(entry.FilePath); statErr == nil && !fi.IsDir() {
				utils.InfoLog("Multiplex: serving cached %s for %s from %s", streamType, c.streamLabel(streamIDRaw), entry.FilePath)
				defer c.registerVODView(username, streamIDRaw, streamType, streamTitle)()
				c.touchVODCache(streamIDRaw)
				serveLocalFileRange(ctx, entry.FilePath, contentTypeForPath(entry.FilePath), "", false)
				return true
			}
			utils.WarnLog("Multiplex: cached %s missing on disk for %s at %s; falling back to upstream", streamType, c.streamLabel(streamIDRaw), entry.FilePath)
		}
	}
	return false
}

// joinStreamSession requests the stream through the session manager for
// multiplexing and returns this client's data and done channels once the
// upstream is up. ok is false when the request has already been answered (and
// the client removed from the session).
func (c *Config) joinStreamSession(ctx *gin.Context, username, streamID, streamType, streamTitle string, targetURL *url.URL, label string) (dataChan chan []byte, doneChan <-chan struct{}, clientKey string, ok bool) {
	// clientKey identifies this connection: one user can hold several (e.g.
	// two devices on the same account), so every later call names its own.
	buffer, clientKey, err := c.sessionManager.RequestStream(username, streamID, streamType, streamTitle, targetURL)
	if err != nil {
		utils.ErrorLog("Multiplex: RequestStream failed for user=%s %s err=%v", username, label, err)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return nil, nil, "", false
	}
	if buffer == nil {
		utils.WarnLog("Multiplex: buffer returned is NIL for %s (user=%s)", label, username)
	}

	// Get the data channel and termination signal for this client
	dataChan, exists := c.sessionManager.GetClientChannel(streamID, clientKey)
	if !exists {
		utils.ErrorLog("Failed to get client channel for user=%s, %s", username, label)
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return nil, nil, "", false
	}
	doneChan, _ = c.sessionManager.GetClientDone(streamID, clientKey)

	if buffer != nil && !c.awaitUpstreamStart(ctx, buffer, username, streamID, clientKey, label) {
		return nil, nil, "", false
	}
	return dataChan, doneChan, clientKey, true
}

// awaitUpstreamStart waits until the pump knows whether the upstream actually
// came up, before we commit to a 200. Historically RequestStream returned
// before the provider was ever contacted, so a 403 or dial timeout could only
// ever surface as an unexplained mid-body disconnect. A co-viewer joining a
// healthy stream sees an already-closed channel here and does not wait. It
// returns false when the request has been answered and the client removed.
func (c *Config) awaitUpstreamStart(ctx *gin.Context, buffer *session.StreamBuffer, username, streamID, clientKey, label string) bool {
	clientGone := ctx.Request.Context().Done()
	select {
	case <-buffer.Ready():
		if startErr := buffer.StartError(); startErr != nil {
			// The session manager serves an error slate for eligible live
			// streams; the bytes arrive on dataChan like any other content,
			// so fall through. Anything else keeps the old behavior, except
			// the status is now genuinely reachable before the first byte.
			if !c.sessionManager.ServesSlateFor(streamID) {
				utils.WarnLog("Multiplex: upstream failed for %s (user=%s): %s",
					label, username, startErr.Error())
				c.sessionManager.RemoveClient(streamID, username, clientKey)
				ctx.AbortWithStatus(http.StatusBadGateway)
				return false
			}
		}
	case <-time.After(upstreamReadyTimeout):
		utils.WarnLog("Multiplex: upstream did not respond within %s for %s (user=%s)",
			upstreamReadyTimeout, label, username)
		c.sessionManager.RemoveClient(streamID, username, clientKey)
		ctx.AbortWithStatus(http.StatusGatewayTimeout)
		return false
	case <-clientGone:
		utils.DebugLog("Multiplex: client %s left before %s started", username, label)
		c.sessionManager.RemoveClient(streamID, username, clientKey)
		return false
	}
	return true
}

// relayToClient writes the session's data to the client until the channel
// closes, the pump drops this client, or the client goes away.
func relayToClient(ctx *gin.Context, dataChan chan []byte, doneChan <-chan struct{}, username, label string) {
	clientGone := ctx.Request.Context().Done()
	ctx.Stream(func(w io.Writer) bool {
		select {
		case data, ok := <-dataChan:
			if !ok {
				utils.DebugLog("Stream channel closed for user %s (%s)", username, label)
				return false
			}
			if _, err := w.Write(data); err != nil {
				// Client disconnected
				utils.DebugLog("Client write error for user %s (%s): %v", username, label, err)
				return false
			}
			// Force immediate delivery to client to avoid periodic buffering
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return true
		case <-doneChan:
			// Pump dropped this client (slow viewer) or the stream stopped
			utils.DebugLog("Stream done signal for user %s (%s)", username, label)
			return false
		case <-clientGone:
			// Client closed the connection while we were waiting for data
			utils.DebugLog("Client disconnected (idle) for user %s (%s)", username, label)
			return false
		}
	})
}
