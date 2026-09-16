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

package types

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChannelMatchWireContract guards the cross-repo wire contract: a
// separate Suite repo binds to ChannelMatch's exact JSON shape
// (StreamID/Name/Category, PascalCase, no json tags) with no compile-time
// link between the two repos. If someone later adds json tags (e.g.
// `json:"stream_id"`) this test must fail.
func TestChannelMatchWireContract(t *testing.T) {
	resp := APIResponse{
		Success: true,
		Data: []ChannelMatch{
			{StreamID: "1", Name: "BBC One", Category: "UK"},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	raw := string(b)

	for _, want := range []string{`"StreamID":"1"`, `"Name":"BBC One"`, `"Category":"UK"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("expected JSON to contain %q, got: %s", want, raw)
		}
	}
}
