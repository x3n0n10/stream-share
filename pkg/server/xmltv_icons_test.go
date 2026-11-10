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
	"strings"
	"testing"
)

// Providers often send a UTF-8 BOM or a blank line before the XML
// declaration, and some concatenate guides; the rewrite must still emit the
// whole guide instead of aborting at the declaration.
func TestRewriteXMLTVIconsToToleratesLeadingDataAndRepeatedDeclarations(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
	decl := `<?xml version="1.0" encoding="UTF-8"?>`
	body := `<tv><channel id="1"><icon src="http://upstream.example.com/ch1.png"/></channel></tv>`
	for name, in := range map[string]string{
		"BOM":                  "\xef\xbb\xbf" + decl + body,
		"leading blank line":   "\n\r\n" + decl + body,
		"repeated declaration": decl + body + "\n" + decl,
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if err := c.rewriteXMLTVIconsTo(ctx, strings.NewReader(in), &out); err != nil {
				t.Fatalf("rewrite failed: %v", err)
			}
			if !strings.HasPrefix(out.String(), decl) {
				t.Errorf("output does not start with the declaration: %q", out.String())
			}
			if strings.Count(out.String(), "<?xml") != 1 {
				t.Errorf("want exactly one declaration, got %q", out.String())
			}
			if !strings.Contains(out.String(), `<channel id="1">`) || !strings.Contains(out.String(), "/img?url=") {
				t.Errorf("guide body missing or icon not rewritten: %q", out.String())
			}
		})
	}
}

func TestRewriteXMLTVIcons(t *testing.T) {
	c := testImageProxyConfig()
	ctx := testRequestContext()
	rewrite := func(t *testing.T, in string) string {
		t.Helper()
		var out strings.Builder
		if err := c.rewriteXMLTVIconsTo(ctx, strings.NewReader(in), &out); err != nil {
			t.Fatalf("rewrite failed: %v", err)
		}
		return out.String()
	}

	t.Run("channel-level icon src rewritten", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/ch1.png"/></channel></tv>`
		out := rewrite(t, in)
		want := "http://proxy.example.com:8080" + strings.ReplaceAll(c.proxyImagePath("http://upstream.example.com/ch1.png"), "&", "&amp;")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten channel icon %q", out, want)
		}
	})

	t.Run("programme-level icon src rewritten", func(t *testing.T) {
		in := `<tv><programme channel="1"><icon src="http://upstream.example.com/prog1.png"/></programme></tv>`
		out := rewrite(t, in)
		want := "http://proxy.example.com:8080" + strings.ReplaceAll(c.proxyImagePath("http://upstream.example.com/prog1.png"), "&", "&amp;")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain rewritten programme icon %q", out, want)
		}
	})

	t.Run("entity-escaped query string survives", func(t *testing.T) {
		in := `<tv><channel id="1"><icon src="http://upstream.example.com/a?b=1&amp;c=2"/></channel></tv>`
		out := rewrite(t, in)
		want := "http://proxy.example.com:8080" + strings.ReplaceAll(c.proxyImagePath("http://upstream.example.com/a?b=1&c=2"), "&", "&amp;")
		if !strings.Contains(out, want) {
			t.Errorf("output %q does not contain the correctly round-tripped rewritten icon %q", out, want)
		}
	})

	t.Run("element text that is a URL, src and href attributes go through /img", func(t *testing.T) {
		in := `<tv><programme channel="1"><title>See https://x.example</title>` +
			`<url>https://www.tvguide.example/p/1</url><image> https://img.example/p.jpg </image><url>not a link</url>` +
			`<review type="url" href="http://rev.example/r">http://rev.example/full</review></programme></tv>`
		out := rewrite(t, in)
		img := func(raw string) string {
			return "http://proxy.example.com:8080" + strings.ReplaceAll(c.proxyImagePath(raw), "&", "&amp;")
		}
		for _, want := range []string{
			"<url>" + img("https://www.tvguide.example/p/1") + "</url>",
			"<image>" + img("https://img.example/p.jpg") + "</image>",
			`href="` + img("http://rev.example/r") + `">` + img("http://rev.example/full") + "</review>",
			"<title>See https://x.example</title>", "<url>not a link</url>",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("output %q does not contain %q", out, want)
			}
		}
	})

	// NOTE: encoding/xml's Decoder/Encoder token round-trip does not
	// preserve self-closing tags (<x/> becomes <x></x>), so byte-identity
	// only holds for a document with no self-closing elements at all, not
	// just "no icon elements" as the design doc loosely put it.
	t.Run("document with no self-closing elements round-trips byte-identical", func(t *testing.T) {
		in := "<tv>\n  <channel id=\"1\">\n    <display-name>Channel One</display-name>\n  </channel>\n</tv>\n"
		out := rewrite(t, in)
		if out != in {
			t.Errorf("output = %q, want byte-identical %q", out, in)
		}
	})

	t.Run("malformed XML is an error", func(t *testing.T) {
		var out strings.Builder
		if err := c.rewriteXMLTVIconsTo(ctx, strings.NewReader(`<tv><channel id="1">`), &out); err == nil {
			t.Errorf("want an error for truncated XML, got output %q", out.String())
		}
	})
}
