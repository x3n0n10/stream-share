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
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/lucasduport/stream-share/pkg/session"
)

var (
	// A country tag only counts when delimited ("NL | ", "UK: ", "[NL]"): a bare
	// "NL-TV" or "BBC One" must keep its letters.
	channelCountryTag = regexp.MustCompile(`^\s*(?:\[[a-z]{2,3}\]|\([a-z]{2,3}\)|[a-z]{2,3}\s*[|:])`)
	channelQualityTag = regexp.MustCompile(`\b(?:uhd|4k|fhd|hd|sd|hevc|h265|2160p|1080p|720p)\b`)
)

// normalizeChannelName reduces a channel name to what two providers share:
// no country tag, no quality words, no punctuation or decoration.
func normalizeChannelName(name string) string {
	s := channelCountryTag.ReplaceAllString(strings.ToLower(name), "")
	s = channelQualityTag.ReplaceAllString(s, " ")
	return strings.Map(func(r rune) rune {
		if (unicode.IsLetter(r) && !unicode.Is(unicode.Lm, r)) || unicode.IsDigit(r) {
			return r
		}
		return -1
	}, s)
}

// qualityClass ranks a name's quality word: 3 UHD/4K, 2 FHD, 1 HD or none, 0 SD.
func qualityClass(name string) int {
	class := -1
	for _, w := range channelQualityTag.FindAllString(strings.ToLower(name), -1) {
		c := -1
		switch w {
		case "uhd", "4k", "2160p":
			c = 3
		case "fhd", "1080p":
			c = 2
		case "sd":
			c = 0
		}
		if c > class {
			class = c
		}
	}
	if class < 0 {
		return 1
	}
	return class
}

// matchFailoverChannel finds this instance's live channel for a channel of
// another instance: the same EPG id, else the same normalised name, and among
// those the one closest in quality (lower class, then lower id, on a tie).
func (c *Config) matchFailoverChannel(epgID, name string) (string, bool) {
	var ids []string
	if epgID != "" {
		epgIndexMu.RLock()
		for id, e := range epgIndex {
			if strings.EqualFold(e, epgID) {
				ids = append(ids, id)
			}
		}
		epgIndexMu.RUnlock()
	}
	if want := normalizeChannelName(name); len(ids) == 0 && want != "" {
		apiChannelIndexMu.RLock()
		for id, n := range apiChannelIndex {
			if normalizeChannelName(n) == want {
				ids = append(ids, id)
			}
		}
		apiChannelIndexMu.RUnlock()
	}
	wantClass := qualityClass(name)
	best, bestClass, bestDist := "", 0, 0
	sort.Strings(ids)
	for _, id := range ids {
		if c.liveStreamFiltered(id) {
			continue
		}
		n, _ := lookupAPIChannelName(id)
		class := qualityClass(n)
		dist := class - wantClass
		if dist < 0 {
			dist = -dist
		}
		if best == "" || dist < bestDist || (dist == bestDist && class < bestClass) {
			best, bestClass, bestDist = id, class, dist
		}
	}
	return best, best != ""
}

// failoverStatus is the HTTP status that tells the calling instance why this
// one could not start the channel.
func failoverStatus(uerr *session.UpstreamError) int {
	switch {
	case uerr == nil:
		return 502
	case uerr.StatusCode >= 400:
		return uerr.StatusCode
	case uerr.Key == session.KeyAllInUse:
		return 503
	case uerr.Key == session.KeyTimeout:
		return 504
	}
	return 502
}
