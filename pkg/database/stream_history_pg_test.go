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
