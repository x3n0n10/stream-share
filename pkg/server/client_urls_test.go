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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestClientSafeValueRewritesEveryURLField(t *testing.T) {
	c := testImageProxyConfig()
	base := "https://ss.example"
	urls := map[string]string{
		"stream_icon": "http://lg.maxnow.cx:8080/logo/ucl.png",
		"cover":       "https://image.tmdb.org/t/p/w600/a.jpg",
	}
	in := map[string]interface{}{
		"stream_icon":   urls["stream_icon"],
		"cover":         urls["cover"],
		"backdrop_path": []interface{}{"https://image.tmdb.org/t/p/b.jpg"},
		"info": map[string]interface{}{
			"cover_big":     "http://lg.maxnow.cx:8080/c.jpg",
			"kinopoisk_url": "https://www.kinopoisk.ru/film/1",
		},
		"seasons":       []interface{}{map[string]interface{}{"cover_big": "https://image.tmdb.org/s.jpg"}},
		"direct_source": "http://prov/x.ts",
		"info2":         map[string]interface{}{"direct_source": "http://prov/y.ts"},
		"plot":          "See https://example.com for more",
		"num":           json.Number("3"),
	}
	want := func(u string) string { return base + c.proxyImagePath(u) }
	wantOut := map[string]interface{}{
		"stream_icon":   want(urls["stream_icon"]),
		"cover":         want(urls["cover"]),
		"backdrop_path": []interface{}{want("https://image.tmdb.org/t/p/b.jpg")},
		"info": map[string]interface{}{
			"cover_big":     want("http://lg.maxnow.cx:8080/c.jpg"),
			"kinopoisk_url": want("https://www.kinopoisk.ru/film/1"),
		},
		"seasons": []interface{}{map[string]interface{}{"cover_big": want("https://image.tmdb.org/s.jpg")}},
		"info2":   map[string]interface{}{},
		"plot":    "See https://example.com for more",
		"num":     json.Number("3"),
	}
	if got := c.clientSafeValue(base, in); !reflect.DeepEqual(got, wantOut) {
		t.Errorf("got %v\nwant %v", got, wantOut)
	}
}

func TestClientSafeValueDoesNotMutateInput(t *testing.T) {
	c := testImageProxyConfig()
	mk := func() map[string]interface{} {
		return map[string]interface{}{
			"cover":         "http://a.b/c.png",
			"direct_source": "http://prov/x.ts",
			"list":          []interface{}{"http://a.b/d.png", map[string]interface{}{"direct_source": "x"}},
		}
	}
	in := mk()
	c.clientSafeValue("https://ss.example", in)
	if !reflect.DeepEqual(in, mk()) {
		t.Errorf("input mutated: %v", in)
	}
}

func TestIsFetchableURL(t *testing.T) {
	for s, want := range map[string]bool{
		"http://a.b/c.png": true, "https://a.b:8080/x?y=1": true,
		"http://prov/logos/Sky Sports.png": true, " http://a.b/c.png ": true,
		"See https://x.com": false, "http://": false, "https:// broken": false,
		"ftp://x/y": false, "http://a b/c": false, "/relative.png": false, "": false,
	} {
		if got := isFetchableURL(s); got != want {
			t.Errorf("isFetchableURL(%q) = %v, want %v", s, got, want)
		}
	}
}

func TestClientSafeValueKeepsOwnImgLinks(t *testing.T) {
	for _, end := range []string{"", "/custom/"} {
		c := testImageProxyConfig()
		c.CustomEndpoint = end
		own := "https://ss.example" + c.proxyImagePath("http://x/y.png")
		if got := c.clientSafeValue("https://ss.example", own); got != own {
			t.Errorf("endpoint %q: own link rewritten to %v", end, got)
		}
	}
}

