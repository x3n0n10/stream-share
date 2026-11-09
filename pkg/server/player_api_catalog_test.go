/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/catchup"
	"github.com/lucasduport/stream-share/pkg/config"
)

// fakeCatalogProvider is a player_api.php stand-in that counts requests per
// action+category and can be switched to failing.
type fakeCatalogProvider struct {
	srv   *httptest.Server
	mu    sync.Mutex
	hits  map[string]int
	fail  atomic.Bool
	delay time.Duration
}

func newFakeCatalogProvider(t *testing.T) *fakeCatalogProvider {
	t.Helper()
	p := &fakeCatalogProvider{hits: map[string]int{}}
	p.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.URL.Query().Get("action")
		cat := r.URL.Query().Get("category_id")
		p.mu.Lock()
		p.hits[action+"|"+cat]++
		p.mu.Unlock()
		if p.delay > 0 {
			time.Sleep(p.delay)
		}
		if p.fail.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case cat != "":
			_, _ = fmt.Fprintf(w, `[{"stream_id":"9","name":"Only %s","tv_archive":0}]`, cat)
		default:
			_, _ = w.Write([]byte(`[{"stream_id":"1","name":"One","category_id":"42","tv_archive":0},{"stream_id":"2","name":"Two","category_id":43,"tv_archive":1}]`))
		}
	}))
	t.Cleanup(p.srv.Close)
	return p
}

func (p *fakeCatalogProvider) count(action, cat string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.hits[action+"|"+cat]
}

func playerAPITestConfig(baseURL string) *Config {
	gin.SetMode(gin.TestMode)
	return &Config{ProxyConfig: &config.ProxyConfig{XtreamBaseURL: baseURL, XtreamUser: "prov", XtreamPassword: "secret"}, imageKey: testImageKey}
}

func callPlayerAPI(t *testing.T, c *Config, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	return callPlayerAPIHost(t, c, "example.com", rawQuery)
}

// callPlayerAPIHost is callPlayerAPI with the request's Host set to host, so
// the client-facing base URL is http://host.
func callPlayerAPIHost(t *testing.T, c *Config, host, rawQuery string) *httptest.ResponseRecorder {
	t.Helper()
	q, err := url.ParseQuery(rawQuery)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php?"+rawQuery, nil)
	ctx.Request.Host = host
	c.xtreamPlayerAPI(ctx, q)
	return w
}

func decodeList(t *testing.T, w *httptest.ResponseRecorder) []map[string]interface{} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("body is not a JSON list: %v (%s)", err, w.Body.String())
	}
	return out
}

func TestPlayerAPICatalogServedFromCache(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage

		for i := 0; i < 3; i++ {
			if got := decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams")); len(got) != 2 {
				t.Fatalf("call %d: %d items, want 2", i, len(got))
			}
		}
		if n := p.count("get_vod_streams", ""); n != 1 {
			t.Fatalf("provider hit %d times, want 1", n)
		}
		// The /watch search shares the same cache entry.
		if _, err := c.getCachedVODCatalog(); err != nil {
			t.Fatal(err)
		}
		if n := p.count("get_vod_streams", ""); n != 1 {
			t.Fatalf("search re-fetched the catalogue: %d provider hits", n)
		}
	})
}

func TestPlayerAPICategorySlicedFromFullCatalogue(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage

		for _, tc := range []struct {
			cat  string
			want []string
		}{{"42", []string{"One"}}, {"43", []string{"Two"}}, {"999", nil}} {
			got := decodeList(t, callPlayerAPI(t, c, "action=get_live_streams&category_id="+tc.cat))
			if len(got) != len(tc.want) {
				t.Fatalf("category %s: got %v, want %v", tc.cat, got, tc.want)
			}
			for i, name := range tc.want {
				if got[i]["name"] != name {
					t.Fatalf("category %s: got %v, want %v", tc.cat, got, tc.want)
				}
			}
		}
		if a, b := p.count("get_live_streams", ""), p.count("get_live_streams", "42"); a != 1 || b != 0 {
			t.Fatalf("provider hits: full=%d category=%d, want 1 and 0", a, b)
		}
		catalogCacheMu.RLock()
		n := len(catalogCache)
		catalogCacheMu.RUnlock()
		if n != 1 {
			t.Fatalf("catalog cache holds %d entries, want only the full list", n)
		}
		if _, ok := c.playerAPICatalogKey("get_live_categories", url.Values{"category_id": {"42"}}); ok {
			t.Fatal("a category list has no category_id slice and must bypass the cache")
		}
	})
}

