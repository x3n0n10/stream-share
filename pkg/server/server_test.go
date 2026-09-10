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
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestMarshallIntoRewritesTvgLogo(t *testing.T) {
	c := &Config{
		ProxyConfig: &config.ProxyConfig{},
		playlist: &m3u.Playlist{
			Tracks: []m3u.Track{
				{
					Name:   "Channel One",
					Length: -1,
					URI:    "http://upstream.example.com/stream1",
					Tags: []m3u.Tag{
						{Name: "tvg-id", Value: "ch1"},
						{Name: "tvg-logo", Value: "http://upstream.example.com/logo1.png"},
					},
				},
			},
		},
	}

	f, err := os.CreateTemp(t.TempDir(), "playlist-*.m3u")
	if err != nil {
		t.Fatalf("CreateTemp: %v", err)
	}
	defer func() { _ = f.Close() }()

	if err := c.marshallInto(f, true); err != nil {
		t.Fatalf("marshallInto: %v", err)
	}

	data, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	out := string(data)

	wantLogo := "/img?url=" + url.QueryEscape("http://upstream.example.com/logo1.png")
	if !strings.Contains(out, wantLogo) {
		t.Errorf("output %q does not contain relative rewritten tvg-logo %q", out, wantLogo)
	}
	if strings.Contains(out, `"http://upstream.example.com/logo1.png"`) {
		t.Errorf("output %q still contains the raw upstream logo URL", out)
	}
	if strings.Contains(out, "://") && strings.Contains(out, "tvg-logo=") {
		// crude guard: tvg-logo's value should never contain a scheme once
		// rewritten — it must stay relative for rewritePlaylistHosts to prefix.
		if idx := strings.Index(out, "tvg-logo="); idx != -1 && strings.Contains(out[idx:idx+200], "://") {
			t.Errorf("output %q has an absolute tvg-logo value, want relative", out)
		}
	}
	if !strings.Contains(out, `tvg-id="ch1"`) {
		t.Errorf("output %q lost the untouched tvg-id tag", out)
	}
	if !strings.Contains(out, "\n/stream1\n") {
		t.Errorf("output %q does not contain the relative track URI /stream1", out)
	}
}