func TestClientSafeValueKeepsKnownShapes(t *testing.T) {
	c := testImageProxyConfig()
	input := map[string]interface{}{
		"movie_image":   "http://upstream.example.com/cover.jpg",
		"direct_source": "http://upstream.example.com/stream.mp4",
		"backdrop_path": []interface{}{
			"http://upstream.example.com/bd1.jpg",
			"http://upstream.example.com/bd2.jpg",
		},
		"name": "Show One",
		"episodes": map[string]interface{}{
			"1": []interface{}{
				map[string]interface{}{
					"id": "101",
					"info": map[string]interface{}{
						"movie_image": "http://upstream.example.com/ep101.jpg",
					},
				},
			},
		},
	}

	got, ok := c.clientSafeValue("http://proxy.example.com:8080", input).(map[string]interface{})
	if !ok {
		t.Fatalf("clientSafeValue did not return a map[string]interface{}")
	}

	wantMovieImage := "http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/cover.jpg")
	if got["movie_image"] != wantMovieImage {
		t.Errorf("movie_image = %v, want %v", got["movie_image"], wantMovieImage)
	}
	if _, exists := got["direct_source"]; exists {
		t.Errorf("direct_source should be stripped, still present: %v", got["direct_source"])
	}
	bd, ok := got["backdrop_path"].([]interface{})
	if !ok || len(bd) != 2 {
		t.Fatalf("backdrop_path missing or wrong shape: %v", got["backdrop_path"])
	}
	wantBd0 := "http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/bd1.jpg")
	if bd[0] != wantBd0 {
		t.Errorf("backdrop_path[0] = %v, want %v", bd[0], wantBd0)
	}
	if got["name"] != "Show One" {
		t.Errorf("unrelated field name changed: %v", got["name"])
	}

	episodes, ok := got["episodes"].(map[string]interface{})
	if !ok {
		t.Fatalf("episodes missing or wrong shape: %v", got["episodes"])
	}
	season1, ok := episodes["1"].([]interface{})
	if !ok || len(season1) != 1 {
		t.Fatalf("episodes[1] missing or wrong shape: %v", episodes["1"])
	}
	ep, ok := season1[0].(map[string]interface{})
	if !ok {
		t.Fatalf("episode 0 wrong shape: %v", season1[0])
	}
	epInfo, ok := ep["info"].(map[string]interface{})
	if !ok {
		t.Fatalf("episode info wrong shape: %v", ep["info"])
	}
	wantEpImage := "http://proxy.example.com:8080" + c.proxyImagePath("http://upstream.example.com/ep101.jpg")
	if epInfo["movie_image"] != wantEpImage {
		t.Errorf("nested episode movie_image = %v, want %v", epInfo["movie_image"], wantEpImage)
	}
}

// A URL with a space in its path is still a link a player would fetch from
// the provider, so it is rewritten too (trimmed of surrounding blanks).
func TestClientSafeValueRewritesURLsWithSpaces(t *testing.T) {
	c := testImageProxyConfig()
	for in, raw := range map[string]string{
		"http://prov/logos/Sky Sports.png": "http://prov/logos/Sky Sports.png",
		"http://prov/l.png ":               "http://prov/l.png",
	} {
		if got, want := c.clientSafeValue("https://ss.example", in), "https://ss.example"+c.proxyImagePath(raw); got != want {
			t.Errorf("clientSafeValue(%q) = %v, want %v", in, got, want)
		}
	}
}

// A provider URL carrying the provider's credentials maps onto this server's
// own route with the local ones, never into an /img link; a provider logo
// without credentials still goes through /img.
func TestClientSafeValueLocalizesProviderCredentialURLs(t *testing.T) {
	c := providerHostConfig()
	got := c.clientSafeValue("https://ss.example", map[string]interface{}{
		"stream_url": "http://prov:8080/movie/PU/PP/77.mkv",
		"icon":       "http://prov:8080/images/l.png",
	}).(map[string]interface{})
	if want := "https://ss.example/anti/movie/localuser/localpass/77.mkv"; got["stream_url"] != want {
		t.Errorf("stream_url = %v, want %v", got["stream_url"], want)
	}
	if want := "https://ss.example" + c.proxyImagePath("http://prov:8080/images/l.png"); got["icon"] != want {
		t.Errorf("icon = %v, want %v", got["icon"], want)
	}
	if strings.Contains(fmt.Sprint(got), "PP") {
		t.Errorf("provider credentials leaked: %v", got)
	}
}
