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

// Plain unit tests for stream_names.go that need no live Postgres (unlike
// stream_names_pg_test.go's SS_TEST_DSN-gated integration tests), so they
// run in every default `go test ./...` invocation.

package database

import "testing"

// TestSearchStreamNamesNilDBReturnsEmptySlice guards
// "empty result is [], never null": SearchStreamNames builds results with
// make([]types.ChannelMatch, 0), not var results []types.ChannelMatch. Both
// look "empty" under len(), but only the former marshals to JSON `[]`
// instead of `null` — which matters to a caller decoding the wire response.
// A zero-value DBManager exercises the nil-db guard without needing Postgres.
func TestSearchStreamNamesNilDBReturnsEmptySlice(t *testing.T) {
	m := &DBManager{}
	results, err := m.SearchStreamNames("bbc", 25)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results == nil {
		t.Fatal("expected non-nil slice, got nil (would marshal to JSON null)")
	}
	if len(results) != 0 {
		t.Fatalf("expected empty slice, got %d results", len(results))
	}
}

// TestEscapeLikePattern covers finding 3: '%' and '_' in a user's search
// query must not act as LIKE/ILIKE wildcards, and the escape character
// itself must be escaped first so a literal backslash in a query survives.
func TestEscapeLikePattern(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"bbc", "bbc"},
		{"%", `\%`},
		{"_", `\_`},
		{"100%_free", `100\%\_free`},
		{`\`, `\\`},
		{`\%`, `\\\%`},
	}
	for _, c := range cases {
		if got := escapeLikePattern(c.in); got != c.want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
