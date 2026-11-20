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
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/session"
)

func TestParseFailoverInstances(t *testing.T) {
	two := `[{"name":"Spare","url":"http://spare.example.test","key":"k1"},{"name":"Third","url":"https://third.example.test/x","key":"k2"}]`
	got, err := parseFailoverInstances(two)
	if err != nil || len(got) != 2 || got[0].Name != "Spare" || got[1].Name != "Third" {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got, err := parseFailoverInstances(""); err != nil || len(got) != 0 {
		t.Fatalf("empty: %+v, %v", got, err)
	}
	for _, bad := range []string{
		"not json",
		`[{"name":"Spare","url":"http://spare.example.test"}]`,
		`[{"name":"Spare","url":"ftp://spare.example.test","key":"k"}]`,
	} {
		if _, err := parseFailoverInstances(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestFailoverTargets(t *testing.T) {
	setChannelIndex(t, map[string]string{"10": "NPO 1 HD"}, map[string]string{"10": "npo1.nl"})
	c := &Config{ProxyConfig: &config.ProxyConfig{InstanceName: "Main", FailoverInstances: `[` +
		`{"name":"Spare","url":"http://127.0.0.1:1/base","key":"test-key"},` +
		`{"name":"Third","url":"http://127.0.0.1:2","key":"other-key"}]`}}
	c.sessionManager = session.NewSessionManager(nil)
	t.Cleanup(c.sessionManager.Stop)
	c.setupFailover()

	for _, id := range []string{"10", "10.ts"} {
		ts := c.failoverTargets(id)
		if len(ts) != 2 || ts[0].Name != "Spare" || ts[1].Name != "Third" {
			t.Fatalf("%s: targets %+v", id, ts)
		}
		u := ts[0].URL
		if u.Path != "/base/api/failover/live" || u.Query().Get("epg") != "npo1.nl" || u.Query().Get("name") != "NPO 1 HD" {
			t.Fatalf("%s: url %s", id, u)
		}
		if ts[0].Header.Get(failoverKeyHeader) != "test-key" || ts[0].Header.Get(failoverFromHeader) != "Main" {
			t.Fatalf("headers %v", ts[0].Header)
		}
		if ts[1].URL.Path != "/api/failover/live" {
			t.Fatalf("third url %s", ts[1].URL)
		}
	}
	if ts := c.failoverTargets("99"); ts != nil {
		t.Fatalf("unknown channel: %+v", ts)
	}
}

// primaryFor is a household-serving server whose provider refuses with 456 and
// whose fallbacks are set to instances (empty = none).
func primaryFor(t *testing.T, instances string) *shareEnv {
	t.Helper()
	e := shareTestServer(t, 1, nil)
	e.p.streams = failoverCatalogue
	e.p.failStatus = 456
	e.c.InstanceName = "Main"
	e.c.FailoverInstances = instances
	e.c.setupFailover()
	return e
}

// Review Focus 3, whole chain: 456 on the primary is served by the fallback; a
// wrong key (401) or a fallback that does not lend (404) leaves the primary
// answering exactly as it does without fallbacks.
func TestFailoverEndToEnd(t *testing.T) {
	setChannelIndex(t,
		map[string]string{"10": "NPO 1 HD", "12": "RTL 4 FHD"},
		map[string]string{"10": "npo1.nl"})
	status := func(e *shareEnv) int {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.front.URL+"/household/10", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		return resp.StatusCode
	}
	control := status(primaryFor(t, ""))

	t.Run("served by the fallback", func(t *testing.T) {
		_, u := failoverServer(t, 1, false)
		base := u[:len(u)-len("/api/failover/live")]
		e := primaryFor(t, `[{"name":"Spare","url":"`+base+`","key":"test-key"}]`)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		code, n, _ := watch(t, ctx, e.front.URL+"/household/10")
		if code != http.StatusOK {
			t.Fatalf("status %d, want 200", code)
		}
		if !eventually(5*time.Second, func() bool {
			for _, s := range e.c.sessionManager.GetAllStreams() {
				if s.GetVia() == "Spare" {
					return true
				}
			}
			return false
		}) {
			t.Fatal("no stream session with Via Spare")
		}
		if !eventually(3*time.Second, func() bool { return atomic.LoadInt64(n) > 0 }) {
			t.Fatal("no fallback bytes")
		}
	})
	for name, noKey := range map[string]bool{"wrong key": false, "fallback without key": true} {
		t.Run(name, func(t *testing.T) {
			_, u := failoverServer(t, 1, noKey)
			base := u[:len(u)-len("/api/failover/live")]
			e := primaryFor(t, `[{"name":"Spare","url":"`+base+`","key":"wrong-key"}]`)
			if got := status(e); got != control {
				t.Fatalf("status %d, want %d as without fallbacks", got, control)
			}
		})
	}
}
