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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/slate"
	xtreamapi "github.com/lucasduport/stream-share/pkg/xtream"
)

func TestProviderTimeshiftStart(t *testing.T) {
	berlin, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		loc  *time.Location
		in   string
		want string
	}{
		{time.UTC, "2026-11-10:19-00", "2026-11-10:19-00"},
		{berlin, "2026-11-10:19-00", "2026-11-10:20-00"},
		{berlin, "2026-07-10:19-00", "2026-07-10:21-00"},
		{berlin, "2026-10-25:00-30", "2026-10-25:02-30"}, // CEST
		{berlin, "2026-10-25:01-30", "2026-10-25:02-30"}, // CET
		{berlin, "2026-03-29:01-30", "2026-03-29:03-30"},
		{berlin, "1762801200", "1762801200"},
		{berlin, "garbage", "garbage"},
	}
	for _, tc := range cases {
		if got := providerTimeshiftStart(tc.in, tc.loc); got != tc.want {
			t.Errorf("%s in %s: got %q, want %q", tc.in, tc.loc, got, tc.want)
		}
	}
}

func TestProviderLocation(t *testing.T) {
	clock := &xtreamapi.AccountInfo{
		ServerTimezone:  "GMT+1",
		ServerTime:      "2026-11-10 17:21:24",
		ServerTimestamp: time.Date(2026, 11, 10, 16, 21, 24, 0, time.UTC).Unix(),
	}
	clockAt := func(local, utc string) *xtreamapi.AccountInfo {
		u, _ := time.Parse(time.RFC3339, utc)
		return &xtreamapi.AccountInfo{ServerTime: local, ServerTimestamp: u.Unix()}
	}
	cases := []struct {
		name    string
		setting string
		info    *xtreamapi.AccountInfo
		want    string // zone name, or "+3600" for a fixed offset
	}{
		{"setting beats provider", "Europe/Amsterdam", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Europe/Amsterdam"},
		{"unknown setting falls through", "Mars/Base", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Africa/Dakar"},
		{"provider zone", "", &xtreamapi.AccountInfo{ServerTimezone: "Africa/Dakar"}, "Africa/Dakar"},
		{"provider clock", "", clock, "provider+01:00"},
		{"clock -01:00", "", clockAt("2026-11-10 15:21:24", "2026-11-10T16:21:24Z"), "provider-01:00"},
		{"clock -03:30", "", clockAt("2026-11-10 12:51:24", "2026-11-10T16:21:24Z"), "provider-03:30"},
		{"no data", "", &xtreamapi.AccountInfo{}, "UTC"},
		{"no cache", "", nil, "UTC"},
	}
	for _, tc := range cases {
		c := providerConfigWith(tc.info)
		c.CatchupProviderTimezone = tc.setting
		loc := c.providerLocation()
		_, off := time.Date(2026, 11, 10, 12, 0, 0, 0, loc).Zone()
		got := loc.String()
		if got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
		if strings.HasPrefix(tc.want, "provider") {
			wantOff := map[string]int{"provider+01:00": 3600, "provider-01:00": -3600, "provider-03:30": -12600}[tc.want]
			if off != wantOff || c.tz.lastSource != "provider clock" {
				t.Errorf("%s: offset %d source %q, want %d from provider clock", tc.name, off, c.tz.lastSource, wantOff)
			}
		}
	}
	// Empty cache, nil state, and nothing configured must never reach the provider.
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("providerLocation contacted the provider")
	}))
	defer up.Close()
	for _, st := range []*providerInfoState{nil, {}} {
		c := &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: up.URL}, providerInfo: st}
		if c.providerLocation() != time.UTC {
			t.Error("nil/empty provider cache should give UTC")
		}
	}
}

func TestTimeshiftProxyConvertsStart(t *testing.T) {
	front, seen := viewerRouteFixtureWith(t, "", func(c *Config) {
		c.providerInfo = &providerInfoState{info: &xtreamapi.AccountInfo{ServerTimezone: "Europe/Berlin"}, lastAttempt: time.Now()}
	})
	if s := getStatus(t, front+"/timeshift/me/pw/60/2026-11-10:19-00/42.ts"); s != 200 {
		t.Fatalf("status %d", s)
	}
	if got := seen(); len(got) != 1 || got[0] != "/timeshift/user/prov/60/2026-11-10:20-00/42.ts" {
		t.Fatalf("provider saw %v", got)
	}
}

