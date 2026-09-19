# Custom Credentials Client-IP Identity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When LDAP is disabled, viewers using the custom proxy credentials are identified by client IP (sessions, history, `/status`, aliases), same as viewers using provider credentials.

**Architecture:** One change in `authWithPathCredentials` (`pkg/server/server.go`): pick a tracking `identity` (LDAP username if LDAP on, otherwise the client IP) and use it for `RegisterUser` and `ctx.Set("username", …)`. `resolveRequestUsername` already reads the context value first, so downstream handlers need no change.

**Tech Stack:** Go 1.23, gin v1.9.0 (`gin.CreateTestContext` / `httptest`), stdlib `testing`. Dependencies are vendored (`vendor/`), no `go get` needed.

## Global Constraints

Copied from the spec ([2026-09-19-custom-credentials-client-ip-identity-design.md](../specs/2026-09-19-custom-credentials-client-ip-identity-design.md)):

- LDAP-enabled behavior is unchanged: session keyed by the LDAP username.
- Credential validation logic is unchanged (path `username`/`password` still compared against `c.User`/`c.Password`).
- No migration of existing `stream_history` rows.
- No new API fields, no new columns, no changes to `stream-share-suite`.
- Custom login name stays in existing `DebugLog` lines and is not otherwise recorded.

Work happens in the worktree `/Users/jorislankhorst/Source/stream-share-wt-ip-identity` on branch `claude/custom-creds-client-ip`. Run all commands from that directory.

---

### Task 1: Use client IP as identity in `authWithPathCredentials`

**Files:**
- Create: `pkg/server/auth_path_credentials_test.go`
- Modify: `pkg/server/server.go` (`authWithPathCredentials`, currently ~lines 533-569)

**Interfaces:**
- Consumes (all already exist):
  - `(*Config).authWithPathCredentials() gin.HandlerFunc`
  - `session.NewSessionManager(db *database.DBManager) *session.SessionManager` (`nil` db is fine, see `pkg/session/multiplex_test.go:120`)
  - `(*SessionManager).GetAllSessions() []*types.UserSession`, `Stop()`
  - `types.UserSession{Username, IPAddress string, …}`
  - `ldapCacheKey(username, password string) (string, bool)`, `ldapAuthCache` (a `sync.Map`), `clearLDAPCache()` (helper in `pkg/server/auth_cache_test.go`, same package)
  - `config.ProxyConfig{User, Password config.CredentialString; LDAPEnabled bool; LDAPAuthCacheMinutes int}`
- Produces: nothing new. No signature changes.

- [ ] **Step 1: Write the tests**

Create `pkg/server/auth_path_credentials_test.go`:

```go
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

// pathAuthRig wires authWithPathCredentials in front of a no-op handler with a
// real SessionManager (nil DB), so tests can inspect which identity got
// registered.
type pathAuthRig struct {
	router *gin.Engine
	sm     *session.SessionManager
}

func newPathAuthRig(t *testing.T, pc *config.ProxyConfig) *pathAuthRig {
	t.Helper()
	gin.SetMode(gin.TestMode)

	sm := session.NewSessionManager(nil)
	t.Cleanup(sm.Stop)

	c := &Config{ProxyConfig: pc, sessionManager: sm}
	router := gin.New()
	router.GET("/live/:username/:password/:id", c.authWithPathCredentials(), func(ctx *gin.Context) {
		ctx.Status(http.StatusOK)
	})
	return &pathAuthRig{router: router, sm: sm}
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
}
```

- [ ] **Step 2: Run tests, verify the IP test fails**

Run: `go test ./pkg/server -run 'TestPathCredentials' -v`

Expected:
- `TestPathCredentialsIdentityIsClientIPWhenLDAPDisabled` **FAIL** with `sessions = map[custom-login:198.51.100.2], want exactly map[198.51.100.1:198.51.100.1 198.51.100.2:198.51.100.2]` (one shared session, last IP wins).
- `TestPathCredentialsBadPasswordRegistersNoSession` PASS (regression guard).
- `TestPathCredentialsIdentityIsLDAPUsernameWhenLDAPEnabled` PASS (regression guard).

If the LDAP test fails instead with a compile error about `LDAPAuthCacheMinutes` or a real LDAP dial attempt, check `pkg/config` field names and `ldapAuthenticateCached` in `pkg/server/auth_cache.go` before continuing.

- [ ] **Step 3: Implement the change**

In `pkg/server/server.go`, inside `authWithPathCredentials`, replace:

```go
		// Register or update the user session and set username in context for later logs
		if c.sessionManager == nil {
			utils.ErrorLog("authWithPathCredentials: sessionManager is NIL - cannot register user session")
		} else {
			c.sessionManager.RegisterUser(username, ip, userAgent)
			utils.InfoLog("authWithPathCredentials: session registered for user=%s ip=%s", username, ip)
		}
		ctx.Set("username", username)
```

with:

```go
		// Track the viewer by client IP unless LDAP is on: a shared local login
		// would otherwise collapse every device into one session/history
		// identity, and IP aliases could never apply. Matches the provider-
		// credentials routes, where resolveRequestUsername falls back to the IP.
		identity := username
		if !c.LDAPEnabled {
			identity = ip
		}

		// Register or update the user session and set identity in context for later logs
		if c.sessionManager == nil {
			utils.ErrorLog("authWithPathCredentials: sessionManager is NIL - cannot register user session")
		} else {
			c.sessionManager.RegisterUser(identity, ip, userAgent)
			utils.InfoLog("authWithPathCredentials: session registered for user=%s ip=%s", username, ip)
		}
		ctx.Set("username", identity)
```

- [ ] **Step 4: Run the new tests, verify all pass**

Run: `go test ./pkg/server -run 'TestPathCredentials' -v`

Expected: all three `PASS`.

- [ ] **Step 5: Run the full package tests and vet**

Run: `go vet ./pkg/server/... && go test ./pkg/server/... ./pkg/session/...`

Expected: `ok` for each package, no vet output. If an unrelated test fails, run it on a clean `origin/master` (`git stash; go test …; git stash pop`) to confirm it was already failing before reporting.

- [ ] **Step 6: Commit**

```bash
git add pkg/server/server.go pkg/server/auth_path_credentials_test.go
git diff --cached --stat
git commit -m "Track client IP as viewer identity with custom credentials

authWithPathCredentials registered sessions under the shared local login,
collapsing every device into one session and one history identity. With
LDAP off, use the client IP instead, matching the provider-credentials
routes, so per-device sessions, history and IP aliases work.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git log --oneline | head -2
```

Expected: `--stat` lists exactly the two files; log shows the new commit on top of `Spec: client IP as viewer identity…`.

- [ ] **Step 7: Note for the PR description (no code)**

Mention in the PR body: the custom login name is no longer recorded in sessions/history/`/status`; existing `stream_history` rows keep the old login name in `username` alongside new IP-keyed rows; devices behind one NAT share an identity (same as the provider-credentials path today).
