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
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

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
	removeCatalogFiles(d.raw)
	if entries, _ := os.ReadDir(filepath.Dir(d.raw)); len(entries) != 0 {
		t.Fatalf("remove left files: %v", entries)
	}
}

// The pending grace deletion must not keep the replaced catalogue (and its
// span index) in memory: only its file path may outlive the cache entry.
func TestDiskModeGraceDoesNotPinOldCatalogue(t *testing.T) {
	invalidateCatalogCache()
	saved := catalogFileGrace
	catalogFileGrace = time.Hour
	t.Cleanup(func() { catalogFileGrace = saved })
	c := diskModeConfig(newFakeCatalogProvider(t).srv.URL)
	key := c.catalogKey("get_vod_streams")

	decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams"))
	collected := make(chan struct{})
	runtime.SetFinalizer(peekCatalogEntry(key).data.(*diskCatalog), func(*diskCatalog) { close(collected) })
	if _, _, err := fetchCatalogEntry(key, true, c.catalogFetcher("get_vod_streams", nil)); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		runtime.GC()
		select {
		case <-collected:
			return
		case <-time.After(50 * time.Millisecond):
		}
	}
	t.Fatal("replaced disk catalogue still reachable during the grace period")
}

func diskModeConfig(baseURL string) *Config {
	c := playerAPITestConfig(baseURL)
	c.CatalogStorage = catalogStorageDisk
	return c
}

func TestDiskModeReplacedFilesRemovedAfterGrace(t *testing.T) {
	invalidateCatalogCache()
	saved := catalogFileGrace
	catalogFileGrace = 50 * time.Millisecond
	t.Cleanup(func() { catalogFileGrace = saved })
	c := diskModeConfig(newFakeCatalogProvider(t).srv.URL)
	key := c.catalogKey("get_vod_streams")

	decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams"))
	old, ok := peekCatalogEntry(key).data.(*diskCatalog)
	if !ok {
		t.Fatalf("disk mode cached %T, want *diskCatalog", peekCatalogEntry(key).data)
	}
	e, _, err := fetchCatalogEntry(key, true, c.catalogFetcher("get_vod_streams", nil))
	if err != nil {
		t.Fatal(err)
	}
	cur := e.data.(*diskCatalog)
	if _, err := os.Stat(old.raw); err != nil {
		t.Fatalf("old file removed before the grace: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := os.Stat(old.raw + ".render"); !os.IsNotExist(err) {
		t.Fatalf("old render still there after the grace: %v", err)
	}
	if _, err := os.Stat(old.raw); !os.IsNotExist(err) {
		t.Fatalf("old file still there after the grace: %v", err)
	}
	if _, err := os.Stat(cur.raw); err != nil {
		t.Fatalf("current file: %v", err)
	}
}

// A provider answering a stream list with an object is served as in memory
// mode (Review Focus 1).
func TestDiskModeObjectAnswerServedAsToday(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"1":{"name":"A"}}`))
	}))
	t.Cleanup(srv.Close)
	bodies := map[string]string{}
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage
		w := callPlayerAPI(t, c, "action=get_vod_streams")
		if w.Code != http.StatusOK {
			t.Fatalf("status %d", w.Code)
		}
		bodies[storage] = w.Body.String()
	})
	if bodies["memory"] != bodies["disk"] {
		t.Fatalf("disk body %q, memory body %q", bodies["disk"], bodies["memory"])
	}
}

// A cache folder that cannot be written is a failed refresh: the last good
// catalogue stays, and with none the player gets 502 (Review Focus 4).
func TestDiskModeUnwritableCacheKeepsLastGood(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	invalidateCatalogCache()
	c := diskModeConfig(newFakeCatalogProvider(t).srv.URL)
	key := c.catalogKey("get_series")
	first := callPlayerAPI(t, c, "action=get_series")
	decodeList(t, first)

	dir, err := catalogRunDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })

	t.Setenv("CATALOG_CACHE_TTL_MINUTES", "1")
	catalogCacheMu.Lock()
	catalogCache[key].fetchedAt = time.Now().Add(-2 * time.Minute)
	catalogCacheMu.Unlock()
	if e, stale, err := fetchCatalogEntry(key, true, c.catalogFetcher("get_series", nil)); err != nil || !stale || e == nil {
		t.Fatalf("forced refresh into an unwritable dir: entry=%v stale=%v err=%v, want last good copy", e != nil, stale, err)
	}
	if again := callPlayerAPI(t, c, "action=get_series"); again.Code != http.StatusOK || again.Body.String() != first.Body.String() {
		t.Fatalf("after failed refresh: %d %q, want the last good body", again.Code, again.Body.String())
	}

	invalidateCatalogCache()
	if w := callPlayerAPI(t, c, "action=get_series"); w.Code != http.StatusBadGateway {
		t.Fatalf("nothing cached, unwritable dir: status %d, want 502", w.Code)
	}
}

// Channel numbers come from a disk-backed live catalogue too.
func TestLiveChannelNumberFromDiskCatalogue(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	d, _ := writeDiskCatalog(t, `[{"num":41,"stream_id":"506638"},{"num":"7","stream_id":9}]`)
	storeCachedCatalog(c.catalogKey("get_live_streams"), d)
	for id, want := range map[string]int{"506638": 41, "9": 7} {
		if n, ok := c.liveChannelNumber(id); !ok || n != want {
			t.Errorf("liveChannelNumber(%q) = %d, %v; want %d", id, n, ok, want)
		}
	}
}
