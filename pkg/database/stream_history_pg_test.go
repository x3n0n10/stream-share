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

// Requires a live Postgres reachable via the SS_TEST_DSN env var; skipped otherwise.
//
// Note this TRUNCATEs stream_history, so never point it at a real deployment.

package database

import (
	"testing"
)

func freshHistoryDB(t *testing.T) *DBManager {
	t.Helper()
	m := testDB(t)
	if _, err := m.db.Exec("TRUNCATE stream_history"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return m
}

func storedTitle(t *testing.T, m *DBManager, id int64) string {
	t.Helper()
	var title string
	if err := m.db.QueryRow("SELECT COALESCE(stream_title, '') FROM stream_history WHERE id=$1", id).Scan(&title); err != nil {
		t.Fatalf("read title: %v", err)
	}
	return title
}

// TestUpdateStreamHistoryTitleCorrectsRow is the regression test: a VOD view's
// history row is inserted by whichever request gets there first, sometimes
// with the raw stream id as a fallback title before the real title resolves.
// UpdateStreamHistoryTitle must be able to correct that row in place.
func TestUpdateStreamHistoryTitleCorrectsRow(t *testing.T) {
	m := freshHistoryDB(t)

	id, err := m.AddStreamHistory("alice", "327914", "movie", "327914", "1.2.3.4", "TestAgent")
	if err != nil {
		t.Fatal(err)
	}
	if got := storedTitle(t, m, id); got != "327914" {
		t.Fatalf("expected fallback title 327914, got %q", got)
	}

	if err := m.UpdateStreamHistoryTitle(id, "The Real Movie Title"); err != nil {
		t.Fatal(err)
	}
	if got := storedTitle(t, m, id); got != "The Real Movie Title" {
		t.Fatalf("expected corrected title, got %q", got)
	}
}
