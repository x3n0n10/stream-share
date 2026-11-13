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

package session

import (
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/lucasduport/stream-share/pkg/database"
	"github.com/lucasduport/stream-share/pkg/types"
)

// viaTestDB connects the way NewDBManager does, from the SS_TEST_DSN URL.
func viaTestDB(t *testing.T) *database.DBManager {
	t.Helper()
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
	return db
}

// historyVia waits for the user's newest history row to carry want.
func historyVia(t *testing.T, db *database.DBManager, user, want string) {
	t.Helper()
	var got string
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		rows, err := db.GetUserHistory(user, time.Time{}, 1, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 {
			if got = rows[0].Via; got == want {
				return
			}
		}
	}
	t.Fatalf("%s: history via = %q, want %q", user, got, want)
}

func waitHistoryRow(t *testing.T, sm *SessionManager, user, streamID string) {
	t.Helper()
	key := streamUserKey(streamID, user)
	for end := time.Now().Add(3 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		sm.liveHistoryMu.Lock()
		id := sm.liveHistoryIDs[key]
		sm.liveHistoryMu.Unlock()
		if id > 0 {
			return
		}
	}
	t.Fatal("history row was not inserted")
}

// Both orders of the race between the history insert and the pump naming the
// fallback end with the row carrying it.
func TestHistoryViaBothOrders(t *testing.T) {
	db := viaTestDB(t)

	t.Run("insert first, pump second", func(t *testing.T) {
		sm := newTestManager()
		defer sm.Stop()
		sm.db = db
		sm.recordLiveHistory("via-a", "via-ch-a", "live", "T", "", "")
		waitHistoryRow(t, sm, "via-a", "via-ch-a")
		historyVia(t, db, "via-a", "")
		sm.recordHistoryVia("via-ch-a", "Spare") // what the pump calls
		historyVia(t, db, "via-a", "Spare")
	})

	t.Run("via set before the insert", func(t *testing.T) {
		sm := newTestManager()
		defer sm.Stop()
		sm.db = db
		ss := &types.StreamSession{StreamID: "via-ch-b", StreamType: "live", Viewers: map[string]time.Time{}}
		ss.SetVia("Spare")
		sm.streamLock.Lock()
		sm.streamSessions["via-ch-b"] = ss
		sm.streamLock.Unlock()
		sm.recordLiveHistory("via-b", "via-ch-b", "live", "T", "", "")
		historyVia(t, db, "via-b", "Spare")
	})

	t.Run("real pump", func(t *testing.T) {
		own := newFakeUpstream(t, 'O', 456)
		spare := newFakeUpstream(t, 'F', 0)
		sm, _ := failoverRig(t, nil, target(t, "Spare", spare.URL))
		sm.db = db
		buffer, _ := watch(t, sm, "via-c", "via-ch-c", "live", own.URL)
		if uerr := waitReady(t, buffer); uerr != nil {
			t.Fatalf("StartError = %+v", uerr)
		}
		historyVia(t, db, "via-c", "Spare")
	})
}
