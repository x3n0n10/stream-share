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
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/filter"
	"github.com/lucasduport/stream-share/pkg/session"
)

func TestNormalizeChannelName(t *testing.T) {
	for in, want := range map[string]string{
		"NL | Alpha 1 HD":  "alpha1",
		"Alpha 1 FHD ⏺ʳᵉᶜ": "alpha1",
		"Alpha 1 4K":       "alpha1",
		"UK: BBC One HD": "bbcone",
		"[NL] Beta 4":     "beta4",
		"Beta 4 UHD HEVC": "beta4",
		"NL-TV":          "nltv",
		"Channel 4 HD":   "channel4",
		"HD":             "",
		"★ ★":            "",
	} {
		if got := normalizeChannelName(in); got != want {
			t.Errorf("normalizeChannelName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQualityClass(t *testing.T) {
	for in, want := range map[string]int{
		"Alpha 1 UHD": 3, "Alpha 1 4K": 3, "Beta 4 FHD": 2, "X 1080p": 2, "Alpha 1 HD": 1, "Alpha 1": 1, "Alpha 1 SD": 0,
		"8K | Beta 4": 4, "X 4320p": 4,
	} {
		if got := qualityClass(in); got != want {
			t.Errorf("qualityClass(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestFailoverStatus(t *testing.T) {
	for _, tc := range []struct {
		err  *session.UpstreamError
		want int
	}{
		{&session.UpstreamError{StatusCode: 456}, 456},
		{&session.UpstreamError{StatusCode: 404}, 404},
		{&session.UpstreamError{Key: session.KeyAllInUse}, 503},
		{&session.UpstreamError{Key: session.KeyTimeout}, 504},
		{&session.UpstreamError{Key: "UNREACHABLE"}, 502},
		{nil, 502},
	} {
		if got := failoverStatus(tc.err); got != tc.want {
			t.Errorf("failoverStatus(%+v) = %d, want %d", tc.err, got, tc.want)
		}
	}
}

// setChannelIndex installs the live channel indices for a test.
func setChannelIndex(t *testing.T, names, epgs map[string]string) {
	t.Helper()
	apiChannelIndexMu.Lock()
	apiChannelIndex = names
	apiChannelIndexMu.Unlock()
	epgIndexMu.Lock()
	epgIndex = epgs
	epgIndexMu.Unlock()
	t.Cleanup(func() {
		apiChannelIndexMu.Lock()
		apiChannelIndex = nil
		apiChannelIndexMu.Unlock()
		epgIndexMu.Lock()
		epgIndex = nil
		epgIndexMu.Unlock()
	})
}

func TestMatchFailoverChannel(t *testing.T) {
	e := shareTestServer(t, 1, &filter.Config{Live: filter.Patterns{Group: `^A\|`}})
	e.p.streams = failoverCatalogue
	// Creating a share reads the catalogue, which the live filter judges ids on.
	if _, _, err := e.c.createLiveShare("lucas", "", "10", time.Now(), time.Now().Add(time.Hour), time.Now()); err != nil {
		t.Fatal(err)
	}
	setChannelIndex(t,
		map[string]string{"10": "Alpha 1 HD", "11": "Alpha 1 UHD", "12": "Beta 4 FHD", "13": "Alpha 1 SD"},
		map[string]string{"10": "Alpha1.nl", "11": "alpha1.nl", "13": "alpha1.nl"})

	for _, tc := range []struct {
		epg, name, want string
	}{
		{"alpha1.NL", "NL | Alpha 1 4K", "11"},
		{"alpha1.nl", "Alpha 1 FHD", "10"},
		{"", "Beta 4 HD", "12"},
		{"unknown.id", "Beta 4", "12"},
		{"", "HD", ""},
		{"", "", ""},
		{"alpha1.nl", "Alpha 1 SD", "10"},
	} {
		got, ok := e.c.matchFailoverChannel(tc.epg, tc.name)
		if got != tc.want || ok != (tc.want != "") {
			t.Errorf("match(%q, %q) = %q, %v; want %q", tc.epg, tc.name, got, ok, tc.want)
		}
	}
}

// failoverCatalogue is the live catalogue the matching tests run against;
// channel 13 sits in a category the filter hides.
const failoverCatalogue = `[{"num":1,"stream_id":10,"name":"Alpha 1 HD","category_id":"10","epg_channel_id":"Alpha1.nl","is_adult":0},` +
	`{"num":2,"stream_id":11,"name":"Alpha 1 UHD","category_id":"10","epg_channel_id":"alpha1.nl","is_adult":0},` +
	`{"num":3,"stream_id":12,"name":"Beta 4 FHD","category_id":"10","is_adult":0},` +
	`{"num":4,"stream_id":13,"name":"Alpha 1 SD","category_id":"20","epg_channel_id":"alpha1.nl","is_adult":0}]`
