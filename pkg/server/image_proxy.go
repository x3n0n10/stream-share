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
	"fmt"
	"net/url"
	"strings"
)

// proxyImageURL rewrites an absolute image/asset URL to point at this
// proxy's generic asset route instead of the upstream provider, so
// clients never resolve icons/covers/manifests directly against the
// provider. Empty, malformed, or relative values are returned unchanged.
func (c *Config) proxyImageURL(raw string) string {
	if _, err := url.ParseRequestURI(raw); err != nil {
		return raw
	}
	protocol := "http"
	if c.HTTPS {
		protocol = "https"
	}
	customEnd := strings.Trim(c.CustomEndpoint, "/")
	if customEnd != "" {
		customEnd = "/" + customEnd
	}
	return fmt.Sprintf("%s://%s:%d%s/img?url=%s",
		protocol, c.HostConfig.Hostname, c.AdvertisedPort, customEnd,
		url.QueryEscape(raw))
}

// rewriteImageFields walks a JSON-decoded player_api response (nested
// map[string]interface{} / []interface{}) and rewrites every known
// upstream-image field through proxyImageURL, in place, at any nesting
// depth. direct_source is deleted rather than rewritten: a player that
// uses it bypasses c.sessionManager's multiplexing entirely, and hiding
// the URL wouldn't restore that, so it's dropped to force a fall back to
// the session-managed stream_id play URL instead.
func (c *Config) rewriteImageFields(v interface{}) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		for key, fieldVal := range val {
			switch key {
			case "stream_icon", "cover", "movie_image":
				if s, ok := fieldVal.(string); ok {
					val[key] = c.proxyImageURL(s)
				}
			case "backdrop_path":
				if arr, ok := fieldVal.([]interface{}); ok {
					for i, item := range arr {
						if s, ok := item.(string); ok {
							arr[i] = c.proxyImageURL(s)
						}
					}
				}
			case "direct_source":
				delete(val, key)
			default:
				val[key] = c.rewriteImageFields(fieldVal)
			}
		}
		return val
	case []interface{}:
		for i, item := range val {
			val[i] = c.rewriteImageFields(item)
		}
		return val
	default:
		return v
	}
}