func TestPlayerAPIUnknownParamsBypassCache(t *testing.T) {
	invalidateCatalogCache()
	c := playerAPITestConfig("http://provider.invalid")
	if _, ok := c.playerAPICatalogKey("get_vod_streams", url.Values{"limit": {"10"}}); ok {
		t.Fatal("request with an unknown parameter must not use the catalog cache")
	}
	if _, ok := c.playerAPICatalogKey("get_vod_info", url.Values{"vod_id": {"1"}}); ok {
		t.Fatal("per-item actions must not use the catalog cache")
	}
}

func TestPlayerAPIServesLastGoodCatalogueWhenProviderFails(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage

		decodeList(t, callPlayerAPI(t, c, "action=get_series"))
		p.fail.Store(true)

		// Expire the entry: the next request must still answer, from the last
		// good copy, while a refresh is attempted.
		key := c.catalogKey("get_series")
		catalogCacheMu.Lock()
		catalogCache[key].fetchedAt = time.Now().Add(-2 * catalogCacheTTL())
		catalogCacheMu.Unlock()

		if got := decodeList(t, callPlayerAPI(t, c, "action=get_series")); len(got) != 2 {
			t.Fatalf("stale catalogue: %d items, want 2", len(got))
		}
		// A forced refresh failing also falls back to the last good copy.
		e, stale, err := fetchCatalogEntry(key, true, c.catalogFetcher("get_series", nil))
		if err != nil || !stale || e == nil {
			t.Fatalf("forced refresh on failure: entry=%v stale=%v err=%v, want last good copy", e != nil, stale, err)
		}
		waitFor(t, func() bool { return p.count("get_series", "") >= 2 })
	})
}

func TestPlayerAPIEmptyAnswerNotCached(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		var hits int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&hits, 1)
			_, _ = w.Write([]byte(`[]`))
		}))
		defer srv.Close()
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage

		for i := 0; i < 2; i++ {
			if got := decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams")); len(got) != 0 {
				t.Fatalf("got %d items, want the provider's empty list", len(got))
			}
		}
		if n := atomic.LoadInt32(&hits); n != 2 {
			t.Fatalf("provider hit %d times, want 2 (empty answers are not cached)", n)
		}
	})
}

func TestPlayerAPIConcurrentRequestsCoalesce(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		p.delay = 50 * time.Millisecond
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage

		var wg sync.WaitGroup
		for i := 0; i < 10; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if w := callPlayerAPI(t, c, "action=get_vod_streams"); w.Code != http.StatusOK {
					t.Errorf("status = %d", w.Code)
				}
			}()
		}
		wg.Wait()
		if n := p.count("get_vod_streams", ""); n != 1 {
			t.Fatalf("provider hit %d times, want 1", n)
		}
	})
}

