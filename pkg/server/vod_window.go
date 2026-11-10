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
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// vodHeadSize is how much the first request of a download asks for. It is a
// bounded request so the connection can be reused for the container index at
// the end of the file (vodTailSize) before the rest of the file follows; the
// header is what a player reads first, so it is not delayed.
var vodHeadSize int64 = 1 << 20

// vodTailMin, vodTailMax and vodTailFraction size the end of the file fetched
// right after the header: demuxers read a container index stored there
// (matroska cues and tags, an MP4 moov written last) before playing. Cues are
// typically well under 1 MiB, but a long movie's moov is several MiB, so the
// tail is max(4 MiB, 0.5 % of the file), capped at 16 MiB. Its last
// vodTailFirst bytes come first: that is where the index nearly always is, so
// a player waiting for it is served after ~1 MiB rather than the whole tail.
//
// Files smaller than vodTailMinFile get no tail fetch: they download in
// moments, and on-demand windows cover a read near their end.
var (
	vodTailMin      int64 = 4 << 20
	vodTailMax      int64 = 16 << 20
	vodTailFraction int64 = 200 // 1/200 = 0.5 %
	vodTailFirst    int64 = 1 << 20
	vodTailMinFile  int64 = 32 << 20
)

// vodTailSize returns how much of the end of a file of size total is fetched
// ahead of the sequential download.
func vodTailSize(total int64) int64 {
	n := total / vodTailFraction
	if n < vodTailMin {
		n = vodTailMin
	}
	if n > vodTailMax {
		n = vodTailMax
	}
	if n > total {
		n = total
	}
	return n
}

// fetchTail fetches the end of the file (see vodTailSize) into f ahead of the
// sequential download, as windows, once the header is in: players read a
// container index there right after the header. The last vodTailFirst bytes
// come first, then the rest of the tail. It returns false when the slot was
// preempted meanwhile (the caller parks and tries again on resume); a failure
// is counted like a failed window and the download goes on.
func (c *Config) fetchTail(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease) bool {
	tailStart := total - vodTailSize(total)
	firstStart := total - vodTailFirst
	if firstStart < tailStart {
		firstStart = tailStart
	}
	for _, w := range []byteSpan{{firstStart, total}, {tailStart, firstStart}} {
		if w.end <= w.start || dl.readableEnd(w.start) >= w.end {
			continue
		}
		if r := dl.readableEnd(w.start); r > w.start {
			w.start = r
		}
		if !c.fetchTailPiece(ctx, dl, f, upstream, total, lease, w) {
			return false
		}
	}
	return true
}

// fetchTailPiece fetches one piece of the tail; see fetchTail.
func (c *Config) fetchTailPiece(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease, w byteSpan) bool {
	if dl.windowsDisabled() {
		return true
	}
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lease.Preempted():
			cancel()
		case <-stop:
		}
	}()
	err := fetchWindow(wctx, dl, f, upstream, w, total)
	if err != nil && lease.IsPreempted() {
		return false
	}
	if err != nil {
		utils.WarnLog("Cache: fetching the end of the file (bytes %d-%d) failed: %v", w.start, w.end-1, err)
		dl.windowFailed(errors.Is(err, errRangeIgnored))
		return true
	}
	utils.DebugLog("Cache: fetched the end of the file (bytes %d-%d) for %s", w.start, w.end-1, utils.MaskURL(upstream))
	return true
}

// errRangeIgnored reports a provider answering a ranged request with the whole
// file (200).
var errRangeIgnored = errors.New("provider ignored the Range header")

// fetchWindows fetches every window readers are waiting for (see
// vodDownload.requestWindow), one ranged request at a time, into f at their
// offsets. Readers are woken as the bytes land. A provider that ignores Range
// disables windows for this download; other failures are counted and retried
// on the readers' next request, up to vodWindowMaxFailures.
//
// A window being fetched when the slot is preempted is abandoned without
// counting as a failure; readers ask for it again.
func (c *Config) fetchWindows(ctx context.Context, dl *vodDownload, f *os.File, upstream string, total int64, lease *provider.Lease) {
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-lease.Preempted():
			cancel()
		case <-stop:
		}
	}()
	for wctx.Err() == nil {
		w, ok := dl.nextWindow()
		if !ok {
			return
		}
		err := fetchWindow(wctx, dl, f, upstream, w, total)
		if err != nil && lease.IsPreempted() {
			dl.requeueWindow(w) // fetched once the download has a slot again
			return
		}
		if err != nil {
			utils.WarnLog("Cache: out-of-order fetch of bytes %d-%d failed: %v", w.start, w.end-1, err)
			dl.windowFailed(errors.Is(err, errRangeIgnored))
			continue
		}
		utils.DebugLog("Cache: fetched bytes %d-%d ahead of the download for %s", w.start, w.end-1, utils.MaskURL(upstream))
	}
}

// fetchWindow fetches w with a ranged request and writes it at its offset.
func fetchWindow(ctx context.Context, dl *vodDownload, f *os.File, upstream string, w byteSpan, total int64) error {
	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", upstream, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", utils.GetIPTVUserAgent())
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", w.start, w.end-1))
	resp, err := vodCacheClient.Do(req)
	if err != nil {
		return err
	}
	// Deferred after cancel, so it runs first: a body read to its end and
	// closed hands its connection back for the next request.
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusPartialContent:
	case http.StatusOK:
		return errRangeIgnored
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	// The answer must start where asked; anything else cannot be placed.
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, fmt.Sprintf("bytes %d-", w.start)) {
		return fmt.Errorf("unexpected Content-Range %q", cr)
	}

	stall := time.AfterFunc(vodCacheStallTimeout, cancel)
	defer stall.Stop()
	body := &stallReader{r: resp.Body, timer: stall, idle: vodCacheStallTimeout}
	buf := make([]byte, 256*1024)
	off := w.start
	for off < w.end {
		n := int64(len(buf))
		if rest := w.end - off; rest < n {
			n = rest
		}
		nr, er := body.Read(buf[:n])
		if nr > 0 {
			if _, ew := f.WriteAt(buf[:nr], off); ew != nil {
				return ew
			}
			dl.addWindow(w.start, off+int64(nr))
			off += int64(nr)
		}
		if er != nil {
			if er == io.EOF && off >= w.end {
				break
			}
			if er == io.EOF && off >= total {
				break
			}
			return er
		}
	}
	// Reach the end of the body (nothing is left of a conforming answer), so
	// the connection is reusable.
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 64<<10))
	return nil
}
