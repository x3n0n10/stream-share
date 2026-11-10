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
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// quietRequestLogger logs failed HTTP requests (status >= 400) at a severity
// matching their status, and successful ones only in debug mode, keeping the
// success flood — health checks, internal API polling — out of the normal log.
// Unlike gin.Logger it redacts credentials from the logged URI.
func (c *Config) quietRequestLogger() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		start := time.Now()
		ctx.Next()

		status := ctx.Writer.Status()
		if status < 400 && !utils.IsDebugLogEnabled() {
			return
		}
		msg := fmt.Sprintf("[HTTP] %d | %v | %s | %s %q",
			status, time.Since(start), ctx.ClientIP(), ctx.Request.Method, c.redactRequestURI(ctx.Request.URL, ctx.FullPath()))
		if status >= 500 {
			utils.ErrorLog("%s", msg)
		} else if status < 400 {
			utils.DebugLog("%s", msg)
		} else {
			utils.WarnLog("%s", msg)
		}
	}
}

// redactRequestURI renders a request path and query for logging with every
// credential masked: the username/password query parameters, path segments the
// matched route declares as :password (stream routes carry a viewer's own
// credentials as /live/<user>/<pass>/<id>, LDAP ones included), and any segment
// equal to the proxy's or the provider's username or password. route is the
// matched route pattern (ctx.FullPath()), empty when nothing matched.
func (c *Config) redactRequestURI(u *url.URL, route string) string {
	secrets := map[string]bool{}
	for _, s := range []string{c.User.String(), c.Password.String(), c.XtreamUser.String(), c.XtreamPassword.String()} {
		if s != "" {
			secrets[s] = true
		}
	}
	segs := strings.Split(u.Path, "/")
	routeSegs := strings.Split(route, "/")
	for i, seg := range segs {
		// :token segments are credentials too (share and download links).
		if secrets[seg] || (i < len(routeSegs) && (routeSegs[i] == ":password" || routeSegs[i] == ":token")) {
			segs[i] = "***"
		}
	}
	out := strings.Join(segs, "/")
	if u.RawQuery == "" {
		return out
	}
	q := u.Query()
	for _, k := range []string{"username", "password"} {
		if q.Has(k) {
			q.Set(k, "***")
		}
	}
	return out + "?" + q.Encode()
}
