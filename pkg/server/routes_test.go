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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/session"
)

// TestTrackManifestLinksFetchThroughImg guards the trackProxy path: the
// per-request track Config must sign with the server's key, or /img (main
// Config) rejects every HLS child link of an M3U-mode track.
func TestTrackManifestLinksFetchThroughImg(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			_, _ = w.Write([]byte("#EXTM3U\nseg.ts\n"))
			return
		}
		_, _ = w.Write([]byte("segment-bytes"))
	}))
	defer up.Close()

	c := testImageProxyConfig()
	c.XtreamUser, c.XtreamPassword, c.M3UFileName = "u", "p", "playlist.m3u"
	c.endpointAntiColision = "anti"
	c.playlist = &m3u.Playlist{Tracks: []m3u.Track{{URI: up.URL + "/live.m3u8"}}}
	router := newImgRouter(c)

	w := getImg(router, "/anti/u/p/0/live.m3u8")
	if w.Code != http.StatusOK {
		t.Fatalf("manifest: status = %d, body=%s", w.Code, w.Body.String())
	}
	var link string
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if strings.Contains(line, "/img?url=") {
			link = line
		}
	}
	if link == "" {
		t.Fatalf("no /img line in %q", w.Body.String())
	}
	u, _ := url.Parse(link)
	if w := getImg(router, u.RequestURI()); w.Code != http.StatusOK || w.Body.String() != "segment-bytes" {
		t.Fatalf("segment: status = %d, body = %q", w.Code, w.Body.String())
	}
}

func TestPlayAndHLSRequireRecentAuth(t *testing.T) {
	up, hits := countingUpstream(t)
	c := testImageProxyConfig()
	c.XtreamUser, c.XtreamPassword, c.XtreamBaseURL = "u", "p", up.URL
	c.sessionManager = session.NewSessionManager(nil)
	t.Cleanup(c.sessionManager.Stop)
	router := newImgRouter(c)

	recentlyAuthenticatedMu.Lock()
	recentlyAuthenticated = map[string]time.Time{}
	recentlyAuthenticatedMu.Unlock()

	for _, path := range []string{"/play/tok/ts", "/hls/tok/1_2"} {
		if w := getImg(router, path); w.Code != http.StatusForbidden {
			t.Fatalf("%s fresh IP: status = %d, want 403", path, w.Code)
		}
		if n := atomic.LoadInt32(hits); n != 0 {
			t.Fatalf("%s: provider saw %d requests", path, n)
		}
	}

	recentlyAuthenticatedMu.Lock()
	recentlyAuthenticated["192.0.2.1"] = time.Now()
	recentlyAuthenticatedMu.Unlock()
	for _, path := range []string{"/play/tok/ts", "/hls/tok/1_2"} {
		// The multiplexed relay needs a CloseNotifier, which a bare recorder lacks.
		w := closeNotifyRecorder{httptest.NewRecorder()}
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code == http.StatusForbidden {
			t.Fatalf("%s after auth: still 403", path)
		}
	}
}

type closeNotifyRecorder struct{ *httptest.ResponseRecorder }

func (closeNotifyRecorder) CloseNotify() <-chan bool { return nil }

// viewerRouteFixture serves newRouter over a fake provider and returns the
// front URL plus a func reporting the provider paths seen so far.
func viewerRouteFixture(t *testing.T, endpoint string) (string, func() []string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var seen []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.Path)
		mu.Unlock()
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(up.Close)
	sm := session.NewSessionManager(nil)
	t.Cleanup(sm.Stop)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			User: "me", Password: "pw", CustomEndpoint: endpoint,
			XtreamBaseURL: up.URL, XtreamUser: "user", XtreamPassword: "prov",
		},
		sessionManager: sm,
		imageKey:       testImageKey,
		playlist:       &m3u.Playlist{},
	}
	r, err := c.newRouter()
	if err != nil {
		t.Fatal(err)
	}
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return front.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func getStatus(t *testing.T, u string) int {
	t.Helper()
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestViewerRoutesUnderCustomEndpoint(t *testing.T) {
	front, seen := viewerRouteFixture(t, "providerA")
	for _, p := range []string{"/providerA/live/me/pw/42.ts", "/providerA/timeshift/me/pw/60/2026-01-01:10-00/42.ts", "/live/me/pw/43.ts"} {
		if s := getStatus(t, front+p); s != 200 {
			t.Fatalf("%s: status %d", p, s)
		}
	}
	got := strings.Join(seen(), " ")
	for _, want := range []string{"/live/user/prov/42.ts", "/timeshift/user/prov/60/2026-01-01:10-00/42.ts", "/live/user/prov/43.ts"} {
		if !strings.Contains(got, want) {
			t.Fatalf("provider saw %q, missing %q", got, want)
		}
	}
}

func TestQueryStyleTimeshift(t *testing.T) {
	for _, prefix := range []string{"", "/providerA"} {
		front, seen := viewerRouteFixture(t, "providerA")
		q := "/streaming/timeshift.php?username=me&password=pw&stream=42&start=2026-01-01:10-00&duration=60"
		if s := getStatus(t, front+prefix+q); s != 200 {
			t.Fatalf("%q: status %d", prefix, s)
		}
		if got := seen(); len(got) != 1 || got[0] != "/timeshift/user/prov/60/2026-01-01:10-00/42.ts" {
			t.Fatalf("%q: provider saw %v", prefix, got)
		}
		if s := getStatus(t, front+prefix+strings.Replace(q, "pw", "bad", 1)); s != 401 {
			t.Fatalf("bad password: status %d, want 401", s)
		}
		if s := getStatus(t, front+prefix+strings.Replace(q, "&start=2026-01-01:10-00", "", 1)); s != 400 {
			t.Fatalf("missing start: status %d, want 400", s)
		}
		if s := getStatus(t, front+prefix+strings.Replace(q, "start=2026-01-01:10-00", "start=..", 1)); s != 400 {
			t.Fatalf("start=..: status %d, want 400", s)
		}
		if s := getStatus(t, front+prefix+"/timeshift/me/pw/1..2/2026-01-01:10-00/42.ts"); s != 400 {
			t.Fatalf("path duration with ..: status %d, want 400", s)
		}
		if len(seen()) != 1 {
			t.Fatalf("rejected requests reached the provider: %v", seen())
		}
	}
}
