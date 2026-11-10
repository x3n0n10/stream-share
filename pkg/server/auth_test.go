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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
)

func TestRequireRecentAuth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	call := func(remoteAddr string) int {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/img", nil)
		ctx.Request.RemoteAddr = remoteAddr
		requireRecentAuth(ctx)
		return w.Code
	}

	t.Run("rejects an IP that never authenticated", func(t *testing.T) {
		if got := call("198.51.100.1:1234"); got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}
	})

	t.Run("allows an IP recorded within the TTL", func(t *testing.T) {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/img", nil)
		ctx.Request.RemoteAddr = "198.51.100.2:1234"

		recordRecentAuth(ctx)
		requireRecentAuth(ctx)

		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200 (recorder default, since requireRecentAuth does nothing on success)", w.Code)
		}
	})

	t.Run("rejects an IP whose last auth is past the TTL", func(t *testing.T) {
		ip := "198.51.100.3"
		recentlyAuthenticatedMu.Lock()
		recentlyAuthenticated[ip] = time.Now().Add(-recentAuthTTL - time.Minute)
		recentlyAuthenticatedMu.Unlock()

		if got := call(ip + ":1234"); got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}

		recentlyAuthenticatedMu.Lock()
		_, stillPresent := recentlyAuthenticated[ip]
		recentlyAuthenticatedMu.Unlock()
		if stillPresent {
			t.Errorf("expired entry for %s was not pruned", ip)
		}
	})
}

func TestCredentialsValidLocal(t *testing.T) {
	c := &Config{ProxyConfig: &config.ProxyConfig{User: "u", Password: "p"}}
	for _, tc := range []struct {
		name, user, pass string
		want             bool
	}{
		{"right", "u", "p", true},
		{"wrong user", "x", "p", false},
		{"wrong pass", "u", "x", false},
		{"both empty", "", "", false},
	} {
		if got := c.credentialsValid(tc.user, tc.pass); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	// An operator who configures empty local credentials must not accept empty requests.
	empty := &Config{ProxyConfig: &config.ProxyConfig{}}
	if empty.credentialsValid("", "") {
		t.Error("empty configured credentials accepted an empty request")
	}
}

func TestSweepRecentAuth(t *testing.T) {
	now := time.Now()
	recentlyAuthenticatedMu.Lock()
	recentlyAuthenticated = map[string]time.Time{"old": now.Add(-25 * time.Hour), "new": now.Add(-time.Hour)}
	recentlyAuthenticatedMu.Unlock()

	sweepRecentAuth(now)

	recentlyAuthenticatedMu.Lock()
	defer recentlyAuthenticatedMu.Unlock()
	if _, ok := recentlyAuthenticated["old"]; ok {
		t.Error("25h-old entry not swept")
	}
	if _, ok := recentlyAuthenticated["new"]; !ok {
		t.Error("1h-old entry swept")
	}
}
