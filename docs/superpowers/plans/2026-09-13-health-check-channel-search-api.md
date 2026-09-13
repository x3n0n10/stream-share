# Health check channel search API (stream-share side) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give an operator a way to find a live channel's `stream_id` by name (with its provider category shown, since names collide across categories) instead of needing to already know the raw id — backed entirely by data this instance already collects at startup, with no new provider calls per search.

**Architecture:** Two pipelines. The **write** side extends the existing channel-name harvest (`warmChannelNameIndex` / `harvestChannelNames`) to also resolve and persist each channel's category, via one new startup call to `get_live_categories` (not the per-category loop `xtreamGenerateM3u` uses for M3U generation — that would multiply provider calls). The **read** side is a new `GET /api/internal/channels?q=` endpoint that only ever queries the already-persisted `stream_names` table.

**Tech Stack:** Go, `gin`, `database/sql` against Postgres (raw SQL, no ORM — matches the rest of `pkg/database`).

**Spec:** `docs/superpowers/specs/2026-09-13-health-check-channel-picker-design.md` (in the `stream-share-suite` repo — this plan implements this repo's half of it).

**Companion plan:** `docs/superpowers/plans/2026-09-13-health-check-channel-picker.md` in `stream-share-suite` builds the Suite proxy route and wizard UI that call the endpoint this plan builds.

## Global Constraints

- Response envelope is `types.APIResponse` (`{success, data}`); `data` is `[]types.ChannelMatch` — `StreamID`, `Name`, `Category`, no JSON tags (PascalCase on the wire), matching `types.VODResult`'s existing convention.
- No per-search or per-category-item network calls to the provider — the search endpoint reads only `stream_names`; category resolution happens once at warm-up via one flat `get_live_categories` call, not per category.
- A missing/uninitialized database or an empty query must return an empty result, never an error — this is a best-effort picker, not a required path.

---

### Task 1: Category harvesting and persistence

**Files:**
- Modify: `pkg/database/schema.go` (migration, after line 121)
- Modify: `pkg/database/stream_names.go` (full rewrite of `UpsertStreamNames`/`UpsertStreamName`)
- Modify: `pkg/database/stream_names_pg_test.go` (update existing call sites, add category tests)
- Modify: `pkg/server/m3u_index.go` (`streamNamesHash`, `persistStreamNamesAsync`, `updateAPIChannelIndex`, new `categoryNameIndex`)
- Modify: `pkg/server/m3u_index_test.go` (update existing hash tests, add category case)
- Modify: `pkg/server/xtream_handlers_api.go` (`harvestChannelNames`, `warmChannelNameIndex`, new `resolveCategoryName`/`warmCategoryNameIndex`)
- Test: `pkg/server/xtream_handlers_api_test.go` (add `resolveCategoryName` test)

**Interfaces:**
- Consumes: nothing outside this task.
- Produces: `stream_names.category` column; `lookupCategoryName(categoryID string) (string, bool)` in `pkg/server/m3u_index.go`, used by nothing else yet (Task 2's `SearchStreamNames` reads the column directly, not this function — this function is only for resolving a category *while harvesting*). `UpsertStreamNames(names, epgIDs, categories map[string]string, source string) error` — Task 2 does not call this directly, but must know the DB write path now persists a `category` column so its read-side query makes sense.

- [ ] **Step 1: Update existing tests for the new signatures, and add new ones — expect them to fail to compile**

In `pkg/server/m3u_index_test.go`, update every `streamNamesHash(...)` call to pass a third `categories` argument, and add a "changed category" case:

```go
func TestStreamNamesHashStableAcrossIteration(t *testing.T) {
	names := map[string]string{}
	epg := map[string]string{}
	for i := 0; i < 200; i++ {
		id := string(rune('a'+i%26)) + string(rune('0'+i%10))
		names[id] = "Channel " + id
		epg[id] = id + ".tv"
	}

	want := streamNamesHash(names, epg, nil)
	for i := 0; i < 20; i++ {
		if got := streamNamesHash(names, epg, nil); got != want {
			t.Fatalf("hash changed between calls on identical input: %s != %s", got, want)
		}
	}
}

func TestStreamNamesHashDetectsChanges(t *testing.T) {
	base := map[string]string{"1": "One", "2": "Two"}
	baseEPG := map[string]string{"1": "one.tv"}
	baseCategories := map[string]string{"1": "News"}
	want := streamNamesHash(base, baseEPG, baseCategories)

	cases := []struct {
		name       string
		names      map[string]string
		epg        map[string]string
		categories map[string]string
	}{
		{"renamed channel", map[string]string{"1": "Uno", "2": "Two"}, baseEPG, baseCategories},
		{"added channel", map[string]string{"1": "One", "2": "Two", "3": "Three"}, baseEPG, baseCategories},
		{"removed channel", map[string]string{"1": "One"}, baseEPG, baseCategories},
		{"changed epg id", base, map[string]string{"1": "uno.tv"}, baseCategories},
		{"removed epg id", base, map[string]string{}, baseCategories},
		{"changed category", base, baseEPG, map[string]string{"1": "Sports"}},
		{"removed category", base, baseEPG, map[string]string{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := streamNamesHash(tc.names, tc.epg, tc.categories); got == want {
				t.Fatal("hash did not change, so a real update would be skipped")
			}
		})
	}
}

func TestStreamNamesHashNoFieldBleed(t *testing.T) {
	a := streamNamesHash(map[string]string{"ab": "c"}, nil, nil)
	b := streamNamesHash(map[string]string{"a": "bc"}, nil, nil)
	if a == b {
		t.Fatal("adjacent fields collide; separators are not doing their job")
	}
}
```

(`TestEnsureChannelIndexAcceptsRelativeTrackLines` is untouched — it doesn't call `streamNamesHash`.)

In `pkg/database/stream_names_pg_test.go`, add a `nil` categories argument to every existing `UpsertStreamNames(...)` call (5 call sites: `TestUpsertStreamNamesBatchLarge`, `TestUpsertStreamNamesConflictUpdates` ×2, `TestUpsertStreamNamesExactBatchBoundary`, `TestLoadStreamNamesRoundTrip` ×2) — e.g. `m.UpsertStreamNames(names, epg, nil, "api")`. Then add:

```go
// TestUpsertStreamNamesPersistsCategory guards the column this whole feature
// depends on: a category with no way to be written back would silently
// break the search picker's category label.
func TestUpsertStreamNamesPersistsCategory(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(
		map[string]string{"1": "One"}, nil, map[string]string{"1": "News"}, "api",
	); err != nil {
		t.Fatal(err)
	}
	var category string
	if err := m.db.QueryRow("SELECT category FROM stream_names WHERE stream_id='1'").Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != "News" {
		t.Fatalf("category = %q, want %q", category, "News")
	}
}

// TestUpsertStreamNamesConflictUpdatesCategory is TestUpsertStreamNamesConflictUpdates'
// sibling for the new column: a re-run must replace the category, not leave
// a stale one from before a provider renamed it.
func TestUpsertStreamNamesConflictUpdatesCategory(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(
		map[string]string{"1": "One"}, nil, map[string]string{"1": "News"}, "api",
	); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertStreamNames(
		map[string]string{"1": "One"}, nil, map[string]string{"1": "Sports"}, "api",
	); err != nil {
		t.Fatal(err)
	}
	var category string
	if err := m.db.QueryRow("SELECT category FROM stream_names WHERE stream_id='1'").Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != "Sports" {
		t.Fatalf("category = %q, want %q (conflict update should replace it)", category, "Sports")
	}
}

// TestUpsertStreamNamesMissingCategoryStoresEmpty mirrors the existing "odd
// ids have no EPG id" spot-check in TestUpsertStreamNamesBatchLarge: a
// channel harvested with no resolved category must store empty, not NULL or
// an error.
func TestUpsertStreamNamesMissingCategoryStoresEmpty(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(map[string]string{"1": "One"}, nil, nil, "api"); err != nil {
		t.Fatal(err)
	}
	var category string
	if err := m.db.QueryRow("SELECT category FROM stream_names WHERE stream_id='1'").Scan(&category); err != nil {
		t.Fatal(err)
	}
	if category != "" {
		t.Fatalf("category = %q, want empty", category)
	}
}
```

In `pkg/server/xtream_handlers_api_test.go`, add:

```go
func TestResolveCategoryNameUsesWarmedIndex(t *testing.T) {
	categoryNameIndexMu.Lock()
	prev := categoryNameIndex
	categoryNameIndex = map[string]string{"7": "Sports"}
	categoryNameIndexMu.Unlock()
	t.Cleanup(func() {
		categoryNameIndexMu.Lock()
		categoryNameIndex = prev
		categoryNameIndexMu.Unlock()
	})

	cases := []struct {
		name string
		item map[string]interface{}
		want string
	}{
		{"known category", map[string]interface{}{"category_id": "7"}, "Sports"},
		{"unknown category", map[string]interface{}{"category_id": "999"}, ""},
		{"missing field", map[string]interface{}{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveCategoryName(tc.item); got != tc.want {
				t.Errorf("resolveCategoryName(%v) = %q, want %q", tc.item, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail to compile**

Run: `go build ./... && go test ./pkg/database/... ./pkg/server/... 2>&1 | head -40`
Expected: build failures — `streamNamesHash`, `UpsertStreamNames` called with the wrong number of arguments; `resolveCategoryName`, `categoryNameIndexMu`, `categoryNameIndex` undefined.

- [ ] **Step 3: Add the schema migration**

In `pkg/database/schema.go`, directly after the existing `epg_channel_id` migration (after line 121):

```go
	// Migration: add category column to existing stream_names tables — the
	// human-readable live-stream category (e.g. "Sports"), resolved at
	// harvest time from get_live_categories. Empty for rows harvested before
	// this migration or from a source that never had one (m3u, vod).
	if _, err := m.db.Exec(`ALTER TABLE stream_names ADD COLUMN IF NOT EXISTS category TEXT NOT NULL DEFAULT ''`); err != nil {
		return fmt.Errorf("failed to add category to stream_names: %w", err)
	}
```

Also update the `CREATE TABLE IF NOT EXISTS stream_names` block itself (lines 105-112) so a from-scratch database gets the column without relying on the migration line — add `category TEXT NOT NULL DEFAULT '',` after `epg_channel_id`:

```go
	if _, err := m.db.Exec(`
        CREATE TABLE IF NOT EXISTS stream_names (
            stream_id      TEXT NOT NULL,
            source         TEXT NOT NULL,
            name           TEXT NOT NULL,
            epg_channel_id TEXT NOT NULL DEFAULT '',
            category       TEXT NOT NULL DEFAULT '',
            updated_at     TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
            PRIMARY KEY (stream_id, source)
        )
    `); err != nil {
```

- [ ] **Step 4: Thread `categories` through `UpsertStreamNames`**

In `pkg/database/stream_names.go`, replace the `upsertStreamNamesSuffix` constant (lines 40-42):

```go
const upsertStreamNamesSuffix = `
    ON CONFLICT (stream_id, source) DO UPDATE
        SET name = EXCLUDED.name, epg_channel_id = EXCLUDED.epg_channel_id,
            category = EXCLUDED.category, updated_at = EXCLUDED.updated_at`
```

Replace `UpsertStreamName` (lines 44-51):

```go
// UpsertStreamName upserts a single stream name with an optional EPG channel ID.
func (m *DBManager) UpsertStreamName(streamID, source, name, epgChannelID string) error {
	epgIDs := map[string]string{}
	if epgChannelID != "" {
		epgIDs[streamID] = epgChannelID
	}
	return m.UpsertStreamNames(map[string]string{streamID: name}, epgIDs, nil, source)
}
```

Replace `UpsertStreamNames` (lines 53-114):

```go
// UpsertStreamNames batch-upserts id→name pairs (with optional EPG channel
// IDs and categories) for the given source.
//
// Rows are written in multi-row batches rather than one statement per name: a
// full channel list runs to thousands of entries, and a per-row loop meant
// thousands of sequential round trips, which is slow enough to stall whatever
// called it and to tie up a pooled connection for minutes.
func (m *DBManager) UpsertStreamNames(names map[string]string, epgIDs map[string]string, categories map[string]string, source string) error {
	if m == nil || m.db == nil || len(names) == 0 {
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), upsertStreamNamesTimeout)
	defer cancel()

	now := time.Now()
	started := now

	// Flatten to a stable slice so batching is straightforward.
	type row struct{ id, name, epgID, category string }
	rows := make([]row, 0, len(names))
	for id, name := range names {
		epgID := ""
		if epgIDs != nil {
			epgID = epgIDs[id]
		}
		category := ""
		if categories != nil {
			category = categories[id]
		}
		rows = append(rows, row{id: id, name: name, epgID: epgID, category: category})
	}

	for start := 0; start < len(rows); start += upsertBatchRows {
		end := start + upsertBatchRows
		if end > len(rows) {
			end = len(rows)
		}
		batch := rows[start:end]

		var sb strings.Builder
		sb.WriteString("INSERT INTO stream_names (stream_id, source, name, epg_channel_id, category, updated_at) VALUES ")
		args := make([]interface{}, 0, len(batch)*6)
		for i, r := range batch {
			if i > 0 {
				sb.WriteString(",")
			}
			n := i * 6
			fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d)", n+1, n+2, n+3, n+4, n+5, n+6)
			args = append(args, r.id, source, r.name, r.epgID, r.category, now)
		}
		sb.WriteString(upsertStreamNamesSuffix)

		if _, err := m.db.ExecContext(ctx, sb.String(), args...); err != nil {
			utils.ErrorLog("Database error upserting stream names (source=%s, batch %d-%d of %d): %v",
				source, start, end, len(rows), err)
			return err
		}
	}

	if elapsed := time.Since(started); elapsed > time.Second {
		utils.DebugLog("Database: upserted %d stream name(s) for source=%s in %s",
			len(rows), source, utils.HumanDuration(elapsed))
	}
	return nil
}
```

(`LoadStreamNames` is untouched — nothing needs the category through that path.)

- [ ] **Step 5: Thread `categories` through the hash and the in-memory index**

In `pkg/server/m3u_index.go`, add a new package var to the existing `var (...)` block (after `epgIndex`, line 52):

```go
	// categoryNameIndex maps a live-stream category_id to its human-readable
	// name, harvested once from get_live_categories at startup (see
	// warmCategoryNameIndex in xtream_handlers_api.go) — live-stream items
	// only carry the id, not the name. Deliberately not refreshed on every
	// get_live_streams harvest: categories change far less often than channel
	// names, and refetching on every player list request would multiply
	// provider calls for no benefit.
	categoryNameIndexMu sync.RWMutex
	categoryNameIndex   map[string]string
```

Replace `streamNamesHash` (lines 64-81):

```go
func streamNamesHash(names map[string]string, epgIDs map[string]string, categories map[string]string) string {
	ids := make([]string, 0, len(names))
	for id := range names {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	h := sha256.New()
	for _, id := range ids {
		h.Write([]byte(id))
		h.Write([]byte{0})
		h.Write([]byte(names[id]))
		h.Write([]byte{0})
		h.Write([]byte(epgIDs[id]))
		h.Write([]byte{1})
		h.Write([]byte(categories[id]))
		h.Write([]byte{2})
	}
	return hex.EncodeToString(h.Sum(nil))
}
```

Replace `persistStreamNamesAsync` (lines 99-124):

```go
func (c *Config) persistStreamNamesAsync(names map[string]string, epgIDs map[string]string, categories map[string]string, source string) {
	if c.db == nil || len(names) == 0 {
		return
	}

	hash := streamNamesHash(names, epgIDs, categories)
	if prev, ok := lastPersisted.Load(source); ok && prev.(string) == hash {
		utils.DebugLog("stream_names: %s index unchanged (%d entries); skipping write", source, len(names))
		return
	}

	if _, busy := persistInFlight.LoadOrStore(source, struct{}{}); busy {
		utils.DebugLog("stream_names: persist for source=%s already running; skipping this round", source)
		return
	}
	go func() {
		defer persistInFlight.Delete(source)
		if err := c.db.UpsertStreamNames(names, epgIDs, categories, source); err != nil {
			utils.WarnLog("stream_names: failed to persist %s channel index: %v", source, err)
			return
		}
		// Only remember the hash on success, so a failed write is retried next time.
		lastPersisted.Store(source, hash)
		utils.DebugLog("stream_names: persisted %d %s entries", len(names), source)
	}()
}
```

Replace `updateAPIChannelIndex` (lines 126-142):

```go
// updateAPIChannelIndex replaces the API-sourced name index with id→name
// pairs, optional EPG IDs, and optional categories.
func (c *Config) updateAPIChannelIndex(names map[string]string, epgIDs map[string]string, categories map[string]string) {
	if len(names) == 0 {
		return
	}
	apiChannelIndexMu.Lock()
	apiChannelIndex = names
	apiChannelIndexMu.Unlock()

	if len(epgIDs) > 0 {
		epgIndexMu.Lock()
		epgIndex = epgIDs // API is authoritative for EPG IDs
		epgIndexMu.Unlock()
	}

	c.persistStreamNamesAsync(names, epgIDs, categories, "api")
}
```

Find the `ensureChannelIndex` function's own `persistStreamNamesAsync` call (the "m3u" source one — search for `c.persistStreamNamesAsync(newIndex, newEPGIndex, "m3u")`) and add a `nil` categories argument: `c.persistStreamNamesAsync(newIndex, newEPGIndex, nil, "m3u")` — the m3u path has no category data available.

Add, directly after `lookupAPIChannelName` (after the existing function ending `return name, ok` / `}`):

```go
// updateCategoryNameIndex replaces the category id→name index, harvested
// once at startup (see warmCategoryNameIndex).
func updateCategoryNameIndex(names map[string]string) {
	if len(names) == 0 {
		return
	}
	categoryNameIndexMu.Lock()
	categoryNameIndex = names
	categoryNameIndexMu.Unlock()
}

// lookupCategoryName returns the human-readable name for a live-stream category_id.
func lookupCategoryName(categoryID string) (string, bool) {
	categoryNameIndexMu.RLock()
	defer categoryNameIndexMu.RUnlock()
	if categoryNameIndex == nil {
		return "", false
	}
	name, ok := categoryNameIndex[categoryID]
	return name, ok
}
```

- [ ] **Step 6: Harvest the category per channel, and warm the category index at startup**

In `pkg/server/xtream_handlers_api.go`, replace `harvestChannelNames` (lines 219-245):

```go
// harvestChannelNames extracts stream_id → name pairs, EPG channel IDs, and
// resolved categories from a get_live_streams response and refreshes the API
// channel name index used by /status and logs.
// It returns the number of channel names indexed.
func (c *Config) harvestChannelNames(resp interface{}) int {
	streams, ok := resp.([]interface{})
	if !ok {
		return 0
	}
	names := make(map[string]string, len(streams))
	epgIDs := make(map[string]string, len(streams))
	categories := make(map[string]string, len(streams))
	for _, item := range streams {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := normalizeStreamID(fmt.Sprintf("%v", m["stream_id"]))
		name := strings.TrimSpace(fmt.Sprintf("%v", m["name"]))
		if id != "" && name != "" {
			names[id] = name
		}
		if epgID, _ := m["epg_channel_id"].(string); strings.TrimSpace(epgID) != "" {
			epgIDs[id] = strings.TrimSpace(epgID)
		}
		if category := resolveCategoryName(m); category != "" {
			categories[id] = category
		}
	}
	c.updateAPIChannelIndex(names, epgIDs, categories)
	return len(names)
}

// resolveCategoryName looks up the human-readable category for a
// get_live_streams item's category_id, using the index warmed by
// warmCategoryNameIndex. Returns "" if the item has no usable category_id or
// it isn't in the index (map miss, or the warm-up hasn't run/failed yet).
func resolveCategoryName(m map[string]interface{}) string {
	id := fmt.Sprintf("%v", m["category_id"])
	if id == "" || id == "<nil>" {
		return ""
	}
	name, _ := lookupCategoryName(id)
	return name
}
```

Replace `warmChannelNameIndex` (lines 247-266ish, ending at the function's closing brace):

```go
// warmChannelNameIndex fetches get_live_streams once so channel names can be
// resolved even before a player requests the list through the proxy — e.g. right
// after a container restart, when a player (TiviMate in Xtream API mode) serves
// the channel list from its own cache and never re-fetches it.
func (c *Config) warmChannelNameIndex() {
	client, err := xtreamapi.New(c.XtreamUser.String(), c.XtreamPassword.String(), c.XtreamBaseURL, "")
	if err != nil {
		utils.WarnLog("Channel name warm-up: failed to create Xtream client: %v", err)
		return
	}

	c.warmCategoryNameIndex(client)

	resp, _, _, err := client.Action(c.ProxyConfig, "get_live_streams", nil)
	if err != nil {
		utils.WarnLog("Channel name warm-up: get_live_streams failed: %v", err)
		return
	}
	if n := c.harvestChannelNames(xtreamapi.ProcessResponse(resp)); n > 0 {
		utils.InfoLog("Channel name warm-up: indexed %d channel names from get_live_streams", n)
	} else {
		utils.WarnLog("Channel name warm-up: get_live_streams returned no usable channel names")
	}
}

// warmCategoryNameIndex fetches get_live_categories once so harvestChannelNames
// can resolve a stream's category_id to a name without a per-item or
// per-category network call — unlike xtreamGenerateM3u's per-category
// get_live_streams loop (pkg/server/xtream_generate.go), which exists to
// build a categorized M3U and would multiply provider calls needlessly here.
// Best-effort: a failure just means channels are indexed without a category
// label until the next successful warm-up.
func (c *Config) warmCategoryNameIndex(client *xtreamapi.Client) {
	resp, _, _, err := client.Action(c.ProxyConfig, "get_live_categories", nil)
	if err != nil {
		utils.WarnLog("Channel name warm-up: get_live_categories failed: %v", err)
		return
	}
	categories, ok := xtreamapi.ProcessResponse(resp).([]interface{})
	if !ok {
		utils.WarnLog("Channel name warm-up: unexpected get_live_categories format: %T", resp)
		return
	}
	names := make(map[string]string, len(categories))
	for _, item := range categories {
		m, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		id := fmt.Sprintf("%v", m["category_id"])
		name := strings.TrimSpace(fmt.Sprintf("%v", m["category_name"]))
		if id != "" && id != "<nil>" && name != "" {
			names[id] = name
		}
	}
	updateCategoryNameIndex(names)
	utils.InfoLog("Channel name warm-up: indexed %d live categories", len(names))
}
```

- [ ] **Step 7: Run the tests to verify they pass**

Run: `go build ./... && go test ./pkg/server/... -run 'TestStreamNamesHash|TestResolveCategoryName|TestEnsureChannelIndex' -v`
Expected: PASS, all cases including the new "changed category"/"removed category" and `resolveCategoryName` cases.

Run: `SS_TEST_DSN="postgres://user:pass@localhost/sstest?sslmode=disable" go test ./pkg/database/... -run TestUpsertStreamNames -v` (against a throwaway Postgres — see the file header comment in `stream_names_pg_test.go`; skips cleanly if `SS_TEST_DSN` is unset)
Expected: PASS, including the three new category tests.

- [ ] **Step 8: Run the full test suite to check for regressions**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere (Postgres-dependent tests skip without `SS_TEST_DSN`, as today).

- [ ] **Step 9: Commit**

```bash
git add pkg/database/schema.go pkg/database/stream_names.go pkg/database/stream_names_pg_test.go \
        pkg/server/m3u_index.go pkg/server/m3u_index_test.go \
        pkg/server/xtream_handlers_api.go pkg/server/xtream_handlers_api_test.go
git commit -m "$(cat <<'EOF'
Harvest and persist each live channel's category

get_live_streams items only carry category_id, not a name, so this adds
one more flat get_live_categories call at warm-up (cached in memory,
not the per-category get_live_streams loop xtreamGenerateM3u uses for
M3U generation) and resolves it per channel when harvesting names. The
resolved category now flows through streamNamesHash and into a new
stream_names.category column — groundwork for the health-check channel
search API, which needs to show which group a channel is in.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Channel search API

**Files:**
- Modify: `pkg/types/types.go` (new `ChannelMatch` struct, after `APIResponse`)
- Modify: `pkg/database/stream_names.go` (new `SearchStreamNames`)
- Modify: `pkg/database/stream_names_pg_test.go` (new tests)
- Create: `pkg/server/handlers_channels.go` (new `searchChannels` handler)
- Create: `pkg/server/handlers_channels_test.go`
- Modify: `pkg/server/api.go` (route registration, after line 82)

**Interfaces:**
- Consumes: `stream_names.category` column from Task 1 (queries it directly — does not call `lookupCategoryName` or any in-memory index, since a search must work across every source ever persisted, not just the live in-memory `api` snapshot).
- Produces: `GET /api/internal/channels?q=` → `types.APIResponse{Success, Data: []types.ChannelMatch}`. Nothing downstream in this repo consumes it — the Suite's `instanceClient.searchChannels` (companion plan) is the consumer.

- [ ] **Step 1: Write the failing tests**

In `pkg/types/types.go`, this task will add (in Step 3) a type the tests below need — write the tests first anyway; they simply won't compile until Step 3 lands, which is expected for this step.

Add to `pkg/database/stream_names_pg_test.go`:

```go
// TestSearchStreamNamesMatchesNameOrID covers both ways an operator finds a
// channel: typing a name fragment, or typing the id directly (e.g. pasting
// one they already half-remember).
func TestSearchStreamNamesMatchesNameOrID(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(
		map[string]string{"101": "BBC One", "102": "BBC Two", "200": "CNN"},
		nil,
		map[string]string{"101": "UK", "102": "UK", "200": "News"},
		"api",
	); err != nil {
		t.Fatal(err)
	}

	byName, err := m.SearchStreamNames("bbc", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(byName) != 2 {
		t.Fatalf("got %d results for 'bbc', want 2: %+v", len(byName), byName)
	}

	byID, err := m.SearchStreamNames("101", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(byID) != 1 || byID[0].StreamID != "101" || byID[0].Category != "UK" {
		t.Fatalf("got %+v, want one match for stream_id 101 with category UK", byID)
	}
}

// TestSearchStreamNamesEmptyQueryReturnsNothing guards against ever dumping
// the full table — this backs a type-to-search picker, not a listing.
func TestSearchStreamNamesEmptyQueryReturnsNothing(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(map[string]string{"1": "One"}, nil, nil, "api"); err != nil {
		t.Fatal(err)
	}
	results, err := m.SearchStreamNames("", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("got %d results for an empty query, want 0", len(results))
	}
}

// TestSearchStreamNamesRespectsLimit is the regression test for a provider
// with a huge lineup and a common query term.
func TestSearchStreamNamesRespectsLimit(t *testing.T) {
	m := testDB(t)
	names := make(map[string]string, 30)
	for i := 0; i < 30; i++ {
		names[fmt.Sprintf("%d", i)] = fmt.Sprintf("Sports %d", i)
	}
	if err := m.UpsertStreamNames(names, nil, nil, "api"); err != nil {
		t.Fatal(err)
	}
	results, err := m.SearchStreamNames("sports", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 10 {
		t.Fatalf("got %d results, want the limit of 10", len(results))
	}
}

// TestSearchStreamNamesDedupesAcrossSources: the same stream_id can be
// indexed from more than one source (api and m3u both run their own
// harvest). A search must not show the same channel twice, and must prefer
// the api-sourced row — the one with a category, since only the api harvest
// resolves one.
func TestSearchStreamNamesDedupesAcrossSources(t *testing.T) {
	m := testDB(t)
	if err := m.UpsertStreamNames(map[string]string{"1": "BBC One (m3u)"}, nil, nil, "m3u"); err != nil {
		t.Fatal(err)
	}
	if err := m.UpsertStreamNames(
		map[string]string{"1": "BBC One"}, nil, map[string]string{"1": "UK"}, "api",
	); err != nil {
		t.Fatal(err)
	}

	results, err := m.SearchStreamNames("bbc", 25)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("got %d results, want 1 (deduped): %+v", len(results), results)
	}
	if results[0].Name != "BBC One" || results[0].Category != "UK" {
		t.Fatalf("got %+v, want the api-sourced row (name=%q category=%q)", results[0], "BBC One", "UK")
	}
}
```

Create `pkg/server/handlers_channels_test.go`:

```go
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
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go build ./... && go test ./pkg/database/... ./pkg/server/...`
Expected: build failures — `types.ChannelMatch` undefined, `SearchStreamNames` undefined, `c.searchChannels` undefined.

- [ ] **Step 3: Add `types.ChannelMatch`**

In `pkg/types/types.go`, directly after the `APIResponse` struct:

```go
// ChannelMatch is one live-channel suggestion for the health-check probe
// picker: a name (and, when known, its provider category) for a stream_id.
type ChannelMatch struct {
	StreamID string
	Name     string
	Category string
}
```

- [ ] **Step 4: Add `SearchStreamNames`**

In `pkg/database/stream_names.go`, add the `types` import to the existing import block:

```go
import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)
```

Append at the end of the file, after `LoadStreamNames`:

```go
// SearchStreamNames returns up to limit channels whose name matches query
// (case-insensitive substring) or whose stream_id starts with it. The same
// stream_id can be indexed from more than one source (api and m3u both run
// their own harvest); a match is deduplicated to one row, preferring api
// (the only source with a resolved category) over m3u over anything else —
// same preference order as "API is authoritative" elsewhere in this file.
//
// An empty query or an uninitialized database return no rows rather than an
// error: this backs a type-to-search picker in a wizard step that must never
// surface a scary failure just because the index isn't warm yet.
func (m *DBManager) SearchStreamNames(query string, limit int) ([]types.ChannelMatch, error) {
	results := make([]types.ChannelMatch, 0)
	query = strings.TrimSpace(query)
	if m == nil || m.db == nil || query == "" {
		return results, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rows, err := m.db.QueryContext(ctx, `
		SELECT stream_id, name, category FROM (
			SELECT DISTINCT ON (stream_id) stream_id, name, category
			FROM stream_names
			WHERE name ILIKE '%' || $1 || '%' OR stream_id LIKE $1 || '%'
			ORDER BY stream_id, CASE source WHEN 'api' THEN 0 WHEN 'm3u' THEN 1 ELSE 2 END
		) deduped
		ORDER BY name
		LIMIT $2`,
		query, limit,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var match types.ChannelMatch
		if err := rows.Scan(&match.StreamID, &match.Name, &match.Category); err != nil {
			return nil, err
		}
		results = append(results, match)
	}
	return results, rows.Err()
}
```

- [ ] **Step 5: Add the `searchChannels` handler and route**

Create `pkg/server/handlers_channels.go`:

```go
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
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

const channelSearchLimit = 25

// searchChannels GET /api/internal/channels?q=... suggests live channels by
// name (or stream_id) for the health-check wizard's probe-channel picker, so
// an operator doesn't have to already know a raw Xtream stream ID. Reads only
// data already collected and persisted at startup (see warmChannelNameIndex
// and warmCategoryNameIndex) — this never calls the upstream provider.
func (c *Config) searchChannels(ctx *gin.Context) {
	query := strings.TrimSpace(ctx.Query("q"))
	results, err := c.db.SearchStreamNames(query, channelSearchLimit)
	if err != nil {
		utils.ErrorLog("Channel search failed: %v", err)
		ctx.JSON(http.StatusInternalServerError, types.APIResponse{
			Success: false,
			Error:   "Failed to search channels: " + err.Error(),
		})
		return
	}
	ctx.JSON(http.StatusOK, types.APIResponse{Success: true, Data: results})
}
```

In `pkg/server/api.go`, add the route directly after the VOD block (after line 82, `api.GET("/vod/status/:requestid", c.getVODRequestStatus)`):

```go
	// Channel search for the health-check wizard's probe-channel picker —
	// reads the name/category index warmed at startup, never the provider.
	api.GET("/channels", c.searchChannels)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go build ./... && go test ./pkg/server/... -run TestSearchChannels -v`
Expected: PASS (2 tests)

Run: `SS_TEST_DSN="postgres://user:pass@localhost/sstest?sslmode=disable" go test ./pkg/database/... -run TestSearchStreamNames -v`
Expected: PASS (4 tests)

- [ ] **Step 7: Run the full test suite and a manual smoke check**

Run: `go build ./... && go test ./...`
Expected: PASS everywhere.

Manual check (needs a running instance with a real Xtream provider configured, after Task 1's warm-up has had a moment to run):

```bash
curl -s "http://localhost:<port>/api/internal/channels?q=<part of a real channel name>" \
  -H "X-API-Key: <the instance's api key>" | jq .
```

Expected: `{"success":true,"data":[{"StreamID":"...","Name":"...","Category":"..."}]}` with at least one match, `Category` populated for anything the provider organizes into a category.

- [ ] **Step 8: Commit**

```bash
git add pkg/types/types.go pkg/database/stream_names.go pkg/database/stream_names_pg_test.go \
        pkg/server/handlers_channels.go pkg/server/handlers_channels_test.go pkg/server/api.go
git commit -m "$(cat <<'EOF'
Add GET /api/internal/channels channel search endpoint

Backs the health-check wizard's channel picker: searches the
stream_names table already populated by the channel-name harvest,
deduplicating a stream_id indexed from more than one source and
preferring the api-sourced row (the one with a resolved category).
Never calls the provider — an empty query or a not-yet-initialized
database just return no matches.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Plan self-review notes

- **Spec coverage:** the new endpoint and its response shape (Task 2), category harvesting via one flat `get_live_categories` call rather than the per-category loop (Task 1), the `stream_names.category` column (Task 1), search matching name-or-id with a result cap (Task 2). The Suite-side proxy route and wizard UI are the companion plan in `stream-share-suite`, not this one.
- **Placeholder scan:** none found — every step has real code or a real command.
- **Type consistency:** `UpsertStreamNames(names, epgIDs, categories map[string]string, source string)` is the same signature everywhere it's called across both tasks and both test files; `types.ChannelMatch{StreamID, Name, Category}` matches what `SearchStreamNames` scans into and what `searchChannels` serializes.
- **Deviation from the spec:** the spec's data-flow sketch showed `SearchStreamNames` reading the table without mentioning cross-source duplication. Investigating the schema (`PRIMARY KEY (stream_id, source)`, i.e. the same channel can have one row per source) surfaced a real duplicate-result bug the spec didn't anticipate; Task 2 handles it with `DISTINCT ON` preferring `api` over `m3u` over anything else, covered by `TestSearchStreamNamesDedupesAcrossSources`.
