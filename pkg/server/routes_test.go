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
	"net/url"
	"testing"

	"github.com/jamesnetherton/m3u"
	"github.com/lucasduport/stream-share/pkg/config"
)

// TestImgRouteNeedsNoLogin exercises the registered route (via c.routes, as
// production does): /img carries no credentials, so a signed link works for a
// client that never authenticated, and an unsigned one does not.
func TestImgRouteNeedsNoLogin(t *testing.T) {
	up, _ := countingUpstream(t)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{User: "u", Password: "p"},
		playlist:    &m3u.Playlist{},
		imageKey:    []byte("0123456789abcdef0123456789abcdef"),
	}
	router := newImgRouter(c)

	if w := getImg(router, "/img?url="+url.QueryEscape(up.URL)); w.Code != http.StatusForbidden {
		t.Errorf("unsigned: status = %d, want 403", w.Code)
	}
	if w := getImg(router, c.proxyImagePath(up.URL+"/logo.png")); w.Code != http.StatusOK || w.Body.String() != "fake-png-bytes" {
		t.Errorf("signed: status = %d body = %q, want 200 and the upstream body", w.Code, w.Body.String())
	}
}
