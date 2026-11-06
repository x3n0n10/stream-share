# Catalogue Disk Cache Design

## Intent

A StreamShare instance with a large provider catalogue uses far too much RAM.
The owner sees over 1 GB per instance in production. On a synthetic catalogue
of the owner's size (22k live, 122k VOD, 20k series) the current build
(PR #63, `fix/category-slice-from-full-catalogue`) reaches 718–793 MB RSS. The goal is **at most
100 MB RSS per instance** at that size, without changing what players,
Discord or the dashboard see.

The fix: keep the provider catalogues on disk under `CACHE_FOLDER` instead of
as parsed Go trees in memory. Only small, purpose-built indexes stay in RAM.

This is built in the fork (`x3n0n10/stream-share`). The upstream author
(lucasduport) wrote the catalogue cache (#42), the filter (#46), channel
numbers (#52), share links (#53) and the disk-cached EPG (#43). This design
must keep their behaviour intact. It changes where a cached catalogue lives,
not how the cache behaves.

## Where the memory goes today

Heap profile of the real binary after a player loaded every list, plus 20
category requests and `get.php` (live heap 296 MB, RSS 718 MB):

| Holder | MB | Source |
|---|---|---|
| Parsed catalogue trees (`catalogEntry.data`, `[]interface{}` of maps) | 178 | `catalog_cache.go` |
| Rendered player JSON (`catalogEntry.rendered`) | 53 | `xtream_handlers_api.go` `renderedJSON` |
| Gzip of rendered JSON (`renderedGzips`) | ~9 | `compress.go` `catalogGzip` |
| M3U stream index (`buildStreamIndex`) | 50 | `m3u_stream_index.go` |
| Per-request playlist copies (`get.php`, M3U routes) | transient, ~3–4× playlist size | `os.ReadFile` + `rewritePlaylistHosts` + `serveBytesCompressed` |

RSS is ~2.4× the live heap because no `GOMEMLIMIT` is set. A refresh also
briefly keeps the old and new parsed tree alive. Per-viewer stream buffers are
small (64–256 KB) and out of scope.

Measured with Go 1.27; the Docker image builds with Go 1.23 (`Dockerfile`). The
final measurement must use a Go 1.23 build.

## Goals

1. ≤ 100 MB RSS at 22k live / 122k VOD / 20k series as the settled level
   (owner's choice: not a hard container limit). Measured 30 s after a player
   has loaded every list, category slices and `get.php`, and again 30 s after
   a forced catalogue refresh. Short spikes above 100 MB during a refresh or
   a request are allowed as long as RSS comes back under it. Peaks are
   recorded in the PR but are not a pass/fail criterion.
2. Players get byte-for-byte the same `player_api.php` answers as today
   (whole lists and category slices), with the same headers (`Vary`,
   `Content-Encoding: gzip` when accepted, `application/json; charset=utf-8`).
3. Every behaviour of the #42 cache stays: one fetch per key for concurrent
   callers, stale-while-revalidate, last good copy on failure, failures and
   empty answers never cached, 1-minute retry backoff, the live refresher's
   forced refresh.
4. All existing tests keep passing. Tests may only change where they reach into
   internals that no longer exist (`catalogEntry.rendered`, `renderedGzips`);
   their assertions about behaviour stay. The cache-core tests in
   `catalog_cache_test.go` and `stream_numbers_test.go` stay unchanged (see 1).

## Non-goals

- Persisting catalogues across restarts. The catalogue directory is wiped at
  startup; behaviour after a restart is the same as today.
- Changing the uncached player_api path (`get_vod_info`, `get_series_info`,
  etc.), the EPG cache, VOD media caching, catchup buffers or multiplexing.
- Image URL rewriting on cached catalogues (today's cached path does not do it;
  keep it that way).
- Upstreaming. It should stay mergeable with future upstream changes, but
  proposing it upstream is a later, separate decision.

## Anti-goals

- Any player-visible change in content or ordering.
- A refresh that builds the whole catalogue in memory (that peak alone breaks
  the budget).
- Losing the last good catalogue because a file write failed or the disk is
  full: a failed write is a failed fetch and the previous entry keeps serving.
- New dependencies.

## Design

### 1. Catalogue entries hold files, not trees

The cache core stays generic: `catalogEntry.data` remains `interface{}` and
`fetchCatalogEntry`, `runCatalogFetch`, `startCatalogRefresh`,
`leadCatalogFetch`, `storeCachedCatalog`, the in-flight map and the backoff
are unchanged, so the cache-core tests (which store arbitrary values such as
`"data"` or `[]interface{}{"a","b"}`) keep passing as they are.

What changes is what the catalogue fetcher puts in `data`:

- for a JSON-array answer, a `*diskCatalog` (new type): path of the provider's
  body as received, under `utils.CatalogCacheDir()` (new, a per-process
  subdirectory of `CACHE_FOLDER/catalog`, see below), plus a lazily built
  render (see 3);
- for anything else (object, empty/fallback values), the small decoded value
  exactly as today, in memory.

`catalogEntry` loses `renderOnce`/`rendered`/`renderErr`; the render lives on
`*diskCatalog`.

**Directory.** Several instances may share a host and, when `CACHE_FOLDER` is
unset, the same temp root. Each process therefore writes into its own
subdirectory (`os.MkdirTemp` under `CACHE_FOLDER/catalog`). At startup a
process removes only sibling subdirectories whose mtime is older than
24 hours (a running instance writes new files at least every TTL), and it
removes its own subdirectory on clean shutdown when a shutdown hook exists.

File names are unique per entry (cache label + fetch time/counter). When an
entry is replaced, the old entry's files are removed after a grace period
(10 minutes), so goroutines still holding the old entry keep working. Add the
comment `shortcut: old files are kept for a fixed 10 min grace, not
refcounted; switch to refcounting if a reader can hold an entry longer`.

### 2. Fetching streams to disk

Add a streaming variant to the Xtream client (`pkg/xtream/xtream_proxy.go`)
that writes the body to a file instead of returning it, sharing the existing
retry loop, stall timeout, `maxResponseBytes` cap, User-Agent and status
handling (factor the loop; do not copy it). It writes to a temp file and
renames on success.

`catalogFetcher` then validates the file without decoding it whole: the same
rules `ActionContext` and `usableCatalog` apply today (empty, `null`, HTML
`<…`, `{}`, `[]`, emergency category data, invalid JSON) must give the same
outcome, using the same `catalogResultError` values. A whole-catalogue array
must contain at least one element. Invalid JSON is detected by a streaming
pass (`json.Decoder`, `UseNumber`) over the file.

Peak memory of a fetch must not depend on catalogue size.

### 3. Rendering once, to disk

`renderCatalog`'s semantics stay: `ProcessResponse` (a no-op for decoded
maps; keep the call for non-array answers), channel-name harvest only for the
whole live list, catalogue filter, catchup flags (copying, and only the whole
list sets the upstream-catchup map).

It becomes a streaming pass over the raw file, once per entry, on first player
request (as `renderOnce` does today):

- Decode one item at a time (`json.Decoder`, `UseNumber`).
- Apply the filter and catchup flags per item. The filter needs a
  per-item form (`filterEntries` is already per item; split it so a single map
  can be judged with a category-name map fetched once per pass).
- Encode each kept item and write it to the rendered file and, through a
  `gzip.Writer`, to the gzip file in the same pass.
- Record for each kept item its byte span in the rendered file and its
  category ids (`category_id` and `category_ids`, the matching rules of
  `categorySlice` from PR #63).
- Collect harvest data (names, EPG ids, categories) per item and call
  `updateAPIChannelIndex` at the end, as `harvestChannelNames` does today.

The output must be byte-identical to what `json.Marshal` of the processed
slice produces today. Test this against the current renderer. For gzip,
"identical" means the decompressed body.

**A failed render is not memoised.** If writing the rendered or gzip file
fails (disk full, I/O error), remove the partial files, answer that request
with a 500 as today's render error does, and let the next request try again.
Only a successful render is kept for the entry's life.

Serving a whole list: stream the rendered or gzip file to the response
(`io.Copy` or `http.ServeContent`), same status and headers as today.

Serving a category slice: `[` + kept spans of that category joined by `,` +
`]`, read with `ReadAt` from the rendered file, gzipped per request when
accepted (small). Unknown category returns `[]`. This replaces the per-request
re-render from PR #63 and gives the same answer.

### 4. Consumers read through an iterator

Add `func (e *catalogEntry) each(fn func(m map[string]interface{}) bool) error`
that streams the raw file item by item (stop when `fn` returns false). Each
consumer keeps its loop body and replaces the `[]interface{}` range with
`each`:

| Consumer | File | Notes |
|---|---|---|
| `keptLiveIDs` | `catalog_filter.go` | already memoised per entry |
| `filterCategories` (used category ids) | `catalog_filter.go` | memoise per stream-list entry |
| `categoryNames` / `categoryNamesFrom` | `catalog_filter.go` | category lists are small; read via `each` |
| `liveChannel` | `live_shares.go` | linear scan, stops at match |
| `shareChannelsAPI` | `live_shares.go` | scan with per-item filter |
| `liveChannelNumber` / `buildLiveNumIndex` | `stream_numbers.go` | already memoised per entry |
| `warmChannelNameIndex`, `refreshAPIChannelIndex` | `xtream_handlers_api.go`, `server.go` | harvest via `each` |
| `searchXtreamVOD`, `searchXtreamSeries` | `vod_search.go` | scan with per-item filter; no in-RAM name index |

`peekCatalog` / `fetchCatalogOnce` / `getCached*Catalog` change to return the
entry (or an iterator) instead of `interface{}`. The fallback value path of
`fetchCatalogOnce` (empty but not an error) keeps returning that small value.

Cost: a scan of the 36 MB VOD file per Discord search, estimated at a few
hundred ms. Acceptable; searches are rare and already take longer for series
(episode crawl).

### 5. M3U stream index

`buildStreamIndex` stores `Title` and `Ext` as substrings of the scanned line,
which keeps every whole M3U line alive (~30 MB). Clone the title
(`strings.Clone`) and intern the extension (a handful of distinct values).
No behaviour change. If the budget is still missed after all steps, the next
measure is replacing the string key with a compact one; decide that from the
measurement, not upfront.

### 6. Playlist routes stream instead of copying

`xtreamGet` (`xtream_handlers_api.go`), the M3U proxy route
(`proxy_handlers.go`) and `xtream_handlers_stream.go` read the whole cached
playlist with `os.ReadFile`, rewrite it with `strings.Split` and compress it
in memory. At this catalogue size that is tens of MB per request, more with
concurrent players. Change `rewritePlaylistHosts` to a line-by-line rewrite
from an `io.Reader` to an `io.Writer` (same per-line rules), and serve through
a streaming gzip writer when the client accepts it, keeping the same
`Content-Type` and `Content-Disposition`. The response may switch to chunked
transfer (no `Content-Length`); players already accept that for the
uncompressed path of other routes.

### 7. `GOMEMLIMIT`

Set `ENV GOMEMLIMIT=90MiB` in the `Dockerfile` runtime stage (and in
`.Dockerfile.goreleaser`). Users can override it. Document it in the README's
environment section.

## Constraints

- Go 1.23 (`go.mod`, Docker builder); vendored dependencies (`-mod=vendor`).
- Match house style: `interface{}`, GPLv3 header on new files, comments explain
  why.
- Logs never carry the provider URL or credentials (`catalogKeyLabel`).
  File names must not contain them either: derive from the key label plus a
  counter or hash.
- `CACHE_FOLDER` may be unset: `utils.CacheRoot()` already falls back to a temp
  directory.
- Disk cost at the owner's size: ~120–150 MB in `CACHE_FOLDER/catalog`.

## Verification

- `go build`, `go vet`, `go test -race ./...` pass.
- Byte-identity test: rendered whole lists and category slices from the new
  path equal the old renderer's output, with filter and catchup on and off.
- Fetch-validation tests for each provider oddity listed in 2.
- A replaced entry's files are removed after the grace period; a failed write
  keeps the previous entry serving.
- Memory: the throwaway harness used for the numbers above (fake provider
  with the owner's sizes, real binary, real Postgres, RSS from
  `/proc/<pid>/status`, heap profile via a temporary pprof import) is rerun
  after each step with a Go 1.23 build. The result table goes in the PR.

## Order of work

Each step is measured before the next:

0. Check the measurement harness into the repo under `tools/memprobe/`
   (fake provider with configurable sizes, plus a script that starts the
   binary against a local Postgres, replays the request sequence below and
   prints RSS and the heap profile). Record the baseline with a Go 1.23 build.
   Request sequence: login, the three category lists, the three whole lists
   with `Accept-Encoding: gzip`, category slices 1–10 for live and VOD,
   `get.php`; RSS sampled after each phase and 30 s later.
1. M3U index clone/intern (5), playlist streaming (6) and `GOMEMLIMIT` (7):
   small, independent.
2. Streaming fetch to disk and the entry/iterator change (1, 2, 4).
3. Render-to-disk and span-based slices (3).

## Calls made without the owner

- Built on PR #63 itself: same branch (`fix/category-slice-from-full-catalogue`)
  and same PR, by the owner's choice. The PR title and description are
  updated when this work is pushed.
- No in-RAM search index; searches scan the file.
- Catalogue files are not reused after a restart.
- `GOMEMLIMIT` default 90 MiB.
- 10-minute grace before old catalogue files are deleted.
- The cache core stays generic (`data interface{}`); disk storage sits in the
  catalogue fetcher, so upstream's cache code and tests stay untouched.
- One catalogue subdirectory per process; startup only removes siblings
  older than 24 h.
- A failed render is retried on the next request, not memoised.
- Playlist routes (`get.php` and the two M3U routes) stream; this was not in
  the first outline but is needed for the budget.
- The measurement harness is checked in under `tools/memprobe/`.