func TestPlayerAPICatchupFlagsDoNotMutateCachedCatalogue(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage
		c.catchupManager = catchup.New(true, t.TempDir(), 3)

		for _, it := range decodeList(t, callPlayerAPI(t, c, "action=get_live_streams")) {
			if fmt.Sprint(it["tv_archive"]) != "1" {
				t.Fatalf("player answer not flagged for catchup: %v", it)
			}
		}
		if !c.catchupManager.HasUpstreamCatchup("2") || c.catchupManager.HasUpstreamCatchup("1") {
			t.Fatal("upstream catchup support must come from the provider's own tv_archive")
		}
		raw, ok := getCachedCatalog(c.catalogKey("get_live_streams"))
		if !ok {
			t.Fatal("catalogue not cached")
		}
		_ = catalogStoreOf(raw).each(func(m map[string]interface{}) bool {
			if m["stream_id"] == "1" && fmt.Sprint(m["tv_archive"]) != "0" {
				t.Fatalf("cached catalogue was mutated: %v", m)
			}
			return true
		})
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// BenchmarkPlayerAPIVODStreams measures a cache hit on a 50k-item catalogue:
// the rendered JSON is reused, so a hit is a copy of bytes, not an encode.
func BenchmarkPlayerAPIVODStreams(b *testing.B) {
	invalidateCatalogCache()
	var sb strings.Builder
	sb.WriteString("[")
	for i := 0; i < 50000; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"num":%d,"name":"Movie %d (2021) 4K","stream_type":"movie","stream_id":%d,"stream_icon":"http://img.example/%d.jpg","rating":"7.1","added":"1690000000","category_id":"12","container_extension":"mkv"}`, i, i, i, i)
	}
	sb.WriteString("]")
	body := sb.String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := playerAPITestConfig(srv.URL)
	q := url.Values{"action": {"get_vod_streams"}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(w)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/player_api.php?action=get_vod_streams", nil)
		c.xtreamPlayerAPI(ctx, q)
		if w.Code != http.StatusOK {
			b.Fatalf("status %d", w.Code)
		}
	}
}

// A category slice is flagged for catchup like the whole list, but must not
// replace the manager's provider-wide upstream-catchup map with its own subset.
func TestPlayerAPICategorySliceKeepsUpstreamCatchupMap(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		p := newFakeCatalogProvider(t)
		c := playerAPITestConfig(p.srv.URL)
		c.CatalogStorage = storage
		c.catchupManager = catchup.New(true, t.TempDir(), 3)

		decodeList(t, callPlayerAPI(t, c, "action=get_live_streams"))
		got := decodeList(t, callPlayerAPI(t, c, "action=get_live_streams&category_id=42"))
		if len(got) != 1 || fmt.Sprint(got[0]["tv_archive"]) != "1" {
			t.Fatalf("slice not flagged for catchup: %v", got)
		}
		// Stream 2 is in category 43, outside the slice, and has native catchup.
		if !c.catchupManager.HasUpstreamCatchup("2") {
			t.Fatal("a category request wiped the upstream catchup map for other categories")
		}
	})
}

// An item a panel lists under several categories (category_ids) is part of
// each of them, as it is when the provider filters.
func TestPlayerAPICategorySliceMatchesCategoryIDs(t *testing.T) {
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`[{"stream_id":"1","category_id":"10","category_ids":[10,20]},` +
				`{"stream_id":"2","category_id":"20"},{"stream_id":"3","category_id":"30","category_ids":[30]}]`))
		}))
		t.Cleanup(srv.Close)
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage

		for cat, want := range map[string]string{"10": "1", "20": "1,2", "30": "3", "99": ""} {
			if got := ids(decodeList(t, callPlayerAPI(t, c, "action=get_vod_streams&category_id="+cat)), "stream_id"); got != want {
				t.Fatalf("category %s: got streams %q, want %q", cat, got, want)
			}
		}
	})
}

// imageCatalogProvider answers every stream list with entries carrying
// provider and third-party URLs, and a direct_source.
func imageCatalogProvider(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[{"stream_id":"1","name":"One","category_id":"42","stream_icon":"http://prov.example/1.png","direct_source":"http://prov.example/live/u/p/1.ts"},` +
			`{"stream_id":"2","name":"Two","category_id":"43","cover":"https://cdn.example/c.jpg","backdrop_path":["https://cdn.example/b.jpg"]}]`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// assertClientSafe fails unless every URL in the decoded body v is a signed
// /img link on base, and no direct_source is left. It returns how many URLs
// it checked.
func assertClientSafe(t *testing.T, c *Config, base string, v interface{}) int {
	t.Helper()
	n := 0
	switch val := v.(type) {
	case map[string]interface{}:
		for k, item := range val {
			if k == "direct_source" {
				t.Fatalf("direct_source handed to the player: %v", val)
			}
			n += assertClientSafe(t, c, base, item)
		}
	case []interface{}:
		for _, item := range val {
			n += assertClientSafe(t, c, base, item)
		}
	case string:
		if !isFetchableURL(val) {
			return 0
		}
		if !strings.HasPrefix(val, base+"/img?url=") {
			t.Fatalf("URL %q is not a %s/img link", val, base)
		}
		u, err := url.Parse(val)
		if err != nil {
			t.Fatal(err)
		}
		if raw := u.Query().Get("url"); u.Query().Get("sig") != c.imageSignature(raw) {
			t.Fatalf("bad signature on %q", val)
		}
		n++
	}
	return n
}

func decodeAny(t *testing.T, w *httptest.ResponseRecorder) interface{} {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var v interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("body is not JSON: %v (%s)", err, w.Body.String())
	}
	return v
}

func TestCatalogListsRewriteURLs(t *testing.T) {
	srv := imageCatalogProvider(t)
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage
		for _, action := range []string{"get_live_streams", "get_vod_streams", "get_series"} {
			whole := decodeAny(t, callPlayerAPIHost(t, c, "ss.example", "action="+action))
			if n := assertClientSafe(t, c, "http://ss.example", whole); n != 3 {
				t.Fatalf("%s whole list: %d URLs rewritten, want 3", action, n)
			}
			slice := decodeAny(t, callPlayerAPIHost(t, c, "ss.example", "action="+action+"&category_id=43"))
			if n := assertClientSafe(t, c, "http://ss.example", slice); n != 2 {
				t.Fatalf("%s category slice: %d URLs rewritten, want 2", action, n)
			}
		}
	})
}

