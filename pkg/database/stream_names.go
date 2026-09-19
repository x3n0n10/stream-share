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

package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// upsertBatchRows is how many rows go into a single multi-row INSERT. Postgres
// caps a statement at 65535 bound parameters and each row binds 5, so the hard
// ceiling is ~13k; 500 keeps statements small while still turning a 10k-channel
// playlist into 20 round trips instead of 10,000.
const upsertBatchRows = 500

// upsertStreamNamesTimeout bounds a full persist so a slow or wedged database
// cannot pin a connection indefinitely.
const upsertStreamNamesTimeout = 30 * time.Second

// category uses CASE WHEN rather than a plain overwrite: resolveCategoryName
// (pkg/server/xtream_handlers_api.go) returns "" whenever the in-memory
// category index is cold, and a plain EXCLUDED.category would wipe a
// previously-persisted category on every such harvest. name/epg_channel_id
// still overwrite unconditionally — a channel rename should always win.
const upsertStreamNamesSuffix = `
    ON CONFLICT (stream_id, source) DO UPDATE
        SET name = EXCLUDED.name, epg_channel_id = EXCLUDED.epg_channel_id,
            category = CASE WHEN EXCLUDED.category = '' THEN stream_names.category ELSE EXCLUDED.category END,
            updated_at = EXCLUDED.updated_at`

// UpsertStreamName upserts a single stream name with an optional EPG channel ID.
func (m *DBManager) UpsertStreamName(streamID, source, name, epgChannelID string) error {
	epgIDs := map[string]string{}
	if epgChannelID != "" {
		epgIDs[streamID] = epgChannelID
	}
	return m.UpsertStreamNames(map[string]string{streamID: name}, epgIDs, nil, source)
}

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

// LoadStreamNames loads all stream names grouped by source, plus a merged EPG channel ID index.
// Returns (map[source]map[streamID]name, map[streamID]epgChannelID, error).
// The EPG index merges across sources; last non-empty value wins (api > m3u).
func (m *DBManager) LoadStreamNames() (map[string]map[string]string, map[string]string, error) {
	bySource := map[string]map[string]string{}
	epgIndex := map[string]string{}
	if m == nil || m.db == nil {
		return bySource, epgIndex, nil
	}
	rows, err := m.db.Query(`SELECT stream_id, source, name, epg_channel_id FROM stream_names`)
	if err != nil {
		return bySource, epgIndex, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, source, name, epgID string
		if err := rows.Scan(&id, &source, &name, &epgID); err != nil {
			continue
		}
		if bySource[source] == nil {
			bySource[source] = map[string]string{}
		}
		bySource[source][id] = name
		if epgID != "" {
			epgIndex[id] = epgID
		}
	}
	return bySource, epgIndex, rows.Err()
}

// escapeLikePattern escapes LIKE/ILIKE metacharacters (and the escape
// character itself) in s so it can be safely embedded as a literal substring
// in a pattern built with ESCAPE '\'. Without this, a query of e.g. "%" or
// "_" would match every row instead of being searched for literally.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s)
}

// stripStreamIDExtension trims a trailing file extension the same way
// normalizeStreamID (pkg/server/m3u_index.go) does when a channel is
// harvested — the stored stream_id never carries one, so a query that
// pastes one in (e.g. "12345.ts", copied straight out of the probe-channel
// field, which itself now fills from a picked suggestion the same way) must
// have it stripped too, or an id lookup that should be an exact prefix
// match silently returns nothing.
func stripStreamIDExtension(id string) string {
	if i := strings.Index(id, "."); i > 0 {
		return id[:i]
	}
	return id
}

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

	nameQuery := escapeLikePattern(query)
	idQuery := escapeLikePattern(stripStreamIDExtension(query))

	// Leading-wildcard ILIKE is a sequential scan — measured ~67ms worst case
	// at 40k rows, fine at today's scale. Add a trigram index (pg_trgm) on
	// name if stream_names grows past roughly 10x that.
	rows, err := m.db.QueryContext(ctx, `
		SELECT stream_id, name, category FROM (
			SELECT DISTINCT ON (stream_id) stream_id, name, category
			FROM stream_names
			WHERE name ILIKE '%' || $1 || '%' ESCAPE '\' OR stream_id LIKE $2 || '%' ESCAPE '\'
			ORDER BY stream_id, CASE source WHEN 'api' THEN 0 WHEN 'm3u' THEN 1 ELSE 2 END
		) deduped
		ORDER BY name
		LIMIT $3`,
		nameQuery, idQuery, limit,
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
