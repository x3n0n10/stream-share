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
	"net/url"
	"strings"
)

// isFetchableURL reports whether s, trimmed of surrounding blanks, is as a
// whole an absolute http(s) URL (so prose that merely contains a link is left
// alone). A space inside the path still makes a link a player would fetch.
func isFetchableURL(s string) bool {
	if !strings.Contains(s, "://") {
		return false // cheap exit for the common non-URL value; a host needs "//"
	}
	u, err := url.Parse(strings.TrimSpace(s))
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// clientSafeValue returns a copy of a JSON-decoded value in which every URL
// string points at this server (base + signed /img link) and direct_source
// is dropped: a player using it would bypass the session manager, and any
// other URL would expose the provider or a third party. Walking every
// string, not a field list, covers fields nobody listed. The input is never
// mutated (cached catalogue data is shared between requests).
func (c *Config) clientSafeValue(base string, v interface{}) interface{} {
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	own := base + customEnd + "/img?url="
	var walk func(v interface{}) interface{}
	walk = func(v interface{}) interface{} {
		switch val := v.(type) {
		case map[string]interface{}:
			out := make(map[string]interface{}, len(val))
			for k, item := range val {
				if k != "direct_source" {
					out[k] = walk(item)
				}
			}
			return out
		case []interface{}:
			out := make([]interface{}, len(val))
			for i, item := range val {
				out[i] = walk(item)
			}
			return out
		case string:
			if !isFetchableURL(val) {
				return val
			}
			raw := strings.TrimSpace(val)
			if strings.HasPrefix(raw, own) {
				return val // already one of our own /img links
			}
			if local, creds, _ := c.localizeProviderURL(base, raw); creds {
				return local // never put the provider's credentials into an /img link
			}
			return base + c.proxyImagePath(raw)
		default:
			return v
		}
	}
	return walk(v)
}

// localizeProviderURL maps an http(s) URL on the Xtream provider's host (either
// scheme) onto this server: base + custom endpoint + everything after the host,
// verbatim so {start}/${start} placeholders survive, with the provider's
// credentials (path segments or username/password query values) swapped for
// the local ones, so the request lands on our own routes and session manager.
// ok is false for any other host; creds reports whether credentials were swapped.
func (c *Config) localizeProviderURL(base, s string) (local string, creds, ok bool) {
	prov, err := url.Parse(c.XtreamBaseURL)
	if err != nil || prov.Host == "" {
		return "", false, false
	}
	i := strings.Index(s, "://")
	if i < 0 {
		return "", false, false
	}
	scheme, rest := strings.ToLower(s[:i]), s[i+3:]
	host := rest
	if j := strings.IndexAny(rest, "/?#"); j >= 0 {
		host, rest = rest[:j], rest[j:]
	} else {
		rest = ""
	}
	if (scheme != "http" && scheme != "https") || !strings.EqualFold(host, prov.Host) {
		return "", false, false
	}
	xu, xp := c.XtreamUser.String(), c.XtreamPassword.String()
	lu, lp := c.User.String(), c.Password.String()
	path, query, hasQuery := strings.Cut(rest, "?")
	newPath := path
	// As rewriteM3UURLLine: each credential as its own path segment.
	for _, cred := range [][2]string{{xu, lu}, {xp, lp}} {
		if cred[0] == "" {
			continue
		}
		for _, from := range []string{cred[0], url.PathEscape(cred[0])} {
			newPath = strings.ReplaceAll(newPath, "/"+from+"/", "/"+url.PathEscape(cred[1])+"/")
		}
	}
	creds = newPath != path
	if hasQuery {
		// Edited pair by pair, not re-encoded, so placeholders stay as they are.
		pairs := strings.Split(query, "&")
		for k, pair := range pairs {
			for _, cred := range [][3]string{{"username", xu, lu}, {"password", xp, lp}} {
				if cred[1] != "" && (pair == cred[0]+"="+cred[1] || pair == cred[0]+"="+url.QueryEscape(cred[1])) {
					pairs[k] = cred[0] + "=" + url.QueryEscape(cred[2])
					creds = true
				}
			}
		}
		newPath += "?" + strings.Join(pairs, "&")
	}
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	return base + customEnd + newPath, creds, true
}
