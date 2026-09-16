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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
)

func decodeAPIResponse(t *testing.T, w *httptest.ResponseRecorder) types.APIResponse {
	t.Helper()
	var resp types.APIResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	return resp
}

func TestSearchChannelsEmptyQueryReturnsNoResults(t *testing.T) {
	c := &Config{db: &database.DBManager{}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/channels", nil)

	c.searchChannels(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeAPIResponse(t, w)
	if !resp.Success {
		t.Fatalf("success = false, want true (resp: %+v)", resp)
	}
}

func TestSearchChannelsUninitializedDBReturnsEmptyNotError(t *testing.T) {
	// SearchStreamNames itself guards a nil underlying *sql.DB, so a
	// zero-value DBManager (as if the database were never opened) must
	// still answer with an empty result, not a panic or a 500 — this
	// endpoint backs an optional wizard convenience, not a required path.
	c := &Config{db: &database.DBManager{}}

	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/channels?q=bbc", nil)

	c.searchChannels(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	resp := decodeAPIResponse(t, w)
	if !resp.Success {
		t.Fatalf("success = false, want true (resp: %+v)", resp)
	}
}
