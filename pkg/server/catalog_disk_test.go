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

package server

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/filter"
)

// diskTestBody covers string and numeric ids, category_ids, a hidden name, a
// hidden category, a non-object element, numbers and HTML-escaped text that
// must survive re-encoding, and trailing bytes after the array.
const diskTestBody = `[{"stream_id":"1","name":"A| One","category_id":"42","tv_archive":0,"epg_channel_id":"one.a"},
 {"stream_id":2,"name":"A| Two","category_id":43,"tv_archive":1,"category_ids":[43,7]},
 {"stream_id":"7703","name":"B| Hidden","category_id":"42"},
 {"stream_id":"4","name":"A| Four","category_id":"99"},
 "junk",
 {"stream_id":"5","name":"A| Five <b>&</b>","category_id":"10","category_ids":["7"],"rating":1.50},
 {"stream_id":"6","name":"A| Six","category_ids":[7,"7"]}] trailing`

// writeDiskCatalog writes body to a fresh file and returns the disk store and
// its in-memory reference, decoded the way the cache decodes (first value only).
func writeDiskCatalog(t *testing.T, body string) (*diskCatalog, *memoryCatalog) {
	t.Helper()
	raw := filepath.Join(t.TempDir(), "catalog")
	if err := os.WriteFile(raw, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(strings.NewReader(body))
	dec.UseNumber()
	var data interface{}
	if err := dec.Decode(&data); err != nil {
		t.Fatal(err)
	}
	return newDiskCatalog(raw), &memoryCatalog{data: data}
}

// diskTestConfig caches category lists for every kind; with filtered set, the
// filter hides category 99 ("B| Sport") and names not starting with "A|".
func diskTestConfig(t *testing.T, filtered, withCatchup bool) *Config {
	t.Helper()
	invalidateCatalogCache()
	c := playerAPITestConfig("")
	cats := []interface{}{
		map[string]interface{}{"category_id": "42", "category_name": "A| News"},
		map[string]interface{}{"category_id": "43", "category_name": "A| Two"},
		map[string]interface{}{"category_id": "10", "category_name": "A| Ten"},
		map[string]interface{}{"category_id": "99", "category_name": "B| Sport"},
	}
	for _, a := range []string{"get_live_categories", "get_vod_categories", "get_series_categories"} {
		storeCachedCatalog(c.catalogKey(a), cats)
	}
	if filtered {
		set, err := filter.NewSet(filter.Config{All: filter.Patterns{Group: `^(A\||$)`, Name: `^A\|`}})
		if err != nil {
			t.Fatal(err)
		}
		c.catalogFilter = set
	}
	if withCatchup {
		c.catchupManager = catchup.New(true, t.TempDir(), 3)
	}
	return c
}

type servedCatalog struct {
	body                  string
	ctype, vary, encoding string
}

// serveTest runs serve against a test request and returns the decompressed body.
func serveTest(t *testing.T, gz bool, serve func(ctx *gin.Context) error) servedCatalog {
	t.Helper()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	if gz {
		ctx.Request.Header.Set("Accept-Encoding", "gzip")
	}
	if err := serve(ctx); err != nil {
		t.Fatal(err)
	}
	h := w.Result().Header
	body := w.Body.Bytes()
	if h.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		if body, err = io.ReadAll(zr); err != nil {
			t.Fatal(err)
		}
	}
	return servedCatalog{string(body), h.Get("Content-Type"), h.Get("Vary"), h.Get("Content-Encoding")}
}

type diskCombo struct {
	action              string
	filtered, withCatch bool
}

func diskCombos() []diskCombo {
	var out []diskCombo
	for _, action := range []string{"get_live_streams", "get_vod_streams", "get_series"} {
		for _, filtered := range []bool{false, true} {
			out = append(out, diskCombo{action, filtered, false})
			if action == "get_live_streams" {
				out = append(out, diskCombo{action, filtered, true})
			}
		}
	}
	return out
}

