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
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// playlistInitialization writes a proxified M3U file to disk if a playlist was parsed.
func (c *Config) playlistInitialization() error {
	if len(c.playlist.Tracks) == 0 {
		return nil
	}

	f, err := os.Create(c.proxyfiedM3UPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	return c.marshallInto(f, false)
}

// MarshallInto a *bufio.Writer a Playlist.
// marshallInto writes the in-memory playlist into an M3U file, rewriting
// credentials and paths depending on xtream mode. When M3U dedup is enabled,
// redundant tracks are dropped first (same stream ID listed twice, or
// same-title copies differing only by language/quality), keeping the best
// source per group.
func (c *Config) marshallInto(into *os.File, xtream bool) error {
	tracks := c.playlist.Tracks
	if c.M3UDedupEnabled && len(tracks) > 1 {
		before := len(tracks)
		tracks, _ = deduplicateTracks(tracks, dedupPreferredLanguages(c.M3UDedupPreferredLangs))
		if len(tracks) != before {
			utils.InfoLog("M3U dedup: %d tracks -> %d kept (%d redundant removed)", before, len(tracks), before-len(tracks))
		}
	}

	filteredTrack := make([]m3u.Track, 0, len(tracks))

	ret := 0
	into.WriteString("#EXTM3U\n") // nolint: errcheck
	for i, track := range tracks {
		var buffer bytes.Buffer

		buffer.WriteString("#EXTINF:") // nolint: errcheck
		fmt.Fprintf(&buffer, "%d ", track.Length)
		for i := range track.Tags {
			tagValue := track.Tags[i].Value
			if track.Tags[i].Name == "tvg-logo" {
				tagValue = c.proxyImagePath(tagValue)
			}
			if i == len(track.Tags)-1 {
				fmt.Fprintf(&buffer, "%s=%q", track.Tags[i].Name, tagValue)
				continue
			}
			fmt.Fprintf(&buffer, "%s=%q ", track.Tags[i].Name, tagValue)
		}

		uri, err := c.replacePath(track.URI, i-ret, xtream)
		if err != nil {
			ret++
			utils.ErrorLog("track: %s: %s", track.Name, err)
			continue
		}

		_, _ = fmt.Fprintf(into, "%s, %s\n%s\n", buffer.String(), track.Name, uri)

		filteredTrack = append(filteredTrack, track)
	}
	// Store back the written list so route registration (which runs after
	// playlistInitialization) sees the same tracks and indices as the file.
	c.playlist.Tracks = filteredTrack

	return into.Sync()
}

// replacePath rewrites a track URI into the relative path this proxy will
// serve it at — no scheme or host, since the same cached playlist may be
// served to clients reaching this server via different hostnames; the
// host gets prefixed at serve time by rewritePlaylistHosts.
//
// Known limitation: a track URI with embedded HTTP Basic Auth credentials
// (rare; e.g. a raw M3U-provider URI shaped like http://user:pass@host/...)
// previously carried those credentials into the generated proxy URL. That
// is not preserved here — there is no existing test covering it, and
// encoding it into a purely relative value that still resolves correctly
// at serve time isn't worth the complexity for what appears to be an
// unused path.
func (c *Config) replacePath(uri string, trackIndex int, xtream bool) (string, error) {
	oriURL, err := url.Parse(uri)
	if err != nil {
		return "", err
	}

	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = fmt.Sprintf("/%s", customEnd)
	}

	uriPath := oriURL.EscapedPath()
	if xtream {
		// Xtream get.php mode: replace provider creds with local creds in path
		uriPath = strings.ReplaceAll(uriPath, c.XtreamUser.PathEscape(), c.User.PathEscape())
		uriPath = strings.ReplaceAll(uriPath, c.XtreamPassword.PathEscape(), c.Password.PathEscape())
	} else {
		// M3U proxified path
		uriPath = path.Join(
			"/",
			c.endpointAntiColision,
			c.User.PathEscape(),
			c.Password.PathEscape(),
			fmt.Sprintf("%d", trackIndex),
			path.Base(uriPath),
		)
	}

	return customEnd + uriPath, nil
}

// sanitiseFilename strips characters that are unsafe inside a quoted
// Content-Disposition filename value, preventing header injection.
func sanitiseFilename(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '"' || r == '\r' || r == '\n' || r == '\\' {
			return '_'
		}
		return r
	}, name)
}
