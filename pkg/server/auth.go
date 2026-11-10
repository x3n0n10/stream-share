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
	"crypto/subtle"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-ldap/ldap/v3"
	"github.com/google/uuid"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

var internalAPIKey string

func init() {
	// Generate a random API key at startup or use from environment
	envKey := os.Getenv("INTERNAL_API_KEY")
	if envKey != "" {
		internalAPIKey = envKey
		utils.InfoLog("Using API key from environment")
	} else {
		internalAPIKey = uuid.New().String()
		utils.InfoLog("Generated new internal API key (set INTERNAL_API_KEY env var to pin it)")
	}
}

func GetAPIKey() string {
	return internalAPIKey
}

// SetAPIKey pins the internal API key to the provided value (e.g. from
// configuration). An empty value leaves the existing (generated or env) key.
func SetAPIKey(key string) {
	if key != "" {
		internalAPIKey = key
		utils.InfoLog("Using internal API key from configuration")
	}
}

// apiKeyAuth middleware validates the internal API key
func (c *Config) apiKeyAuth() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		key := ctx.GetHeader("X-API-Key")
		utils.DebugLog("API Key auth check - received key: %s...", utils.MaskString(key))

		if subtle.ConstantTimeCompare([]byte(key), []byte(internalAPIKey)) != 1 {
			utils.DebugLog("API authentication failed - invalid key: %s", utils.MaskString(key))
			ctx.AbortWithStatusJSON(401, types.APIResponse{
				Success: false,
				Error:   "Invalid API key",
			})
			return
		}
		utils.DebugLog("API authentication successful for endpoint: %s", ctx.Request.URL.Path)
		ctx.Next()
	}
}

// authRequest represents credentials supplied via form/query params
// for endpoints using GET/POST with standard query binding.
type authRequest struct {
	Username string `form:"username" binding:"required"`
	Password string `form:"password" binding:"required"`
}

// recentlyAuthenticated tracks client IPs that have successfully
// authenticated recently, so /img (whose rewritten links never carry
// credentials -- players fetch them directly with no way to attach a
// username/password) can require some prior proof of access instead of
// being fully open to any request.
var (
	recentlyAuthenticated   = map[string]time.Time{}
	recentlyAuthenticatedMu sync.Mutex
)

// recentAuthTTL is how long a client IP stays trusted after its last
// successful authentication.
const recentAuthTTL = 24 * time.Hour

// recordRecentAuth marks ctx's client IP as recently authenticated. Called
// from authenticate/appAuthenticate on a successful check, and chained
// directly onto the Xtream direct-stream and M3U-track routes in
// routes.go, which have no c.authenticate of their own -- reaching those
// handlers at all already proves gin matched the request's path against
// the literal configured credentials.
func recordRecentAuth(ctx *gin.Context) {
	recentlyAuthenticatedMu.Lock()
	recentlyAuthenticated[ctx.ClientIP()] = time.Now()
	recentlyAuthenticatedMu.Unlock()
}

// sweepRecentAuth drops entries past recentAuthTTL so IPs that never come
// back do not accumulate for the life of the process.
func sweepRecentAuth(now time.Time) {
	recentlyAuthenticatedMu.Lock()
	defer recentlyAuthenticatedMu.Unlock()
	for ip, last := range recentlyAuthenticated {
		if now.Sub(last) > recentAuthTTL {
			delete(recentlyAuthenticated, ip)
		}
	}
}

// requireRecentAuth guards routes that can never carry credentials
// themselves (the stream routes record the trust; /play and /hls are
// guarded with it), by requiring the calling IP to have
// authenticated successfully elsewhere within recentAuthTTL. This is
// IP-based, not per-viewer: a client sharing a NAT/office IP with someone
// who just authenticated gets the same trust window. Weaker than real
// credentials, but far narrower than accepting every request.
func requireRecentAuth(ctx *gin.Context) {
	ip := ctx.ClientIP()

	recentlyAuthenticatedMu.Lock()
	last, ok := recentlyAuthenticated[ip]
	if ok && time.Since(last) > recentAuthTTL {
		delete(recentlyAuthenticated, ip)
		ok = false
	}
	recentlyAuthenticatedMu.Unlock()

	if !ok {
		ctx.AbortWithStatus(http.StatusForbidden)
	}
}

