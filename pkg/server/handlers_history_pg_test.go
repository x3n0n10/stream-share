/*
 * stream-share is a project to efficiently share the use of an IPTV service.
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

// Requires a live Postgres reachable via SS_TEST_DSN (a URL); skipped otherwise.

package server

import (
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/database"
)

// A history row served by a fallback has "via" in both history endpoints; a
// row without one has no such key.
func TestHistoryEndpointsCarryVia(t *testing.T) {
	dsn := os.Getenv("SS_TEST_DSN")
	if dsn == "" {
		t.Skip("SS_TEST_DSN not set; skipping Postgres integration tests")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pw, _ := u.User.Password()
	t.Setenv("DB_HOST", u.Hostname())
	t.Setenv("DB_PORT", u.Port())
	t.Setenv("DB_NAME", u.Path[1:])
	t.Setenv("DB_USER", u.User.Username())
	if pw == "" {
		pw = "-" // NewDBManager builds a key=value string: an empty password breaks it
	}
	t.Setenv("DB_PASSWORD", pw)
	t.Setenv("DB_SSLMODE", u.Query().Get("sslmode"))
	db, err := database.NewDBManager("")
	if err != nil {
		t.Skipf("no db: %v", err)
	}
	if _, err := db.AddStreamHistory("viahist-a", "viahist-ch1", "live", "T", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := db.SetStreamHistoryVia("viahist-ch1", "Spare"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddStreamHistory("viahist-b", "viahist-ch2", "live", "T", "", ""); err != nil {
		t.Fatal(err)
	}

	gin.SetMode(gin.TestMode)
	c := &Config{ProxyConfig: &config.ProxyConfig{}, db: db}
	r := gin.New()
	r.GET("/history", c.getHistoryFeed)
	r.GET("/history/:username", c.getUserHistory)
	get := func(path string) string {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w.Body.String()
	}

	if body := get("/history/viahist-a"); !strings.Contains(body, `"via":"Spare"`) {
		t.Fatalf("user history lacks via: %s", body)
	}
	if body := get("/history/viahist-b"); strings.Contains(body, `"via"`) {
		t.Fatalf("user history row without via has the key: %s", body)
	}
	feed := get("/history?limit=500")
	for _, row := range strings.Split(feed, `{"username"`) {
		switch {
		case strings.HasPrefix(row, `":"viahist-a"`):
			if !strings.Contains(row, `"via":"Spare"`) {
				t.Fatalf("feed row a lacks via: %s", row)
			}
		case strings.HasPrefix(row, `":"viahist-b"`):
			if strings.Contains(strings.SplitN(row, "}", 2)[0], `"via"`) {
				t.Fatalf("feed row b has via: %s", row)
			}
		}
	}
	if !strings.Contains(feed, `"viahist-a"`) {
		t.Fatalf("feed lacks the row: %s", feed)
	}
}