// A channel the provider's list does not flag as archived, but that has no
// local buffer either, still asks the provider: its EPG may list recordings.
func TestTimeshiftWithoutBufferAsksProvider(t *testing.T) {
	front, seen := viewerRouteFixtureWith(t, "", func(c *Config) {
		c.catchupManager = catchup.New(true, t.TempDir(), 3)
	})
	if s := getStatus(t, front+"/timeshift/me/pw/60/2026-11-10:19-00/42.ts"); s != 200 {
		t.Fatalf("status %d", s)
	}
	if got := seen(); len(got) != 1 || got[0] != "/timeshift/user/prov/60/2026-11-10:19-00/42.ts" {
		t.Fatalf("provider saw %v", got)
	}
}

// A start in the buffer's first minute is served from the buffer: players send
// whole minutes, so a pause right after tuning in resumes locally.
func TestTimeshiftInBufferFirstMinuteStaysLocal(t *testing.T) {
	front, seen := viewerRouteFixtureWith(t, "", func(c *Config) {
		c.catchupManager = catchup.New(true, t.TempDir(), 3)
		c.catchupManager.StartBuffer("42").Stop() // stopped: serving it ends at EOF
	})
	start := time.Now().UTC().Truncate(time.Minute).Format(timeshiftLayout)
	getStatus(t, front+"/timeshift/me/pw/60/"+start+"/42.ts")
	for _, p := range seen() {
		if strings.HasPrefix(p, "/timeshift/") {
			t.Fatalf("provider asked for a time the buffer covers: %v", seen())
		}
	}
}

// catchupSlate stands in for the ffmpeg slate renderer.
type catchupSlate struct{ path string }

func (s catchupSlate) Available() bool                 { return true }
func (s catchupSlate) Clip(slate.View) (string, error) { return s.path, nil }

// A time before the local buffer goes to the provider: its recording plays when
// it has one, its 404 becomes the "no recording" slate, other answers are asked
// once more (a just-closed connection may still count), and without slates the
// player gets the 404 as before.
func TestTimeshiftBeforeBuffer(t *testing.T) {
	clip := filepath.Join(t.TempDir(), "slate.ts")
	if err := os.WriteFile(clip, []byte("SLATE"), 0o600); err != nil {
		t.Fatal(err)
	}
	defer func(d time.Duration) { catchupRetryDelay = d }(catchupRetryDelay)
	catchupRetryDelay = 10 * time.Millisecond
	for _, tc := range []struct {
		name       string
		provider   []int // answers in order; the last one repeats
		slates     bool
		wantStatus int
		wantBody   string
		wantAsked  int
	}{
		{"provider has it", []int{200}, true, 200, "x", 1},
		{"provider has nothing", []int{404}, true, 200, "SLATE", 1},
		{"provider has nothing, slates off", []int{404}, false, 404, "", 1},
		{"connection limit", []int{456}, true, 200, "SLATE", 2},
		{"connection freed on the second try", []int{456, 200}, true, 200, "x", 2},
		// Some providers (behind a CDN) say "no recording" with an empty page.
		{"provider answers an empty page", []int{statusEmptyPage}, true, 200, "SLATE", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			var asked []string
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				asked = append(asked, r.URL.Path)
				status := tc.provider[min(len(asked), len(tc.provider))-1]
				mu.Unlock()
				if status == statusEmptyPage {
					emptyPage(w)
					return
				}
				w.Header().Set("Content-Type", "video/mp2t")
				w.WriteHeader(status)
				_, _ = w.Write([]byte("x"))
			}))
			defer up.Close()
			front, _ := viewerRouteFixtureWith(t, "", func(c *Config) {
				c.XtreamBaseURL = up.URL
				c.catchupManager = catchup.New(true, t.TempDir(), 3)
				c.catchupManager.StartBuffer("42") // started now: 20 hours ago is before it
				if tc.slates {
					c.sessionManager.SetErrorCatalog(session.LoadCatalog(""))
					c.sessionManager.SetSlateProvider(catchupSlate{clip})
					c.sessionManager.SetSlateRetryMax(300 * time.Millisecond)
				}
			})
			resp, err := http.Get(front + "/timeshift/me/pw/60/" + time.Now().UTC().Add(-20*time.Hour).Format(timeshiftLayout) + "/42.ts")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != tc.wantStatus || !strings.HasPrefix(string(body), tc.wantBody) {
				t.Fatalf("got %d %q, want %d %q", resp.StatusCode, body, tc.wantStatus, tc.wantBody)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(asked) != tc.wantAsked || !strings.HasPrefix(asked[0], "/timeshift/") {
				t.Fatalf("provider asked %v, want %d timeshift request(s)", asked, tc.wantAsked)
			}
		})
	}
}

