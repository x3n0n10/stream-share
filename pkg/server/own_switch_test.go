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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/session"
)

// ownSwitchServer serves live channels at /:user/live/:id and cached series
// at /:user/series/:id from one provider with capacity connections. The
// series download runs about 1.6 s (1 MiB, 64 KiB per 100 ms).
func ownSwitchServer(t *testing.T, capacity int) (*Config, *httptest.Server) {
	t.Helper()
	t.Setenv("CACHE_FOLDER", t.TempDir())
	gin.SetMode(gin.TestMode)
	invalidateCatalogCache()
	prevSpacing := setVODSpacing(20 * time.Millisecond)
	t.Cleanup(func() { setVODSpacing(prevSpacing) })
	vodCacheClient.CloseIdleConnections()

	live := &liveProvider{}
	files := &fakeVODProvider{data: randomBytes(t, 1<<20), chunk: 64 << 10, delay: 100 * time.Millisecond}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/series/"):
			files.ServeHTTP(w, r)
			return
		case r.URL.Path == "/get.php": // the VOD list refresh; not a stream
			http.NotFound(w, r)
			return
		}
		live.ServeHTTP(w, r)
	}))
	t.Cleanup(up.Close)

	c := &Config{
		ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL, XtreamUser: "prov", XtreamPassword: "secret", VODCacheEnabled: true},
		db:          &database.DBManager{},
	}
	c.providerSlots = provider.New(func() int { return capacity })
	c.sessionManager = session.NewSessionManager(nil)
	c.sessionManager.SetProviderSlots(c.providerSlots)
	c.sessionManager.SetVODLeftHook(c.vodViewerLeft)
	t.Cleanup(func() { waitDownloadsIdle(c) })
	t.Cleanup(c.sessionManager.Stop)

	// As the auth middleware does: the user has a session.
	login := func(ctx *gin.Context) {
		ctx.Set("username", ctx.Param("user"))
		c.sessionManager.RegisterUser(ctx.Param("user"), ctx.ClientIP(), "")
	}
	r := gin.New()
	r.GET("/:user/live/:id", func(ctx *gin.Context) {
		login(ctx)
		c.xtreamStreamHandler(ctx)
	})
	r.GET("/:user/series/:id", func(ctx *gin.Context) {
		login(ctx)
		c.streamVODWithCache(ctx, "series", ".mkv")
	})
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return c, front
}

// watchLive opens a live channel as user and fails unless it plays.
func watchLive(t *testing.T, ctx context.Context, front, user, id string) (*int64, <-chan struct{}) {
	t.Helper()
	code, n, done := watch(t, ctx, front+"/"+user+"/live/"+id)
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(n) > 0 }) {
		t.Fatalf("live %s for %s: %d", id, user, code)
	}
	return n, done
}

// A viewer leaving live for a movie on a one-connection provider gets the
// movie: their own live stream is stopped, not "in use".
func TestOwnSwitchLiveToMovie(t *testing.T) {
	_, front := ownSwitchServer(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, liveDone := watchLive(t, ctx, front.URL, "alice", "101")
	resp, _, err := getRange(t, front.URL+"/alice/series/601.mkv", "bytes=0-1023")
	if err != nil || resp.StatusCode >= 400 {
		t.Fatalf("movie after own live: %v %v, want it to play", resp, err)
	}
	select {
	case <-liveDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the viewer's live stream should have stopped")
	}
}

// A viewer leaving a movie for live gets the channel: the download of the
// movie they left gives its connection up instead of keeping it for the
// reader grace.
func TestOwnSwitchMovieToLive(t *testing.T) {
	_, front := ownSwitchServer(t, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	resp, _, err := getRange(t, front.URL+"/alice/series/601.mkv", "bytes=0-1023")
	if err != nil || resp.StatusCode >= 400 {
		t.Fatalf("movie: %v %v", resp, err)
	}
	watchLive(t, ctx, front.URL, "alice", "101")
}

// Another user's streams still turn a request away: a movie while someone
// else watches live gets 503, and live while someone else's movie downloads
// gets no stream.
func TestOtherUsersStreamsStillRefuse(t *testing.T) {
	t.Run("live then movie", func(t *testing.T) {
		_, front := ownSwitchServer(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n, _ := watchLive(t, ctx, front.URL, "bob", "101")
		resp, _, err := getRange(t, front.URL+"/alice/series/601.mkv", "bytes=0-1023")
		if err != nil || resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("alice's movie while bob watches: %v %v, want 503", resp, err)
		}
		before := atomic.LoadInt64(n)
		if !eventually(time.Second, func() bool { return atomic.LoadInt64(n) > before }) {
			t.Fatal("bob's live stream must keep playing")
		}
	})
	t.Run("movie then live", func(t *testing.T) {
		_, front := ownSwitchServer(t, 1)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		// bob keeps a request open on the download, so it stays watched.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, front.URL+"/bob/series/601.mkv", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil || resp.StatusCode >= 400 {
			t.Fatalf("bob's movie: %v %v", resp, err)
		}
		defer func() { _ = resp.Body.Close() }()
		code, n, _ := watch(t, ctx, front.URL+"/alice/live/101")
		if code == http.StatusOK && eventually(2*time.Second, func() bool { return atomic.LoadInt64(n) > 0 }) {
			t.Fatal("alice's live must not take bob's movie connection")
		}
	})
}
