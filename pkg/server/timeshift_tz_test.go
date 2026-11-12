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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
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

// A buffer that starts after the requested time still serves the request:
// channels without provider recordings keep their partial replay.
func TestTimeshiftWithLateBufferStaysLocal(t *testing.T) {
	front, seen := viewerRouteFixtureWith(t, "", func(c *Config) {
		c.catchupManager = catchup.New(true, t.TempDir(), 3)
		c.catchupManager.StartBuffer("42").Stop() // stopped: serving it ends at EOF
	})
	getStatus(t, front+"/timeshift/me/pw/60/2026-11-10:19-00/42.ts")
	for _, p := range seen() {
		if strings.HasPrefix(p, "/timeshift/") {
			t.Fatalf("provider asked for catchup despite a local buffer: %v", seen())
		}
	}
}