// statusEmptyPage marks a fake provider answer of 200 with an empty HTML page,
// how some providers say they have no recording.
const statusEmptyPage = -1

func emptyPage(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=UTF-8")
	w.Header().Set("Content-Length", "0")
	w.WriteHeader(http.StatusOK)
}

// A provider "success" without a recording is an empty answer or a small text
// page; real video, even mislabelled as text, is not.
func TestEmptyCatchupAnswer(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		ctype  string
		length int64
		empty  bool
	}{
		{"empty html page", 200, "text/html; charset=UTF-8", 0, true},
		{"empty, no type", 200, "", 0, true},
		{"small error page", 200, "text/html", 512, true},
		{"recording", 200, "video/mp2t", 736096256, false},
		{"recording of unknown length", 200, "video/mp2t", -1, false},
		{"recording mislabelled as html", 200, "text/html", 736096256, false},
		{"html of unknown length", 200, "text/html", -1, false},
		{"not a success", 404, "text/html", 0, false},
	} {
		resp := &http.Response{StatusCode: tc.status, ContentLength: tc.length, Header: http.Header{}}
		resp.Header.Set("Content-Type", tc.ctype)
		if got := emptyCatchupAnswer(resp); got != tc.empty {
			t.Errorf("%s: empty = %v, want %v", tc.name, got, tc.empty)
		}
	}
}

// A start counts as covered when any of its minute is buffered, also for
// second-precision (unix) starts.
func TestBufferCoversWholeMinutes(t *testing.T) {
	oldest := time.Date(2026, 11, 12, 20, 0, 40, 0, time.UTC)
	buf, err := catchup.NewDiskBuffer(t.TempDir(), "7", oldest, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer buf.Stop()
	for start, want := range map[string]bool{
		"20:00:00": true,  // its minute reaches into the buffer
		"20:00:59": true,  // inside the buffer
		"19:59:00": false, // whole minute before the buffer
		"19:59:50": false, // unix start in that same minute
	} {
		at, _ := time.Parse("15:04:05", start)
		ts := time.Date(2026, 11, 12, at.Hour(), at.Minute(), at.Second(), 0, time.UTC)
		if got := bufferCovers(buf, ts); got != want {
			t.Errorf("start %s: covered = %v, want %v", start, got, want)
		}
	}
}

// A provider 404 for a time well inside its archive means the channel list's
// archive flag is wrong for that channel; a 404 for a time it may simply not
// have (too recent, too old) teaches nothing.
func TestProviderArchiveMissLearned(t *testing.T) {
	notFound := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer notFound.Close()
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { emptyPage(w) }))
	defer empty.Close()
	for _, tc := range []struct {
		name     string
		provider string
		age      time.Duration
		learns   bool
	}{
		{"an hour ago", notFound.URL, time.Hour, true},
		{"two minutes ago", notFound.URL, 2 * time.Minute, false},
		{"three days ago", notFound.URL, 72 * time.Hour, false},
		{"an hour ago, empty page", empty.URL, time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cm := catchup.New(true, t.TempDir(), 3)
			cm.SetUpstreamCatchup(map[string]bool{"42": true})
			front, _ := viewerRouteFixtureWith(t, "", func(c *Config) {
				c.catchupManager = cm
				c.XtreamBaseURL = tc.provider
			})
			start := time.Now().UTC().Add(-tc.age).Format(timeshiftLayout)
			getStatus(t, front+"/timeshift/me/pw/60/"+start+"/42.ts") // slates off: the provider's answer
			if learned := !cm.HasUpstreamCatchup("42"); learned != tc.learns {
				t.Fatalf("learned = %v, want %v", learned, tc.learns)
			}
		})
	}
}
