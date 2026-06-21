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

package database

import "time"

// UpsertStreamNames batch-upserts id→name pairs for the given source ("m3u", "api", "vod").
func (m *DBManager) UpsertStreamNames(names map[string]string, source string) error {
	if m == nil || m.db == nil || len(names) == 0 {
		return nil
	}
	now := time.Now()
	for id, name := range names {
		if _, err := m.db.Exec(`
            INSERT INTO stream_names (stream_id, source, name, updated_at)
            VALUES ($1, $2, $3, $4)
            ON CONFLICT (stream_id, source) DO UPDATE
                SET name = EXCLUDED.name, updated_at = EXCLUDED.updated_at
        `, id, source, name, now); err != nil {
			return err
		}
	}
	return nil
}

// UpsertStreamName upserts a single stream name.
func (m *DBManager) UpsertStreamName(streamID, source, name string) error {
	if m == nil || m.db == nil {
		return nil
	}
	_, err := m.db.Exec(`
        INSERT INTO stream_names (stream_id, source, name, updated_at)
        VALUES ($1, $2, $3, NOW())
        ON CONFLICT (stream_id, source) DO UPDATE
            SET name = EXCLUDED.name, updated_at = EXCLUDED.updated_at
    `, streamID, source, name)
	return err
}

// LoadStreamNames loads all stream names grouped by source.
// Returns map[source]map[streamID]name.
func (m *DBManager) LoadStreamNames() (map[string]map[string]string, error) {
	result := map[string]map[string]string{}
	if m == nil || m.db == nil {
		return result, nil
	}
	rows, err := m.db.Query(`SELECT stream_id, source, name FROM stream_names`)
	if err != nil {
		return result, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id, source, name string
		if err := rows.Scan(&id, &source, &name); err != nil {
			continue
		}
		if result[source] == nil {
			result[source] = map[string]string{}
		}
		result[source][id] = name
	}
	return result, rows.Err()
}