func TestDiskCatalogEachMatchesMemory(t *testing.T) {
	d, m := writeDiskCatalog(t, diskTestBody)
	collect := func(s catalogStore, limit int) []map[string]interface{} {
		var out []map[string]interface{}
		if err := s.each(func(it map[string]interface{}) bool {
			out = append(out, it)
			return len(out) < limit
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	for _, limit := range []int{100, 2} {
		got, want := collect(d, limit), collect(m, limit)
		if len(want) != min(limit, 6) || !reflect.DeepEqual(got, want) {
			t.Fatalf("limit %d: disk %v, memory %v", limit, got, want)
		}
	}
	raw := filepath.Join(t.TempDir(), "truncated")
	if err := os.WriteFile(raw, []byte(`[{"stream_id":"1"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newDiskCatalog(raw).each(func(map[string]interface{}) bool { return true }); err == nil {
		t.Fatal("a truncated file must be an error, not a shorter list")
	}
}

func TestDiskCatalogWholeIsByteIdentical(t *testing.T) {
	for _, cb := range diskCombos() {
		c := diskTestConfig(t, cb.filtered, cb.withCatch)
		d, m := writeDiskCatalog(t, diskTestBody)
		for _, gz := range []bool{false, true} {
			want := serveTest(t, gz, func(ctx *gin.Context) error { return m.serveWhole(ctx, c, cb.action) })
			got := serveTest(t, gz, func(ctx *gin.Context) error { return d.serveWhole(ctx, c, cb.action) })
			if got != want {
				t.Fatalf("%+v gzip=%v:\ndisk   %+v\nmemory %+v", cb, gz, got, want)
			}
		}
	}
}

func TestDiskCatalogSliceIsByteIdentical(t *testing.T) {
	for _, cb := range diskCombos() {
		c := diskTestConfig(t, cb.filtered, cb.withCatch)
		d, m := writeDiskCatalog(t, diskTestBody)
		for _, cat := range []string{"42", "43", "7", "999"} {
			for _, gz := range []bool{false, true} {
				want := serveTest(t, gz, func(ctx *gin.Context) error { return m.serveSlice(ctx, c, cb.action, cat) })
				got := serveTest(t, gz, func(ctx *gin.Context) error { return d.serveSlice(ctx, c, cb.action, cat) })
				if got != want {
					t.Fatalf("%+v category %s gzip=%v:\ndisk   %+v\nmemory %+v", cb, cat, gz, got, want)
				}
			}
		}
	}
}

func TestDiskCatalogRendersOnceConcurrently(t *testing.T) {
	c := diskTestConfig(t, false, false)
	d, _ := writeDiskCatalog(t, diskTestBody)
	wave := func() []string {
		bodies := make([]string, 20)
		var wg sync.WaitGroup
		for i := range bodies {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				w := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(w)
				ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
				if err := d.serveWhole(ctx, c, "get_vod_streams"); err != nil {
					t.Error(err)
				}
				bodies[i] = w.Body.String()
			}(i)
		}
		wg.Wait()
		return bodies
	}
	first := wave()
	info, err := os.Stat(d.raw + ".render")
	if err != nil {
		t.Fatal(err)
	}
	second := wave()
	for _, b := range append(first, second...) {
		if b != first[0] || b == "" {
			t.Fatalf("bodies differ: %q vs %q", b, first[0])
		}
	}
	if again, err := os.Stat(d.raw + ".render"); err != nil || !again.ModTime().Equal(info.ModTime()) {
		t.Fatalf("render file rewritten: %v %v", again.ModTime(), info.ModTime())
	}
}

func TestDiskCatalogRenderFailureIsRetried(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	c := diskTestConfig(t, false, false)
	d, _ := writeDiskCatalog(t, diskTestBody)
	dir := filepath.Dir(d.raw)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php", nil)
	if err := d.serveWhole(ctx, c, "get_vod_streams"); err == nil {
		t.Fatal("render into a read-only directory must fail")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("failed render left files: %v", entries)
	}
	if got := serveTest(t, false, func(ctx *gin.Context) error { return d.serveWhole(ctx, c, "get_vod_streams") }); !strings.HasPrefix(got.body, "[{") {
		t.Fatalf("retry body = %q", got.body)
	}
}

func TestDiskCatalogHarvestsNamesFromWholeList(t *testing.T) {
	c := diskTestConfig(t, true, false)
	d, _ := writeDiskCatalog(t, diskTestBody)
	// Replace whatever index earlier tests left.
	reset := newChannelHarvest()
	reset.add(map[string]interface{}{"stream_id": "1", "name": "placeholder"})
	c.applyChannelHarvest(reset)
	got := serveTest(t, false, func(ctx *gin.Context) error { return d.serveWhole(ctx, c, "get_live_streams") })
	if strings.Contains(got.body, "Hidden") {
		t.Fatalf("filter did not hide the channel: %s", got.body)
	}
	if name, ok := lookupAPIChannelName(normalizeStreamID("7703")); !ok || name != "B| Hidden" {
		t.Fatalf("hidden channel name = %q %v", name, ok)
	}
}

func TestDiskCatalogRemoveDeletesFiles(t *testing.T) {
	c := diskTestConfig(t, false, false)
	d, _ := writeDiskCatalog(t, diskTestBody)
	serveTest(t, false, func(ctx *gin.Context) error { return d.serveWhole(ctx, c, "get_vod_streams") })
	d.remove()
	if entries, _ := os.ReadDir(filepath.Dir(d.raw)); len(entries) != 0 {
		t.Fatalf("remove left files: %v", entries)
	}
}
