# Catalogue Disk Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Cut StreamShare's RAM on large catalogues: always-on playlist/M3U-index savings, plus an opt-in `CATALOG_STORAGE=disk` mode that keeps the three stream-list catalogues on disk, targeting ≤ 100 MB settled RSS at 22k live / 122k VOD / 20k series.

**Architecture:** The generic catalogue cache core (`catalog_cache.go`) is untouched. The catalogue fetcher stores a `catalogStore` value in `catalogEntry.data`: `*memoryCatalog` (today's parsed slice and render code, default) or `*diskCatalog` (provider body in a file, rendered once to a second file plus a `.gz` sibling, category slices served from byte spans). Every consumer reads items through `catalogStoreOf(data).each(fn)`, so it does not care which store it gets.

**Tech Stack:** Go 1.23 (`go.mod`), stdlib only (`encoding/json` streaming `Decoder`, `compress/gzip`, `runtime/debug`), Gin, vendored deps (`-mod=vendor`).

**Spec:** `docs/superpowers/specs/2026-11-06-catalog-disk-cache-design.md`

## Global Constraints

- Branch `fix/category-slice-from-full-catalogue` (PR #63). Commit per task; do not push (the controller pushes).
- Build and test with Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`. Tests need a fresh cache root: `export CACHE_FOLDER=$(mktemp -d)`.
- Full suite command (used as "suite passes" below): `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` → every package `ok`. `TestXtreamGetServesStaleWhileRefreshing` is flaky on master (~1 in 60); rerun once if only that fails.
- No new dependencies. House style: `interface{}` (not `any`), no generics, GPLv3 header (copy from `pkg/server/helpers.go`) on new `.go` files, comments say why.
- Logs and file names never contain the provider URL or credentials: use `catalogKeyLabel(key)` for anything derived from a cache key.
- Do not change `catalog_cache.go`'s cache logic (fetch, coalescing, stale/backoff, store). Do not change `catalog_cache_test.go` or `stream_numbers_test.go`.
- Player-visible `player_api.php` bodies must stay byte-identical in both modes (gzip compared after decompression).
- Setting: `CATALOG_STORAGE` / `--catalog-storage`, values `memory` (default) and `disk`; unknown → warning + `memory`.
- Disk mode only: `debug.SetMemoryLimit(90 << 20)` at startup when the `GOMEMLIMIT` env var is unset. Memory mode never touches the limit.
- Disk mode applies only to `get_live_streams`, `get_vod_streams`, `get_series`. Category lists and non-array answers always use `*memoryCatalog`.
- Old catalogue files are removed 10 minutes after their entry is replaced (`catalogFileGrace`), with the comment `// shortcut: old files are kept for a fixed 10 min grace, not refcounted; switch to refcounting if a reader can hold an entry longer`.

## Review Focus

1. A provider that answers a stream list with a JSON object instead of an array, in disk mode: must be served exactly as today (memory store). Test in Task 6.
2. Numeric `category_id` / `category_ids` values (`42` vs `"42"`): category slices must still match in both modes. Test in Task 5.
3. Many players hitting a stream list at once right after a refresh in disk mode: exactly one render, every response identical, no race. Test in Task 5.
4. `CACHE_FOLDER` not writable in disk mode: the fetch fails like a provider error (last good copy keeps serving, nothing cached when there is none); a render failure answers 500 and the next request retries. Tests in Tasks 5 and 6.
5. Playlist lines longer than 64 KB, and a playlist without trailing newline: streamed rewrite must equal today's output byte for byte. Test in Task 1.

---

## File Structure

- `tools/memprobe/fakeprov/main.go` (new): fake Xtream provider with configurable sizes.
- `tools/memprobe/measure.sh` (new): builds the binary with `-tags memprobe`, runs it against the fake provider and a Postgres, replays the request sequence, prints RSS and the heap top.
- `pprof_memprobe.go` (new, repo root, `package main`, `//go:build memprobe`): imports `net/http/pprof`, listens on `127.0.0.1:16060`.
- `pkg/server/image_proxy.go` (modify): streaming `rewritePlaylistHostsTo`.
- `pkg/server/proxy_handlers.go`, `pkg/server/xtream_handlers_stream.go`, `pkg/server/xtream_handlers_api.go` (modify): playlist routes stream.
- `pkg/server/m3u_stream_index.go` (modify): clone title, intern ext.
- `pkg/server/catalog_store.go` (new): `catalogStore` interface, `memoryCatalog`, `catalogStoreOf`, per-item helpers shared by both stores.
- `pkg/server/catalog_disk.go` (new): `diskCatalog` (iteration, render-to-disk, serving), catalogue directory and grace deletion.
- `pkg/server/catalog_storage.go` (new): `parseCatalogStorage`, `applyCatalogMemoryLimit`.
- `pkg/xtream/xtream_proxy.go` (modify): shared retry loop, `ActionToFile`, exported `FallbackForAction`.
- `pkg/xtream/catalog_file.go` (new): `ClassifyCatalogFile`.
- `pkg/utils/cache.go` (modify): `CatalogCacheDir()`.
- `pkg/config/config.go`, `cmd/root.go` (modify): the setting.
- `README.md`, `docker-compose.yml` (modify): document the setting.

---

### Task 0: Memory measurement harness

**Files:**
- Create: `tools/memprobe/fakeprov/main.go`, `tools/memprobe/measure.sh`, `pprof_memprobe.go`

**Interfaces:**
- Produces: `tools/memprobe/measure.sh <label>` prints, in order, lines `idle after warmup:`, `after full lists:`, `after slices+m3u:`, `30s later:`, `after forced refresh +30s:` each followed by `VmHWM: N MB  VmRSS: N MB`, then the top 15 of `go tool pprof -top` (inuse_space). Env: `CATALOG_STORAGE` passed through; `GO` selects the toolchain (default the Go 1.23 path above); `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER` passed through.

- [ ] **Step 1: Write the fake provider**

Port `/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad/fakeprov/main.go` (read it) into `tools/memprobe/fakeprov/main.go` with the GPL header. Keep its item shapes and its defaults `NLIVE=22000`, `NVOD=122000`, `NSERIES=20000`, `ADDR` env. Add: a `GET /bump` endpoint that changes one item name in each list (so a forced refresh produces a new body).

- [ ] **Step 2: Write `pprof_memprobe.go`**

`//go:build memprobe`, `package main`, blank-import `net/http/pprof`, `func init()` starting `http.ListenAndServe("127.0.0.1:16060", nil)` in a goroutine.

- [ ] **Step 3: Write `measure.sh`**

Port `/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad/measure.sh` (read it). Changes: build both binaries itself (`$GO build -mod=vendor -tags memprobe -o "$OUT/ss-bin" .` and the fake provider), work dir from `mktemp -d`, kill processes by PID (never `pkill -f`), and after the `30s later` sample: `curl /bump` on the provider, set `CATALOG_CACHE_TTL_MINUTES=1` at launch so the next request after 60 s triggers a background refresh; request the three whole lists again after 65 s, sleep 30 s, sample `after forced refresh +30s`. Print the pprof top at the end.

- [ ] **Step 4: Run it against the current code**

A Postgres is running on `/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad` port `5433`, db `streamshare`, user `postgres` (start with `/home/linuxbrew/.linuxbrew/opt/postgresql@16/bin/pg_ctl -D <scratchpad>/pg -o "-p 5433 -k <scratchpad>" -l <scratchpad>/pg.log start` if not).
Run: `DB_HOST=<scratchpad> DB_PORT=5433 DB_NAME=streamshare DB_USER=postgres tools/memprobe/measure.sh baseline`
Expected: all five samples print; `after slices+m3u` in the 600–850 MB range. Paste the full output in your report (the controller records it as the baseline).

- [ ] **Step 5: Verify normal builds are unaffected and commit**

Run: `$G build -mod=vendor ./... && $G vet -mod=vendor ./...` → no output.

```bash
git add tools/memprobe pprof_memprobe.go
git commit -m "chore(memprobe): add a memory measurement harness"
```

---

### Task 1: Playlist routes stream; M3U index stops pinning lines

**Files:**
- Modify: `pkg/server/image_proxy.go:255-295`, `pkg/server/proxy_handlers.go:58-68`, `pkg/server/xtream_handlers_stream.go:70-79`, `pkg/server/xtream_handlers_api.go:212-218`, `pkg/server/m3u_stream_index.go:61-104`
- Test: `pkg/server/image_proxy_test.go`

**Interfaces:**
- Produces: `func (c *Config) rewritePlaylistHostsTo(ctx *gin.Context, r io.Reader, w io.Writer) error`. `rewritePlaylistHosts(ctx, body []byte) []byte` stays (existing tests use it) as a thin wrapper over it.

- [ ] **Step 1: Write the failing test**

In `image_proxy_test.go`, `TestRewritePlaylistHostsToMatchesBytes`: for inputs (a) the existing test playlists in this file, (b) the same without trailing `\n`, (c) a playlist with a 100 000-byte `#EXTINF` line and a `\r\n` line ending, assert `rewritePlaylistHostsTo` output equals `rewritePlaylistHosts` output computed by the *old* algorithm. Keep the old algorithm in the test file as `legacyRewritePlaylistHosts` (copy the current function body) so the comparison survives the refactor.

- [ ] **Step 2: Run it**

Run: `$G test -mod=vendor ./pkg/server -run TestRewritePlaylistHostsToMatchesBytes`
Expected: FAIL to compile (`rewritePlaylistHostsTo` undefined).

- [ ] **Step 3: Implement**

Line loop with `bufio.Reader.ReadString('\n')` (no line-length limit), same per-line rules, writes each rewritten line followed by `\n` only when the input line had one. `rewritePlaylistHosts` becomes: `bytes.Buffer` + `rewritePlaylistHostsTo`.
Routes: open the file first (missing file → same 500 as today), set `Content-Disposition` as today, `w := responseWriter(ctx, "application/octet-stream")`, `rewritePlaylistHostsTo(ctx, f, w)`, `w.Close()`; a mid-stream error is logged with `utils.WarnLog` (headers are already sent). `xtream_handlers_stream.go` gains gzip-when-accepted through `responseWriter`; that is intended.
`buildStreamIndex`: store `strings.Clone(lastExtinf)` and an interned ext (small `map[string]string` local to the build).

- [ ] **Step 4: Run tests**

Run: `$G test -mod=vendor ./pkg/server -run 'RewritePlaylist|M3U|Xtream|GetM3U|StreamIndex'` → `ok`; then the full suite → all `ok`.

- [ ] **Step 5: Measure and commit**

Run `tools/memprobe/measure.sh step1`; report the output.

```bash
git add pkg/server
git commit -m "perf(playlist): stream playlist rewrites; stop pinning M3U lines in the stream index"
```

---

### Task 2: `catalogStore` with `each`; consumers and per-item helpers

**Files:**
- Create: `pkg/server/catalog_store.go`
- Modify: `pkg/server/catalog_filter.go` (`filterEntries`, `filterCategories`, `categoryNames`, `keptLiveIDs`), `pkg/server/live_shares.go` (`liveChannel`, `shareChannelsAPI`), `pkg/server/stream_numbers.go` (`buildLiveNumIndex`), `pkg/server/xtream_handlers_api.go` (`harvestChannelNames`, `injectCatchupFlags`, `warmChannelNameIndex`, `servePlayerAPICatalog` slice path), `pkg/server/server.go` (`refreshAPIChannelIndex`), `pkg/server/vod_search.go` (`searchXtreamVOD`, `searchXtreamSeries`)
- Test: `pkg/server/catalog_store_test.go` (new)

**Interfaces:**
- Produces (in `catalog_store.go`):
  - `type catalogStore interface { each(fn func(m map[string]interface{}) bool) error }` (Task 3 adds the serving methods).
  - `type memoryCatalog struct { data interface{} }`; `each` ranges over `data` when it is a `[]interface{}` (non-map elements skipped), otherwise iterates nothing and returns nil.
  - `func catalogStoreOf(data interface{}) catalogStore` — returns `data` if it already is a `catalogStore`, else `&memoryCatalog{data: data}`. In this task the cache still holds raw slices, so every consumer goes through this wrapper.
- Produces (also in `catalog_store.go`, used by Task 5's disk render):
  - `func (c *Config) keepCatalogEntry(kind filter.Kind, m map[string]interface{}, names map[string]string) bool` — the body of `filterEntries`' per-item decision; `filterEntries` calls it.
  - `type channelHarvest struct { names, epgIDs, categories map[string]string }`, `func newChannelHarvest() *channelHarvest`, `func (h *channelHarvest) add(m map[string]interface{})`, `func (c *Config) applyChannelHarvest(h *channelHarvest) int` (calls `updateAPIChannelIndex`, returns `len(h.names)`). `harvestChannelNames` becomes these three calls.
  - `func (c *Config) catchupFlagged(m map[string]interface{}, upstream map[string]bool) map[string]interface{}` — returns the flagged shallow copy and records upstream catchup in `upstream`; `injectCatchupFlags` loops over it and keeps calling `SetUpstreamCatchup` only when `whole`.
  - `func inCategory(m map[string]interface{}, categoryID string) bool` — the match rule of `categorySlice` (`category_id`, or any of `category_ids`); `categorySlice` uses it.
- Behaviour: every consumer listed keeps its loop body and gets its items from `catalogStoreOf(x).each(...)` where it used `x.([]interface{})`. Where a consumer first ran `filterPlayerAPICatalog` over a stream list (`shareChannelsAPI`, both searches), it instead fetches `names := c.categoryNames(pair)` once and skips items failing `c.keepCatalogEntry` when `c.catalogFilter.ActiveFor(kind)`. `filterCategories` memoises the used-category set per stream-list entry pointer (same pattern as `liveKeptMem`).

- [ ] **Step 1: Write the failing tests**

`catalog_store_test.go`:
- `TestCatalogStoreOfWrapsRawSlices`: `catalogStoreOf([]interface{}{map[string]interface{}{"stream_id":"1"}})` iterates one item; `catalogStoreOf("x").each` iterates none and returns nil; a store passed in is returned as is.
- `TestKeepCatalogEntryMatchesFilterEntries`: with a filter excluding category `Adult` and name `XXX`, `keepCatalogEntry` agrees with `filterEntries` on a 4-item list.
- `TestInCategoryMatchesNumericIDs`: item `{"category_id": json.Number("42")}` is in `"42"`; item `{"category_id":"1","category_ids":[]interface{}{json.Number("7")}}` is in `"7"`; not in `"8"`.
- `TestChannelHarvestCollectsNamesAndEPG`: two items → `applyChannelHarvest` returns 2 and `lookupAPIChannelName(normalizeStreamID(id))` returns both names.

- [ ] **Step 2: Run them**

Run: `$G test -mod=vendor ./pkg/server -run 'CatalogStoreOf|KeepCatalogEntry|InCategory|ChannelHarvest'` → FAIL (undefined).

- [ ] **Step 3: Implement** the interface, wrapper and helpers, and switch every consumer listed.

- [ ] **Step 4: Run tests**

New tests pass; full suite passes (search, share, filter, numbers, watch-filter, player_api tests unchanged).

- [ ] **Step 5: Measure and commit**

Run `tools/memprobe/measure.sh step2`; report (expected: same as step1 within noise — no behaviour change).

```bash
git add pkg/server
git commit -m "refactor(catalog): read catalogues through catalogStore.each; per-item filter, harvest and catchup helpers"
```

---

### Task 3: Serving through `catalogStore`; `CATALOG_STORAGE` setting

**Files:**
- Create: `pkg/server/catalog_storage.go`, `pkg/server/catalog_storage_test.go`
- Modify: `pkg/server/catalog_store.go`
- Modify: `pkg/config/config.go` (add field), `cmd/root.go` (flag + `viper.GetString`), `pkg/server/server.go` (normalise in `NewServer`), `pkg/server/xtream_handlers_api.go` (`catalogFetcher`, `servePlayerAPICatalog`, `renderCatalog`, `categorySlice`), `pkg/server/catalog_cache.go` (only remove `renderOnce`/`rendered`/`renderErr` and `renderedJSON`), `pkg/server/compress.go` (remove `renderedGzip`, `renderedGzips`, `catalogGzip`), `README.md`, `docker-compose.yml`
- Test: `pkg/server/catalog_storage_test.go`, existing `player_api_catalog_test.go`

**Interfaces:**
- Consumes: `catalogStore`, `memoryCatalog`, `catalogStoreOf`, `inCategory` (Task 2).
- Produces:
  - `config.ProxyConfig.CatalogStorage string`
  - `const catalogStorageMemory = "memory"`, `catalogStorageDisk = "disk"`
  - `func parseCatalogStorage(s string) string` (trims, lower-cases; unknown or empty → `memory`, unknown also `utils.WarnLog`)
  - `func (c *Config) diskCatalogs() bool` (`c.CatalogStorage == catalogStorageDisk`)
  - `catalogStore` gains `serveWhole(ctx *gin.Context, c *Config, action string) error` and `serveSlice(ctx *gin.Context, c *Config, action, categoryID string) error`.
  - `memoryCatalog` gains the memo fields `renderOnce sync.Once; rendered []byte; renderErr error; gzOnce sync.Once; gz []byte; gzErr error`. A wrapper made by `catalogStoreOf` for a raw value has no lasting memo; that only happens for raw values stored by tests and small fallback values.
- Behaviour: `servePlayerAPICatalog` keeps its error branches, logging and the debug dump; after the fetch it calls `catalogStoreOf(e.data).serveWhole(...)` or `.serveSlice(...)`; a returned error → `ctx.AbortWithError(http.StatusInternalServerError, ...)` as today. `memoryCatalog.serveWhole` = today's memoised `renderCatalog(action, data, true)` + gzip memo + headers (`Vary`, `Content-Encoding`, `application/json; charset=utf-8`); `serveSlice` = today's per-request slice render + `gzipBytes`. `catalogFetcher` wraps every successful result in `&memoryCatalog{data: resp}` (the `catalogResultError` paths stay unwrapped).

- [ ] **Step 1: Write the failing tests**

`catalog_storage_test.go`:
- `TestParseCatalogStorage`: `""`→`memory`, `"memory"`→`memory`, `" Disk "`→`disk`, `"ssd"`→`memory`.
- `TestMemoryCatalogRendersOnce`: two `serveWhole` calls on one cached entry give equal bodies and the second does not call `renderCatalog` again (count harvest calls via the channel index, or compare the memoised slice's backing pointer).

- [ ] **Step 2: Run them**

Run: `$G test -mod=vendor ./pkg/server -run 'ParseCatalogStorage|MemoryCatalogRendersOnce'` → FAIL (undefined).

- [ ] **Step 3: Implement** the setting (flag help: `Where cached provider catalogues are kept: memory (default) or disk (lower RAM, uses CACHE_FOLDER)`), the serving methods, and move today's render/gzip memo into `memoryCatalog`. `serveSlice` uses `categorySlice` (which now uses `inCategory`). Update `TestPlayerAPICatchupFlagsDoNotMutateCachedCatalogue` (`player_api_catalog_test.go:258`) to read items through `catalogStoreOf(raw).each` — its assertions stay. README: add `CATALOG_STORAGE` to the environment table with the trade-off in two lines (memory: fastest, RAM grows with the catalogue; disk: far less RAM, ~2–3× the provider's catalogue size on disk, slightly slower searches). `docker-compose.yml`: commented `# CATALOG_STORAGE: disk` next to the other optional settings.

- [ ] **Step 4: Run tests**

New tests pass; full suite passes.

- [ ] **Step 5: Measure and commit**

Run `tools/memprobe/measure.sh step3`; report (expected: same as step 2 within noise).


```bash
git add cmd pkg README.md docker-compose.yml
git commit -m "refactor(catalog): serve cached catalogues through catalogStore; add CATALOG_STORAGE"
```

---

### Task 4: Xtream client fetches a catalogue straight to a file

**Files:**
- Create: `pkg/xtream/catalog_file.go`, `pkg/xtream/catalog_file_test.go`
- Modify: `pkg/xtream/xtream_proxy.go`

**Interfaces:**
- Produces:
  - `func (c *Client) ActionToFile(ctx context.Context, cfg *config.ProxyConfig, action string, q url.Values, path string) (httpcode int, err error)` — same request building, 5 attempts, `retryBackoff`, `bodyStallTimeout`, `maxResponseBytes` cap and status handling as `ActionContext`; writes `path+".part"` and renames to `path` on success; removes the `.part` on failure. Non-200 / transport failure → `http.StatusBadGateway` and the last error, as `ActionContext`.
  - `func FallbackForAction(action string) interface{}` — exported `fallbackForAction` (the unexported name stays as an alias or callers switch).
  - ```go
    type CatalogFileKind int
    const (
        CatalogFileArray CatalogFileKind = iota // non-empty JSON array; stays on disk
        CatalogFileSmall                        // decoded value in Small (object, [], {}, fallback)
    )
    func ClassifyCatalogFile(path, action string) (kind CatalogFileKind, small interface{}, err error)
    ```
    Mirrors `ActionContext`'s tail exactly: empty / `null` / leading `<` → `CatalogFileSmall`, `FallbackForAction(action)`, nil; `{}` → small `map[string]interface{}{}`; `[]` → small `[]interface{}{}`; leading `[` → stream-validate the whole array with `json.Decoder` + `UseNumber` (token, then `Decode` per element into `json.RawMessage`, then closing token, then EOF); valid with ≥ 1 element → `CatalogFileArray`, nil, nil; invalid JSON → `CatalogFileSmall`, `FallbackForAction(action)`, the decode error; any other leading byte (an object) → decode whole into `interface{}` with `UseNumber`, `CatalogFileSmall`. Memory use must not grow with array size.
- The retry loop is factored into one unexported function used by both `ActionContext` and `ActionToFile`; do not duplicate it.

- [ ] **Step 1: Write the failing tests**

`catalog_file_test.go`, table `TestClassifyCatalogFile` over file contents: `""`, `"null"`, `"<html>"`, `"{}"`, `"[]"`, `"[{\"a\":1},{\"a\":2}]"`, `"[{\"a\":1},"` (truncated), `"{\"1\":{\"a\":1}}"`, leading whitespace variants. Expected kinds/values per the rules above; for the truncated case `err != nil`.
`TestActionToFileWritesBody` (httptest server, 200 + JSON) → file equals body, no `.part` left. `TestActionToFileRetriesThenFails` (server always 503, `retryBackoff` shortened through the package's existing test hook if one exists, otherwise accept the ~real backoff) → `StatusBadGateway`, error, no file and no `.part`.

- [ ] **Step 2: Run them** → FAIL (undefined).

Run: `$G test -mod=vendor ./pkg/xtream -run 'ClassifyCatalogFile|ActionToFile'`

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run tests** → new tests pass; `./pkg/xtream` and full suite pass.

- [ ] **Step 5: Commit**

```bash
git add pkg/xtream
git commit -m "feat(xtream): fetch a catalogue straight to a file and classify it without decoding it whole"
```

---

### Task 5: `diskCatalog`: iterate, render to disk once, serve whole lists and slices

**Files:**
- Create: `pkg/server/catalog_disk.go`, `pkg/server/catalog_disk_test.go`
- Modify: `pkg/utils/cache.go` (add `catalogSubdir = "catalog"`, `func CatalogCacheDir() string`)

**Interfaces:**
- Consumes: `catalogStore` (with Task 3's serving methods), `memoryCatalog`, `keepCatalogEntry`, `channelHarvest`/`applyChannelHarvest`, `catchupFlagged`, `inCategory` (Task 2), `categoryNames`, `catalogActions`, `writeGzipSibling`, `serveFileCompressed`, `responseWriter` (`compress.go`).
- Produces:
  - `type diskCatalog struct { raw string; mu sync.Mutex; render *diskRender }` and `type diskRender struct { path string; spans map[string][]catalogSpan }`, `type catalogSpan struct { off int64; n int32 }`
  - `func newDiskCatalog(raw string) *diskCatalog`
  - `(d *diskCatalog) each(fn) error`: opens `raw`, `json.Decoder` + `UseNumber`, decodes one element at a time into `map[string]interface{}` (non-object elements skipped), stops when `fn` returns false.
  - `(d *diskCatalog) ensureRender(c *Config, action string) (*diskRender, error)`: under `mu`; returns the stored render if any; otherwise writes `raw+".render"` (temp + rename): `[`, then for each item in order: skip if the filter is active for the kind and `keepCatalogEntry` is false; for `get_live_streams` with catchup enabled replace by `catchupFlagged`; `json.Marshal` the item, write `,` between items, record its span and its category ids (every id `inCategory` would match: `category_id` plus `category_ids` entries, via `catalogString`); harvest every item *before* filtering for `get_live_streams` (as `renderCatalog(whole)` does); then `]`. Then `writeGzipSibling(path)`, `applyChannelHarvest`, and for live+catchup `SetUpstreamCatchup(upstream)`. On any error remove the partial files and return the error without storing (next call retries).
  - `(d *diskCatalog) serveWhole`: `ensureRender`, then `serveFileCompressed(ctx, r.path, "application/json; charset=utf-8")`.
  - `(d *diskCatalog) serveSlice`: `ensureRender`, then `responseWriter(ctx, "application/json; charset=utf-8")`, write `[`, the category's spans read with `ReadAt` joined by `,`, `]`. Unknown category → `[]`.
  - `(d *diskCatalog) remove()`: removes `raw`, `raw+".render"`, `raw+".render.gz"`.

- [ ] **Step 1: Write the failing tests** (`catalog_disk_test.go`; each test writes a provider body to a file under `t.TempDir()` and builds `newDiskCatalog(path)`; the reference is `&memoryCatalog{data: <same body decoded with UseNumber>}`):
- `TestDiskCatalogEachMatchesMemory`: same items in the same order, early stop honoured.
- `TestDiskCatalogWholeIsByteIdentical`: for `get_live_streams`, `get_vod_streams`, `get_series` × {filter off, filter excluding one category and one name} × {catchup off, on (live only)}: `serveWhole` body (plain, and gzip after decompression) equals `memoryCatalog.serveWhole` body; same `Content-Type`, `Vary`, `Content-Encoding`.
- `TestDiskCatalogSliceIsByteIdentical`: categories `"42"` (string id), `"43"` (numeric id `43`), `"7"` (only in `category_ids`), `"999"` (unknown → `[]`): equals `memoryCatalog.serveSlice`.
- `TestDiskCatalogRendersOnceConcurrently`: 20 goroutines call `serveWhole` on a fresh catalogue; every body equal; the render file's mtime unchanged after a second wave (exactly one render). Run under `-race`.
- `TestDiskCatalogRenderFailureIsRetried`: make the directory read-only (`os.Chmod(dir, 0o500)`; skip the test when running as root), `serveWhole` returns an error; restore `0o700`; next `serveWhole` succeeds.
- `TestDiskCatalogHarvestsNamesFromWholeList`: after `serveWhole` on live with a filter hiding one channel, the hidden channel's name is still known to the channel index.

- [ ] **Step 2: Run them** → FAIL (undefined).

Run: `$G test -mod=vendor -race ./pkg/server -run DiskCatalog`

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run tests** → new tests pass; full suite passes.

- [ ] **Step 5: Commit**

```bash
git add pkg/server pkg/utils
git commit -m "feat(catalog): disk-backed catalogue store with render-once files and span-based category slices"
```

---

### Task 6: Disk mode wiring: fetch to disk, directory, grace deletion, memory limit

**Files:**
- Modify: `pkg/server/catalog_disk.go`, `pkg/server/catalog_storage.go`, `pkg/server/xtream_handlers_api.go` (`catalogFetcher`), `pkg/server/server.go` (call `applyCatalogMemoryLimit` in `NewServer`)
- Test: `pkg/server/catalog_disk_test.go`, `pkg/server/catalog_storage_test.go`, `pkg/server/player_api_catalog_test.go`, `pkg/server/catalog_filter_test.go`, `pkg/server/vod_search_test.go`, `pkg/server/live_shares_test.go`, `pkg/server/watch_filter_test.go`, `pkg/server/catalog_filter_m3u_test.go`

**Interfaces:**
- Consumes: `ActionToFile`, `ClassifyCatalogFile`, `FallbackForAction` (Task 4); `newDiskCatalog`, `remove` (Task 5); `utils.CatalogCacheDir`.
- Produces:
  - `func catalogRunDir() (string, error)` — once per process (`sync.Once`): `os.MkdirAll(utils.CatalogCacheDir())`, remove sibling directories whose mtime is older than 24 h, then `os.MkdirTemp(utils.CatalogCacheDir(), "run-")`. Errors are returned to every caller (and retried on the next call when the first failed).
  - `var catalogFileGrace = 10 * time.Minute` (tests shorten it).
  - `func applyCatalogMemoryLimit(storage string)` — when `storage == catalogStorageDisk` and `os.Getenv("GOMEMLIMIT") == ""`: `debug.SetMemoryLimit(90 << 20)` and `utils.InfoLog("Catalogue storage: disk; GOMEMLIMIT set to 90 MiB (override with GOMEMLIMIT)")`.
  - `func forEachCatalogStorage(t *testing.T, run func(t *testing.T, storage string))` (test helper in `catalog_storage_test.go`): runs `run` as subtests `memory` and `disk`, each with `invalidateCatalogCache()` first.
- Behaviour of `catalogFetcher` in disk mode, only for the three stream lists and only when `q` carries no `category_id`: file name `<catalogKeyLabel(key)>-<unix nanos>.json` in `catalogRunDir()`; `ActionToFile`; a transport error or a run-dir error → `&catalogResultError{code: httpcode, err: ...}` exactly like today's `Action` error path; then `ClassifyCatalogFile`: `CatalogFileArray` → after success, if the entry currently cached under this key holds a `*diskCatalog`, schedule its `remove()` with `time.AfterFunc(catalogFileGrace, ...)` (with the shortcut comment from Global Constraints), and return `newDiskCatalog(path)`; `CatalogFileSmall` → delete the file and continue with today's logic on `small` (usable check, `catalogResultError` for empty/unusable, wrap in `&memoryCatalog{}` on success); classification error → delete file, `&catalogResultError{data: small, code: httpcode, err: ...}`.

- [ ] **Step 1: Write the failing tests**
- `TestApplyCatalogMemoryLimit`: with `t.Setenv("GOMEMLIMIT", "")` → after `applyCatalogMemoryLimit("disk")`, `debug.SetMemoryLimit(-1) == 90<<20`; restore the previous limit in `t.Cleanup`. `"memory"` leaves the limit unchanged. With `GOMEMLIMIT=200MiB` set, `"disk"` leaves the limit unchanged.
- `TestCatalogRunDirRemovesOnlyStaleSiblings`: pre-create `catalog/run-old` (mtime −25 h) and `catalog/run-new` (mtime −1 h) under a temp `CACHE_FOLDER`, reset the once (expose an unexported test reset), call `catalogRunDir()` → `run-old` gone, `run-new` kept, a new `run-*` exists.
- `TestDiskModeReplacedFilesRemovedAfterGrace`: disk mode, `catalogFileGrace = 50ms`, fetch, force a refresh (`fetchCatalogEntry(key, true, ...)`), old raw file exists right after, gone after 200 ms; the new one exists.
- `TestDiskModeObjectAnswerServedAsToday` (Review Focus 1): provider answers `get_vod_streams` with `{"1":{"name":"A"}}` → disk-mode body equals memory-mode body.
- `TestDiskModeUnwritableCacheKeepsLastGood` (Review Focus 4): disk mode, first fetch OK, then `chmod 0o500` the run dir and expire the entry (`CATALOG_CACHE_TTL_MINUTES` via `t.Setenv` plus the forced fetch) → the old body is still served; with no previous entry and an unwritable dir → `502` like a provider failure. Skip as root.
- Convert to `forEachCatalogStorage` the tests in the listed test files that fetch catalogues through `catalogFetcher` / `xtreamPlayerAPI` / the search and share handlers; set `c.CatalogStorage = storage` on their `Config`. Assertions unchanged. (`TestPlayerAPICategorySlicedFromFullCatalogue`'s "one cache entry only" assertion holds in both modes.)

- [ ] **Step 2: Run them** → the new tests FAIL (undefined / memory behaviour in disk subtests).

Run: `$G test -mod=vendor -race ./pkg/server -run 'CatalogMemoryLimit|CatalogRunDir|DiskMode|PlayerAPI|Filter|Search|Share|Watch'`

- [ ] **Step 3: Implement.**

- [ ] **Step 4: Run tests** → all pass; full suite passes.

- [ ] **Step 5: Measure both modes and commit**

Run `tools/memprobe/measure.sh final-memory` and `CATALOG_STORAGE=disk tools/memprobe/measure.sh final-disk`; report both outputs in full. Disk mode: `30s later` and `after forced refresh +30s` must be ≤ 100 MB VmRSS (spec Goal 1). If not, report the pprof top instead of tuning on your own.

```bash
git add pkg/server
git commit -m "feat(catalog): CATALOG_STORAGE=disk fetches stream lists to disk; grace deletion and memory limit"
```

---

## After the last task (controller)

- Whole-branch review (subagent-driven-development's final review).
- Update PR #63's title and description: what the PR now covers, the measurement table (baseline, step 1, step 3, final memory, final disk, Go 1.23), the new setting, test plan. Push.
