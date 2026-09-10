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
	"strings"
	"testing"
)

func TestRewriteXMLTVIcons(t *testing.T) {
	c := testImageProxyConfig()

	t.Run("channel-level icon src rewritten", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/ch1.png"/></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/ch1.png")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten channel icon %q", out, want)
		}
	})

	t.Run("programme-level icon src rewritten", func(t *testing.T) {
		in := `<tv><programme channel="1"><icon src="http://upstream.example.com/prog1.png"/></programme></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/prog1.png")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten programme icon %q", out, want)
		}
	})

	t.Run("entity-escaped query string survives", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/a?b=1&amp;c=2"/></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		want := "http://proxy.example.com:8080/img?url=" + url.QueryEscape("http://upstream.example.com/a?b=1&c=2")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain the correctly round-tripped rewritten icon %q", out, want)
		}
	})

	t.Run("url element left untouched", func(t *testing.T) {
		in := `<tv><channel id="1"><url>http://upstream.example.com/info</url></channel></tv>`
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		if !strings.Contains(out, "http://upstream.example.com/info") {
			t.Errorf("output %q should still contain the untouched <url> content", out)
		}
	})

	// NOTE: encoding/xml's Decoder/Encoder token round-trip does not
	// preserve self-closing tags (<x/> becomes <x></x>), so byte-identity
	// only holds for a document with no self-closing elements at all, not
	// just "no icon elements" as the design doc loosely put it.
	t.Run("document with no self-closing elements round-trips byte-identical", func(t *testing.T) {
		in := "<tv>\n  <channel id=\"1\">\n    <display-name>Channel One</display-name>\n  </channel>\n</tv>\n"
		out := string(c.rewriteXMLTVIcons([]byte(in)))
		if out != in {
			t.Errorf("output = %q, want byte-identical %q", out, in)
		}
	})

	t.Run("malformed XML falls back to the original bytes", func(t *testing.T) {
		in := `<tv><channel id="1">`
		out := c.rewriteXMLTVIcons([]byte(in))
		if string(out) != in {
			t.Errorf("output = %q, want unmodified original %q", string(out), in)
		}
	})
}
