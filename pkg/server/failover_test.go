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
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/session"
)

// failoverServer is shareTestServer plus the failover route, keyed "test-key"
// unless noKey, on a catalogue where channel 10 is "NPO 1 HD".
func failoverServer(t *testing.T, capacity int, noKey bool) (*shareEnv, string) {
	t.Helper()
	e := shareTestServer(t, capacity, nil)
	e.p.streams = failoverCatalogue
	if !noKey {
		e.c.FailoverKey = "test-key"
	}
	setChannelIndex(t,
		map[string]string{"10": "NPO 1 HD", "12": "RTL 4 FHD"},
		map[string]string{"10": "npo1.nl"})
	r := gin.New()
	r.GET("/api/failover/live", e.c.handleFailoverLive)
	front := httptest.NewServer(r)
	t.Cleanup(front.Close)
	return e, front.URL + "/api/failover/live"
}

func failoverGet(t *testing.T, ctx context.Context, url, key, from string) *http.Response {
	t.Helper()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if key != "" {
		req.Header.Set(failoverKeyHeader, key)
	}
	req.Header.Set(failoverFromHeader, from)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestFailoverEndpointRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, key, query string
		noKey            bool
		want             int
	}{
		{"no key configured", "test-key", "?epg=npo1.nl", true, http.StatusNotFound},
		{"missing key", "", "?epg=npo1.nl", false, http.StatusUnauthorized},
		{"wrong key", "nope", "?epg=npo1.nl", false, http.StatusUnauthorized},
		{"no match", "test-key", "?epg=none.nl&name=Nothing", false, http.StatusNotFound},
	} {
		_, u := failoverServer(t, 1, tc.noKey)
		if got := failoverGet(t, context.Background(), u+tc.query, tc.key, "Main").StatusCode; got != tc.want {
			t.Errorf("%s: status %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestFailoverCaller(t *testing.T) {
	for in, want := range map[string]string{
		"Ma\nin<b>":             "Mainb",
		"":                      "instance",
		"<>":                    "instance",
		"Main.1_x-y z":          "Main.1_x-y z",
		strings.Repeat("é", 70): strings.Repeat("é", 64),
	} {
		if got := failoverCaller(in); got != want {
			t.Errorf("failoverCaller(%q) = %q, want %q", in, got, want)
		}
	}
	// The raw header reaches the handler as is.
	e, _ := failoverServer(t, 1, false)
	r := gin.New()
	r.GET("/f", e.c.handleFailoverLive)
	req := httptest.NewRequest(http.MethodGet, "/f?epg=npo1.nl", nil)
	req.Header.Set(failoverKeyHeader, "test-key")
	req.Header["X-Failover-From"] = []string{"Ma\nin<b>"}
	ctx, cancel := context.WithCancel(context.Background())
	req = req.WithContext(ctx)
	done := make(chan struct{})
	go func() { defer close(done); r.ServeHTTP(closeNotifyRecorder{httptest.NewRecorder()}, req) }()
	if !eventually(3*time.Second, func() bool {
		return e.c.sessionManager.GetUserSession(session.FailoverIdentityPrefix+"Mainb/10") != nil
	}) {
		t.Fatal("viewer should be named after the sanitised caller")
	}
	cancel()
	<-done
}

func TestFailoverEndpointStreams(t *testing.T) {
	e, u := failoverServer(t, 1, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	resp := failoverGet(t, ctx, u+"?epg=npo1.nl&name=NPO+1", "test-key", "Main")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	buf := make([]byte, 188)
	if n, _ := resp.Body.Read(buf); n == 0 || buf[0] != 0x47 {
		t.Fatalf("no provider bytes: %d %x", n, buf[0])
	}
	if e.c.sessionManager.GetUserSession(session.FailoverIdentityPrefix+"Main/10") == nil {
		t.Fatal("no viewer failover:Main/10")
	}
}

// Review Focus 1: the provider's refusal reaches the caller as its status, with
// no slate bytes, even though this instance slates its own viewers.
func TestFailoverEndpointNeverSlates(t *testing.T) {
	e, u := failoverServer(t, 1, false)
	enableSlates(t, e)
	e.p.failStatus = 456
	resp := failoverGet(t, context.Background(), u+"?epg=npo1.nl", "test-key", "Main")
	body := make([]byte, 16)
	n, _ := resp.Body.Read(body)
	if resp.StatusCode != 456 || n != 0 {
		t.Fatalf("status %d with %d body bytes, want 456 and none", resp.StatusCode, n)
	}
}

func TestFailoverEndpointBusyWhileHouseholdHoldsTheConnection(t *testing.T) {
	e, u := failoverServer(t, 1, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, mine, _ := watch(t, ctx, e.front.URL+"/household/12")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return atomic.LoadInt64(mine) > 0 }) {
		t.Fatalf("household stream: %d", code)
	}
	if got := failoverGet(t, ctx, u+"?epg=npo1.nl", "test-key", "Main").StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", got)
	}
}

// Review Focus 1: joining a household stream that shows its slate is a failure
// for the caller, not a 200 carrying the slate.
func TestFailoverEndpointRefusesASlatingHouseholdStream(t *testing.T) {
	e, u := failoverServer(t, 1, false)
	enableSlates(t, e)
	e.p.failStatus = 456
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	code, _, _ := watch(t, ctx, e.front.URL+"/household/10")
	if code != http.StatusOK || !eventually(3*time.Second, func() bool { return e.c.sessionManager.Slating("10") }) {
		t.Fatalf("household should be slating (status %d)", code)
	}
	if got := failoverGet(t, ctx, u+"?epg=npo1.nl", "test-key", "Main").StatusCode; got != 456 {
		t.Fatalf("status %d, want 456", got)
	}
}

func enableSlates(t *testing.T, e *shareEnv) {
	t.Helper()
	clip := filepath.Join(t.TempDir(), "slate.ts")
	if err := os.WriteFile(clip, bytes.Repeat([]byte("SLATE"), 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	e.c.sessionManager.SetSlateProvider(catchupSlate{clip})
	e.c.sessionManager.SetSlateRetryMax(20 * time.Second)
}