// authenticate validates form/query credentials using LDAP (if enabled) or
// local credentials. Used for GET/POST endpoints.
func (c *Config) authenticate(ctx *gin.Context) {
	utils.DebugLog("-> Incoming URL: %s", c.redactRequestURI(ctx.Request.URL, ctx.FullPath()))
	var authReq authRequest
	if err := ctx.Bind(&authReq); err != nil {
		utils.DebugLog("Bind error: %v", err)
		_ = ctx.AbortWithError(http.StatusBadRequest, err)
		return
	}

	if !c.credentialsValid(authReq.Username, authReq.Password) {
		utils.DebugLog("Authentication failed for user: %s", authReq.Username)
		ctx.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	recordRecentAuth(ctx)
}

// credentialsValid is the single login rule: LDAP when enabled, else the local
// user/password, compared in constant time (both always evaluated).
func (c *Config) credentialsValid(user, pass string) bool {
	if c.LDAPEnabled {
		return c.ldapAuthenticateCached(user, pass)
	}
	userMatch := subtle.ConstantTimeCompare([]byte(c.User.String()), []byte(user))
	passMatch := subtle.ConstantTimeCompare([]byte(c.Password.String()), []byte(pass))
	// An empty configured user never matches, so empty credentials cannot log in.
	configured := 0
	if c.User.String() != "" {
		configured = 1
	}
	return userMatch&passMatch&configured == 1
}

// appAuthenticate validates credentials for application/x-www-form-urlencoded
// bodies (player_api POST). It replays the body to allow downstream reading.
func (c *Config) appAuthenticate(ctx *gin.Context) {
	utils.DebugLog("-> Incoming URL: %s", c.redactRequestURI(ctx.Request.URL, ctx.FullPath()))

	contents, err := io.ReadAll(ctx.Request.Body)
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}

	q, err := url.ParseQuery(string(contents))
	if err != nil {
		ctx.AbortWithError(http.StatusInternalServerError, err) // nolint: errcheck
		return
	}
	if len(q["username"]) == 0 || len(q["password"]) == 0 {
		ctx.AbortWithError(http.StatusBadRequest, fmt.Errorf("bad body url query parameters")) // nolint: errcheck
		return
	}
	utils.DebugLog("App auth from %s", ctx.ClientIP())

	if !c.credentialsValid(q["username"][0], q["password"][0]) {
		utils.DebugLog("App authentication failed for user: %s", q["username"][0])
		ctx.AbortWithStatus(http.StatusUnauthorized)
		return
	}

	recordRecentAuth(ctx)
	ctx.Request.Body = io.NopCloser(bytes.NewReader(contents))
}

