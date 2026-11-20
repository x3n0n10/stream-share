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
	"crypto/subtle"
	"net/http"
	"unicode"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/session"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const (
	failoverKeyHeader  = "X-Failover-Key"
	failoverFromHeader = "X-Failover-From"
)

// failoverCaller is the calling instance's display name from its header, kept
// to plain characters since it ends up in logs and session names.
func failoverCaller(raw string) string {
	out := make([]rune, 0, 64)
	for _, r := range raw {
		if len(out) == 64 {
			break
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '.' || r == '_' || r == '-' {
			out = append(out, r)
		}
	}
	if len(out) == 0 {
		return "instance"
	}
	return string(out)
}

// handleFailoverLive GET /api/failover/live?epg=&name= lends this instance's
// matching live channel to another instance (the key is the credential). The
// caller is a guest: it yields to the household and never gets an error slate
// (see session.FailoverIdentityPrefix).
func (c *Config) handleFailoverLive(ctx *gin.Context) {
	// An empty key would match an absent header: an instance without a key never lends.
	if c.FailoverKey == "" {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	if subtle.ConstantTimeCompare([]byte(ctx.GetHeader(failoverKeyHeader)), []byte(c.FailoverKey)) != 1 {
		ctx.AbortWithStatus(http.StatusUnauthorized)
		return
	}
	from := failoverCaller(ctx.GetHeader(failoverFromHeader))
	id, ok := c.matchFailoverChannel(ctx.Query("epg"), ctx.Query("name"))
	if !ok {
		ctx.AbortWithStatus(http.StatusNotFound)
		return
	}
	targetURL, _ := c.shareUpstream(id)
	identity := session.FailoverIdentityPrefix + from + "/" + id
	if c.sessionManager != nil {
		c.sessionManager.RegisterUser(identity, ctx.ClientIP(), ctx.Request.UserAgent())
	}
	ctx.Set("username", identity)
	utils.InfoLog("Failover: serving %s for %s", c.streamLabel(id), from)
	c.multiplexedStream(ctx, targetURL)
}
