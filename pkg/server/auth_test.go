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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
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
