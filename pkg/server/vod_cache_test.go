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
	"strings"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

// An explicit extension gives the same upstream and cache file in both
// modes, and the per-type defaults agree across modes (probing off), so a
// player request and a Discord download land on one cache file.
func TestVODResolverModesAgree(t *testing.T) {
	t.Setenv("CACHE_FOLDER", t.TempDir())
	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: "http://provider.invalid"}}

	upA, destA, _ := c.resolveVODTarget("movie", "", "10.mkv", false)
	upB, destB, _ := c.resolveVODTarget("movie", "", "10.mkv", true)
	if upA != upB || destA != destB || !strings.HasSuffix(destA, "10.mkv") {
		t.Fatalf("explicit ext differs: %s %s / %s %s", upA, destA, upB, destB)
	}

	// Nothing known, probing off: per-type default in both modes.
	for _, probe := range []bool{false, true} {
		if _, d, _ := c.resolveVODTarget("series", "", "30", probe); !strings.HasSuffix(d, "30.mkv") {
			t.Fatalf("series default probe=%v: %s", probe, d)
		}
		if _, d, _ := c.resolveVODTarget("movie", "", "31", probe); !strings.HasSuffix(d, "31.mp4") {
			t.Fatalf("movie default probe=%v: %s", probe, d)
		}
	}
}
