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

package xtream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/lucasduport/stream-share/pkg/config"
)

func TestClassifyCatalogFile(t *testing.T) {
	fb := FallbackForAction(getLiveStreams)
	cases := []struct {
		name, body string
		kind       CatalogFileKind
		small      interface{}
		wantErr    bool
	}{
		{"empty", "", CatalogFileSmall, fb, false},
		{"blank", " \n ", CatalogFileSmall, fb, false},
		{"null", "null", CatalogFileSmall, fb, false},
		{"html", "<html>", CatalogFileSmall, fb, false},
		{"empty object", "{}", CatalogFileSmall, map[string]interface{}{}, false},
		{"empty array", "[]", CatalogFileSmall, []interface{}{}, false},
		{"padded empty array", "\n [] \n", CatalogFileSmall, []interface{}{}, false},
		{"array", `[{"a":1},{"a":2}]`, CatalogFileArray, nil, false},
		{"padded array", " \n[{\"a\":1}]\n", CatalogFileArray, nil, false},
		{"truncated", `[{"a":1},`, CatalogFileSmall, fb, true},
		{"truncated open", `[`, CatalogFileSmall, fb, true},
		{"trailing garbage", `[{"a":1}] x`, CatalogFileArray, nil, false},
		{"object", `{"1":{"a":1}}`, CatalogFileSmall, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "c.json")
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			kind, small, err := ClassifyCatalogFile(p, getLiveStreams)
			if (err != nil) != tc.wantErr || kind != tc.kind {
				t.Fatalf("kind=%v err=%v, want kind=%v wantErr=%v", kind, err, tc.kind, tc.wantErr)
			}
			if tc.wantErr && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("err = %v, want unexpected EOF", err)
			}
			if tc.name == "object" {
				if m, ok := small.(map[string]interface{}); !ok || len(m) != 1 {
					t.Fatalf("small = %#v", small)
				}
				return
			}
			if !reflect.DeepEqual(small, tc.small) {
				t.Fatalf("small = %#v, want %#v", small, tc.small)
			}
		})
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New("u", "p", srv.URL, "ua")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestActionToFileWritesBody(t *testing.T) {
	const body = `[{"a":1},{"a":2}]`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) })
	p := filepath.Join(t.TempDir(), "c.json")
	code, err := c.ActionToFile(context.Background(), &config.ProxyConfig{}, getLiveStreams, nil, p)
	if err != nil || code != http.StatusOK {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if got, _ := os.ReadFile(p); string(got) != body {
		t.Fatalf("file = %q", got)
	}
	if _, err := os.Stat(p + ".part"); !os.IsNotExist(err) {
		t.Fatalf(".part left behind: %v", err)
	}
}

func TestActionToFileRetriesThenFails(t *testing.T) {
	hits := 0
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) { hits++; w.WriteHeader(http.StatusServiceUnavailable) })
	p := filepath.Join(t.TempDir(), "c.json")
	code, err := c.ActionToFile(context.Background(), &config.ProxyConfig{}, getLiveStreams, nil, p)
	if err == nil || code != http.StatusBadGateway || hits != 5 {
		t.Fatalf("code=%d err=%v hits=%d", code, err, hits)
	}
	for _, f := range []string{p, p + ".part"} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Fatalf("%s should not exist: %v", f, err)
		}
	}
}
