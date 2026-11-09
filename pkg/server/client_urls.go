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
			if raw := strings.TrimSpace(val); !strings.HasPrefix(raw, own) {
				return base + c.proxyImagePath(raw)
			}
			return val // already one of our own /img links
		default:
			return v
		}
	}
	return walk(v)
}
