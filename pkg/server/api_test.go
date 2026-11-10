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
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

// TestInternalAPIPanicHidesDetails drives a real internal API handler into a
// panic (GET /instance dereferences the nil embedded ProxyConfig) and checks
// the caller sees only a generic 500 while the operator still gets the detail
// in the log.
func TestInternalAPIPanicHidesDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logBuf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logBuf)
	t.Cleanup(func() { log.SetOutput(prev) })

	router := gin.New()
	c := &Config{}
	c.setupInternalAPI(router)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/instance", nil)
	req.Header.Set("X-API-Key", GetAPIKey())
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"Internal server error"`) {
		t.Errorf("body = %q, want generic error message", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "runtime error") {
		t.Errorf("body leaks panic text: %q", w.Body.String())
	}
	if !strings.Contains(logBuf.String(), "runtime error") {
		t.Errorf("log lacks panic detail: %q", logBuf.String())
	}
}
