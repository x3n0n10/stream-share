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
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/session"
)

func TestProviderStreamURLEscapesCredentials(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{
		XtreamBaseURL: "http://prov.example:8080/", XtreamUser: "u s", XtreamPassword: "a#b/c?d%",
	}}
	cases := []struct {
		kind string
		segs []string
	}{
		{"", []string{"123.ts"}},
		{"live", []string{"123.ts"}},
		{"movie", []string{"123.ts"}},
		{"series", []string{"123.ts"}},
		{"timeshift", []string{"60", "2026-11-08:20-00", "123.ts"}},
	}
	for _, tc := range cases {
		got := c.providerStreamURL(tc.kind, tc.segs...)
		u, err := url.Parse(got)
		if err != nil {
			t.Fatalf("%q: parse %q: %v", tc.kind, got, err)
		}
		if u.Fragment != "" || u.RawQuery != "" {
			t.Errorf("%q: fragment/query leaked in %q", tc.kind, got)
		}
		var want []string
		if tc.kind != "" {
			want = append(want, tc.kind)
		}
		want = append(want, "u s", "a#b/c?d%")
		want = append(want, tc.segs...)
		var gotSegs []string
		for _, s := range strings.Split(strings.TrimPrefix(u.EscapedPath(), "/"), "/") {
			d, err := url.PathUnescape(s)
			if err != nil {
				t.Fatalf("%q: unescape %q: %v", tc.kind, s, err)
			}
			gotSegs = append(gotSegs, d)
		}
		if !reflect.DeepEqual(gotSegs, want) {
			t.Errorf("%q: segments %q, want %q", tc.kind, gotSegs, want)
		}
	}
}

func TestLiveStreamReachesProviderWithEscapedPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var mu sync.Mutex
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = r.URL.EscapedPath()
		mu.Unlock()
		_, _ = w.Write([]byte("x"))
	}))
	t.Cleanup(up.Close)

	sm := session.NewSessionManager(nil)
	t.Cleanup(sm.Stop)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			User: "me", Password: "pw",
			XtreamBaseURL: up.URL, XtreamUser: "user", XtreamPassword: "a#b",
		},
		sessionManager: sm,
	}
	r := gin.New()
	r.GET("/live/:username/:password/:id", c.authWithPathCredentials(), c.xtreamProxyCredentialsLiveStreamHandler)

	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	resp, err := http.Get(front.URL + "/live/me/pw/42.ts")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	mu.Lock()
	defer mu.Unlock()
	if want := "/live/user/a%23b/42.ts"; seen != want {
		t.Fatalf("provider saw %q (status %d), want %q", seen, resp.StatusCode, want)
	}
}

func TestHlsrUpstreamCarriesEscapedPassword(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var seen string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.URL.EscapedPath()
	}))
	t.Cleanup(up.Close)
	upURL, _ := url.Parse(up.URL)
	hlsChannelsRedirectURLLock.Lock()
	hlsChannelsRedirectURL["chan1.m3u8"] = *upURL
	hlsChannelsRedirectURLLock.Unlock()
	t.Cleanup(resetHlsRedirectCache)

	c := &Config{ProxyConfig: &config.ProxyConfig{XtreamUser: "user", XtreamPassword: "a#b"}}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/hlsr/tok/user/a%23b/chan1/h/seg.ts", nil)
	ctx.Params = gin.Params{{Key: "token", Value: "tok"}, {Key: "channel", Value: "chan1"}, {Key: "hash", Value: "h"}, {Key: "chunk", Value: "seg.ts"}}
	c.xtreamHlsrStream(ctx)

	if want := "/hlsr/tok/user/a%23b/chan1/h/seg.ts"; seen != want {
		t.Fatalf("provider saw %q, want %q", seen, want)
	}
}
