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
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/session"
)

// pathAuthRig wires authWithPathCredentials in front of a handler that records
// ctx.GetString("username") into ctxUsername, with a real SessionManager
// (nil DB), so tests can inspect which identity got registered.
type pathAuthRig struct {
	router      *gin.Engine
	sm          *session.SessionManager
	ctxUsername string
}

func newPathAuthRig(t *testing.T, pc *config.ProxyConfig) *pathAuthRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	sm := session.NewSessionManager(nil)
	t.Cleanup(sm.Stop)

	c := &Config{ProxyConfig: pc, sessionManager: sm}
	router := gin.New()
	rig := &pathAuthRig{sm: sm}
	router.GET("/live/:username/:password/:id", c.authWithPathCredentials(), func(ctx *gin.Context) {
		rig.ctxUsername = ctx.GetString("username")
		ctx.Status(http.StatusOK)
	})
	rig.router = router
	return rig
}

// get performs GET /live/<user>/<pass>/1 as though it came from remoteIP and
// returns the response status.
func (r *pathAuthRig) get(user, pass, remoteIP string) int {
	req := httptest.NewRequest(http.MethodGet, "/live/"+user+"/"+pass+"/1", nil)
	req.RemoteAddr = remoteIP + ":40000"
	w := httptest.NewRecorder()
	r.router.ServeHTTP(w, req)
	return w.Code
}

// sessionIPs returns registered session identity -> recorded IPAddress.
func (r *pathAuthRig) sessionIPs() map[string]string {
	out := map[string]string{}
	for _, s := range r.sm.GetAllSessions() {
		out[s.Username] = s.IPAddress
	}
	return out
}

func TestPathCredentialsIdentityIsClientIPWhenLDAPDisabled(t *testing.T) {
	rig := newPathAuthRig(t, &config.ProxyConfig{
		User:     "custom-login",
		Password: "custom-pass",
	})

	for _, ip := range []string{"198.51.100.1", "198.51.100.2"} {
		if got := rig.get("custom-login", "custom-pass", ip); got != http.StatusOK {
			t.Fatalf("status from %s = %d, want 200", ip, got)
		}
	}

	got := rig.sessionIPs()
	want := map[string]string{
		"198.51.100.1": "198.51.100.1",
		"198.51.100.2": "198.51.100.2",
	}
	if len(got) != len(want) {
		t.Fatalf("sessions = %v, want exactly %v", got, want)
	}
	for id, ip := range want {
		if got[id] != ip {
			t.Errorf("sessions = %v, want %v", got, want)
			break
		}
	}
	if _, shared := got["custom-login"]; shared {
		t.Errorf("custom login must not be a session key, got %v", got)
	}

	if rig.ctxUsername != "198.51.100.2" {
		t.Errorf("ctxUsername = %q, want %q", rig.ctxUsername, "198.51.100.2")
	}
}

func TestPathCredentialsBadPasswordRegistersNoSession(t *testing.T) {
	rig := newPathAuthRig(t, &config.ProxyConfig{
		User:     "custom-login",
		Password: "custom-pass",
	})

	if got := rig.get("custom-login", "wrong", "198.51.100.1"); got != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", got)
	}
	if got := rig.sessionIPs(); len(got) != 0 {
		t.Errorf("sessions = %v, want none", got)
	}
}

func TestPathCredentialsIdentityIsLDAPUsernameWhenLDAPEnabled(t *testing.T) {
	clearLDAPCache()
	defer clearLDAPCache()

	rig := newPathAuthRig(t, &config.ProxyConfig{
		LDAPEnabled:          true,
		LDAPAuthCacheMinutes: 5,
	})

	// Pre-seed a successful auth so no directory is contacted (same technique
	// as TestLDAPCacheHitAvoidsDirectory).
	key, ok := ldapCacheKey("carol", "secret")
	if !ok {
		t.Fatal("no cache key")
	}
	ldapAuthCache.Store(key, time.Now().Add(time.Minute))

	if got := rig.get("carol", "secret", "198.51.100.9"); got != http.StatusOK {
		t.Fatalf("status = %d, want 200", got)
	}

	got := rig.sessionIPs()
	if len(got) != 1 || got["carol"] != "198.51.100.9" {
		t.Errorf("sessions = %v, want {carol: 198.51.100.9}", got)
	}

	if rig.ctxUsername != "carol" {
		t.Errorf("ctxUsername = %q, want %q", rig.ctxUsername, "carol")
	}
}
