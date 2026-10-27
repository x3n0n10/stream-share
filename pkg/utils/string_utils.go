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

package utils

import (
	"fmt"
	"strings"
	"time"
)

// MaskString masks sensitive parts of strings for logging.
func MaskString(s string) string {
	switch {
	case len(s) == 0:
		return "[empty]"
	case len(s) <= 8:
		return s[:1] + "******"
	default:
		return s[:4] + "..." + s[len(s)-4:]
	}
}

// MaskURL masks sensitive parts of URLs for logging: the user/pass path
// segments of stream URLs (http://host/path/user/pass/id) and the username and
// password query parameters of API URLs (get.php?username=...&password=...).
func MaskURL(urlStr string) string {
	query := ""
	if i := strings.IndexByte(urlStr, '?'); i >= 0 {
		urlStr, query = urlStr[:i], urlStr[i+1:]
	}
	parts := strings.Split(urlStr, "/")
	if len(parts) >= 7 {
		// For URLs like http://host/path/user/pass/id
		parts[5] = MaskString(parts[5]) // Password
		parts[4] = MaskString(parts[4]) // Username
	}
	out := strings.Join(parts, "/")
	if query == "" {
		return out
	}
	params := strings.Split(query, "&")
	for i, kv := range params {
		k, _, _ := strings.Cut(kv, "=")
		if k == "username" || k == "password" {
			params[i] = k + "=***"
		}
	}
	return out + "?" + strings.Join(params, "&")
}

// HumanDuration formats a duration into a short, human-friendly string (e.g., "2 minutes", "3 hours").
func HumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d seconds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d minutes", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d hours", int(d.Hours()))
	default:
		return fmt.Sprintf("%d days", int(d.Hours()/24))
	}
}

// HumanBytes formats a byte count into a short, human-friendly string (e.g., 1.2 GB)
func HumanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	pre := []string{"KB", "MB", "GB", "TB", "PB", "EB"}
	if exp >= len(pre) {
		exp = len(pre) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(b)/float64(div), pre[exp])
}
