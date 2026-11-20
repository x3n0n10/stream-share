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
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/provider"
	"github.com/lucasduport/stream-share/pkg/session"
)

func TestHealthzReportsReadiness(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{}}

	call := func() int {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/healthz", nil)
		c.healthz(ctx)
		return w.Code
	}

	if got := call(); got != http.StatusServiceUnavailable {
		t.Errorf("before ready: got %d, want 503", got)
	}
	atomic.StoreInt32(&c.ready, 1)
	if got := call(); got != http.StatusOK {
		t.Errorf("after ready: got %d, want 200", got)
	}
}

func TestIsBlockedStatus(t *testing.T) {
	cases := []struct {
		name   string
		codes  string
		status int
		want   bool
	}{
		{"default matches 456", "", 456, true},
		{"default rejects 403", "", 403, false},
		{"custom single", "455", 455, true},
		{"custom rejects default when overridden", "455", 456, false},
		{"custom list with spaces", " 456 , 461 ", 461, true},
		{"zero status never blocked", "456", 0, false},
		{"malformed entries ignored", "abc,456", 456, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{ProxyConfig: &config.ProxyConfig{HealthCheckBlockedCodes: tc.codes}}
			if got := c.isBlockedStatus(tc.status); got != tc.want {
				t.Errorf("isBlockedStatus(%d) with codes %q = %v, want %v", tc.status, tc.codes, got, tc.want)
			}
		})
	}
}

func TestParseDailyTimes(t *testing.T) {
	got := parseDailyTimes(" 04:00, 16:00 ,,bad,25:00,12:61,08:30")
	want := []dailyTime{{4, 0}, {16, 0}, {8, 30}}
	if len(got) != len(want) {
		t.Fatalf("parseDailyTimes returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestNextDailyTime(t *testing.T) {
	times := []dailyTime{{4, 0}, {16, 0}}
	loc := time.UTC

	cases := []struct {
		name string
		now  time.Time
		want time.Time
	}{
		{
			name: "before first",
			now:  time.Date(2026, 8, 3, 2, 30, 0, 0, loc),
			want: time.Date(2026, 8, 3, 4, 0, 0, 0, loc),
		},
		{
			name: "between the two",
			now:  time.Date(2026, 8, 3, 9, 0, 0, 0, loc),
			want: time.Date(2026, 8, 3, 16, 0, 0, 0, loc),
		},
		{
			name: "after last rolls to tomorrow",
			now:  time.Date(2026, 8, 3, 20, 0, 0, 0, loc),
			want: time.Date(2026, 8, 4, 4, 0, 0, 0, loc),
		},
		{
			name: "exactly at a time picks the next one",
			now:  time.Date(2026, 8, 3, 4, 0, 0, 0, loc),
			want: time.Date(2026, 8, 3, 16, 0, 0, 0, loc),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nextDailyTime(tc.now, times); !got.Equal(tc.want) {
				t.Errorf("nextDailyTime(%v) = %v, want %v", tc.now, got, tc.want)
			}
		})
	}
}

func TestClassifyFailuresPrefersBlocked(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	status, code, _ := c.classifyFailures([]*session.UpstreamError{{StatusCode: 503, Key: "503"}, {StatusCode: 456, Key: "456"}})
	if status != healthStatusBlocked || code != "456" {
		t.Errorf("got %s/%s, want blocked/456", status, code)
	}
	status, code, _ = c.classifyFailures([]*session.UpstreamError{{StatusCode: 503, Key: "503"}})
	if status != healthStatusError || code != "503" {
		t.Errorf("got %s/%s, want error/503", status, code)
	}
}

// probeHarness returns a Config whose probe upstream counts requests and
// answers with the given status (0 = stream forever).
func probeHarness(t *testing.T, upStatus int) (*Config, *session.SessionManager, *int64) {
	t.Helper()
	var hits int64
	stop := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if upStatus != 0 {
			w.WriteHeader(upStatus)
			return
		}
		for r.Context().Err() == nil {
			select {
			case <-stop:
				return
			default:
			}
			_, _ = w.Write([]byte("data"))
			w.(http.Flusher).Flush()
			time.Sleep(10 * time.Millisecond)
		}
	}))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(stop) }) // runs before up.Close so streaming handlers exit
	sm := session.NewSessionManager(nil)
	t.Cleanup(sm.Stop)
	c := &Config{
		ProxyConfig: &config.ProxyConfig{
			XtreamBaseURL: up.URL, XtreamUser: "u", XtreamPassword: "p",
			HealthCheckStreamID: "42", HealthCheckTimeoutSeconds: 5,
		},
		sessionManager: sm,
	}
	return c, sm, &hits
}

func TestProbeReportsActiveStreamHealthy(t *testing.T) {
	c, sm, hits := probeHarness(t, 0)
	u, _ := url.Parse(c.XtreamBaseURL + "/live/u/p/7")
	buf, key, err := sm.RequestStream("viewer", "s1", "live", "S1", u)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := sm.GetClientChannel("s1", key)
	go func() {
		for range ch {
		}
	}()
	select {
	case <-buf.Ready():
	case <-time.After(3 * time.Second):
		t.Fatal("stream never ready")
	}
	before := atomic.LoadInt64(hits)
	status, _, detail := c.runProviderProbe()
	if status != healthStatusHealthy || !strings.Contains(detail, "serving 1") {
		t.Errorf("got %s %q", status, detail)
	}
	if got := atomic.LoadInt64(hits); got != before {
		t.Errorf("probe dialed upstream (%d -> %d)", before, got)
	}
}

func TestProbeBusyIsHealthy(t *testing.T) {
	c, sm, hits := probeHarness(t, http.StatusOK)
	slots := provider.New(func() int { return 1 })
	sm.SetProviderSlots(slots)
	lease, err := slots.AcquireClass(context.Background(), "viewer", func() provider.Class { return provider.Foreground }, provider.Fresh)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	status, _, detail := c.runProviderProbe()
	if status != healthStatusHealthy || detail != "provider connection in use" {
		t.Errorf("got %s %q", status, detail)
	}
	if got := atomic.LoadInt64(hits); got != 0 {
		t.Errorf("upstream requests = %d, want 0", got)
	}
}

func TestProbeIdleDialsProvider(t *testing.T) {
	c, _, hits := probeHarness(t, 456)
	status, code, _ := c.runProviderProbe()
	if status != healthStatusBlocked || code != "456" {
		t.Errorf("got %s/%s", status, code)
	}
	if got := atomic.LoadInt64(hits); got != 1 {
		t.Errorf("upstream requests = %d, want 1", got)
	}
}