// testRenderPath is where a disk catalogue renders its list for base.
func testRenderPath(raw, base string) string {
	return fmt.Sprintf("%s.render-%x", raw, sha256.Sum256([]byte(base)))[:len(raw)+len(".render-")+8]
}

func TestCatalogRenderPerBaseURL(t *testing.T) {
	srv := imageCatalogProvider(t)
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage
		get := func(host string) interface{} {
			return decodeAny(t, callPlayerAPIHost(t, c, host, "action=get_vod_streams"))
		}
		for _, host := range []string{"a.example", "b.example"} {
			assertClientSafe(t, c, "http://"+host, get(host))
		}
		switch s := peekCatalogEntry(c.catalogKey("get_vod_streams")).data.(type) {
		case *diskCatalog:
			p := testRenderPath(s.raw, "http://a.example")
			before, err := os.Stat(p)
			if err != nil {
				t.Fatal(err)
			}
			assertClientSafe(t, c, "http://a.example", get("a.example"))
			after, err := os.Stat(p)
			if err != nil || !os.SameFile(before, after) || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("render for a.example rewritten: %v", err)
			}
		case *memoryCatalog:
			if len(s.renders) != 2 || s.renders[0].base != "http://a.example" {
				t.Fatalf("renders = %+v", s.renders)
			}
			// The memo is what the next a.example request must serve.
			s.renders[0].body = []byte(`"memo"`)
			if got := get("a.example"); got != "memo" {
				t.Fatalf("a.example re-rendered: %v", got)
			}
		default:
			t.Fatalf("cached %T", s)
		}
	})
}

func TestCatalogRenderEvictsOldestBase(t *testing.T) {
	srv := imageCatalogProvider(t)
	forEachCatalogStorage(t, func(t *testing.T, storage string) {
		c := playerAPITestConfig(srv.URL)
		c.CatalogStorage = storage
		get := func(host string) {
			assertClientSafe(t, c, "http://"+host, decodeAny(t, callPlayerAPIHost(t, c, host, "action=get_series")))
		}
		for i := 0; i < maxCatalogRenders+1; i++ {
			get(fmt.Sprintf("h%d.example", i))
		}
		first := "http://h0.example"
		switch s := peekCatalogEntry(c.catalogKey("get_series")).data.(type) {
		case *diskCatalog:
			p := testRenderPath(s.raw, first)
			for _, f := range []string{p, p + ".gz"} {
				if _, err := os.Stat(f); !os.IsNotExist(err) {
					t.Fatalf("evicted render file %s still there: %v", f, err)
				}
			}
			if files, _ := filepath.Glob(s.raw + ".render-*"); len(files) != 2*maxCatalogRenders {
				t.Fatalf("render files = %v", files)
			}
			get("h0.example")
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("re-render: %v", err)
			}
		case *memoryCatalog:
			if len(s.renders) != maxCatalogRenders {
				t.Fatalf("%d renders kept", len(s.renders))
			}
			for _, r := range s.renders {
				if r.base == first {
					t.Fatal("oldest base not evicted")
				}
			}
			get("h0.example")
		default:
			t.Fatalf("cached %T", s)
		}
	})
}

func TestCatalogRenderConcurrentBases(t *testing.T) {
	invalidateCatalogCache()
	c := diskModeConfig(imageCatalogProvider(t).URL)
	hosts := []string{"a.example", "b.example"}
	wave := func() {
		res := make([]*httptest.ResponseRecorder, 20)
		var wg sync.WaitGroup
		for i := range res {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res[i] = callPlayerAPIHost(t, c, hosts[i%2], "action=get_vod_streams")
			}(i)
		}
		wg.Wait()
		for i, w := range res {
			assertClientSafe(t, c, "http://"+hosts[i%2], decodeAny(t, w))
		}
	}
	wave()
	d := peekCatalogEntry(c.catalogKey("get_vod_streams")).data.(*diskCatalog)
	if len(d.renders) != 2 {
		t.Fatalf("%d renders for 2 hosts", len(d.renders))
	}
	stat := func() []os.FileInfo {
		var out []os.FileInfo
		for _, h := range hosts {
			info, err := os.Stat(testRenderPath(d.raw, "http://"+h))
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, info)
		}
		return out
	}
	before := stat()
	wave()
	for i, after := range stat() {
		if !os.SameFile(before[i], after) || !after.ModTime().Equal(before[i].ModTime()) {
			t.Fatalf("render for %s rewritten", hosts[i])
		}
	}
}