// ldapAuthenticate binds with an optional service account, finds the user DN,
// optionally validates group membership, then attempts a user bind.
func ldapAuthenticate(server, baseDN, bindDN, bindPassword, userAttr, groupAttr, requiredGroup, username, password string) bool {
	utils.DebugLog("LDAP DialURL: %s", server)
	l, err := ldap.DialURL(server)
	if err != nil {
		utils.DebugLog("LDAP DialURL error: %v", err)
		return false
	}
	defer l.Close()

	// Bind with service account
	if bindDN != "" && bindPassword != "" {
		utils.DebugLog("LDAP service bind attempt: DN=%s", bindDN)
		if err := l.Bind(bindDN, bindPassword); err != nil {
			utils.DebugLog("LDAP service bind error: %v", err)
			return false
		}
		utils.DebugLog("LDAP service bind succeeded")
	}

	// Search for user DN
	filter := fmt.Sprintf("(%s=%s)", userAttr, ldap.EscapeFilter(username))
	utils.DebugLog("LDAP search: baseDN=%s, filter=%s", baseDN, filter)
	searchRequest := ldap.NewSearchRequest(
		baseDN,
		ldap.ScopeWholeSubtree, ldap.NeverDerefAliases, 1, 0, false,
		filter,
		[]string{"dn", groupAttr}, // Include group attribute
		nil,
	)
	sr, err := l.Search(searchRequest)
	if err != nil {
		utils.DebugLog("LDAP search error: %v", err)
		return false
	}
	if len(sr.Entries) == 0 {
		utils.DebugLog("LDAP search: no entries found for user: %s", username)
		return false
	}
	userDN := sr.Entries[0].DN
	utils.DebugLog("LDAP user DN found: %s", userDN)

	// Check group membership if requiredGroup is specified
	if requiredGroup != "" && groupAttr != "" {
		hasGroup := false
		for _, entry := range sr.Entries {
			for _, groupValue := range entry.GetAttributeValues(groupAttr) {
				utils.DebugLog("LDAP user group: %s", groupValue)
				if ldapGroupMatches(groupValue, requiredGroup) {
					hasGroup = true
					break
				}
			}
		}
		if !hasGroup {
			utils.DebugLog("LDAP user %s is not a member of required group: %s", username, requiredGroup)
			return false
		}
		utils.DebugLog("LDAP user %s is a member of required group: %s", username, requiredGroup)
	}

	// Try to bind as user
	utils.DebugLog("LDAP user bind attempt: DN=%s", userDN)
	if err := l.Bind(userDN, password); err != nil {
		utils.DebugLog("LDAP user bind error: %v", err)
		return false
	}
	utils.DebugLog("LDAP user bind succeeded for user: %s", username)
	return true
}

// ldapGroupMatches reports whether groupValue (which may be a DN like
// "cn=iptv,ou=groups,dc=example,dc=com" or a plain name) matches requiredGroup.
// For DN values it extracts the CN and compares; for plain values it compares exactly
// (case-insensitive). This replaces a substring match that would accept "notiptv"
// when requiredGroup is "iptv".
func ldapGroupMatches(groupValue, requiredGroup string) bool {
	lower := strings.ToLower(groupValue)
	prefix := "cn=" + strings.ToLower(requiredGroup)
	// DN form: must start with "cn=<group>," or equal "cn=<group>"
	if strings.HasPrefix(lower, "cn=") {
		return strings.HasPrefix(lower, prefix+",") || lower == prefix
	}
	// Plain name: exact case-insensitive equality
	return lower == strings.ToLower(requiredGroup)
}

// Authentication middleware that checks credentials from URL path parameters
// and manages user sessions for multiplexing
// authWithPathCredentials authenticates :username/:password path params against
// either LDAP (if enabled) or local credentials, and registers the user session.
func (c *Config) authWithPathCredentials() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		username := ctx.Param("username")
		password := ctx.Param("password")
		ip := ctx.ClientIP()
		userAgent := ctx.Request.UserAgent()

		utils.DebugLog("Path credentials auth check: username=%s, IP=%s", username, ip)

		if !c.credentialsValid(username, password) {
			utils.DebugLog("Authentication failed for user in path: %s", username)
			ctx.AbortWithStatus(http.StatusUnauthorized)
			return
		}

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
			// Per request: RegisterUser itself logs a genuinely new session.
			utils.DebugLog("authWithPathCredentials: session registered for user=%s ip=%s", username, ip)
		}
		ctx.Set("username", identity)

		ctx.Next()
	}
}

// resolveRequestUsername derives the viewer identity for session tracking. It
// prefers the authenticated username set by middleware, then path/query params,
// and finally falls back to the client IP. The fallback matters for the direct
// Xtream-credentials routes (e.g. /movie/<user>/<pass>/:id), which have no auth
// middleware and therefore no username in context — without it, VOD views would
// never be registered and /status would miss them.
func (c *Config) resolveRequestUsername(ctx *gin.Context) string {
	username := ctx.GetString("username")
	if username == "" {
		username = ctx.Param("username")
	}
	if username == "" {
		username = ctx.Query("username")
	}
	if username == "" {
		username = ctx.ClientIP()
	}
	return username
}
