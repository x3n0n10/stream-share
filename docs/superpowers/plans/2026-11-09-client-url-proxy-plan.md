# Client URLs Through StreamShare (A10) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every URL a player receives from player_api points at StreamShare (spec A10), for uncached actions and cached catalogue lists, in both storage modes.

**Architecture:** One pure function turns a decoded player_api value into a client-safe copy (absolute http(s) strings → signed `/img` links, `direct_source` removed). The uncached path calls it once per answer; the catalogue render calls it per item, and rendered output is kept per client-facing base URL (max 4) in both `memoryCatalog` and `diskCatalog`.

**Tech Stack:** Go 1.23, Gin, stdlib.

**Spec:** `docs/superpowers/specs/2026-11-08-security-and-readability-design.md` (section A10; A1 for signing)

## Global Constraints

- Branch `fix/security-hardening` (PR #64, owner approved including A10 here). Commit per task; do not push (the controller pushes).
- Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`; tests need `CACHE_FOLDER=$(mktemp -d)`.
- Suite: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean; `gofmt -l pkg cmd tools` empty. CI also runs golangci-lint (errcheck, staticcheck).
- No new dependencies or settings. House style: `interface{}`, GPLv3 header (copy from `pkg/server/helpers.go`) on new files, comments say why.
- Ponytail: smallest complete change, reuse existing helpers (`proxyImagePath`, `requestBaseURL`, `writeGzipSibling`, `serveFileCompressed`, `responseWriter`), no unrequested abstraction.
- Never mutate catalogue data held in the cache (shared by concurrent requests).
- Player-visible bodies: memory and disk mode byte-identical for the same base URL.
- Disk-mode memory profile must not regress.

## Review Focus

1. Values that look like URLs but are not addresses to fetch: `"http://"` alone, `"https:// broken"`, `"ftp://x"`, a URL with spaces → unchanged. Test in Task 1.
2. A value that is already a StreamShare `/img` link (same base) is not wrapped twice. Test in Task 1.
3. Huge lists: the per-item rewrite allocates per item only (no whole-list copies beyond what the render already builds). Checked by the memprobe run in Task 2.
4. Concurrent first requests from two base URLs on a fresh entry: each base renders once, no race, no mixed links. Test in Task 2.
5. Category slices use the same base-specific render as the whole list. Test in Task 2.

---

### Task 1: The client-safe rewrite, used on the uncached path

**Files:**
- Create: `pkg/server/client_urls.go`, `pkg/server/client_urls_test.go`
- Modify: `pkg/server/image_proxy.go` (delete `rewriteImageFields`), `pkg/server/xtream_handlers_api.go:~319` (uncached path), tests that used `rewriteImageFields` (`TestRewriteImageFields` in image_proxy_test.go moves to the new function)

**Interfaces:**
- Produces: `func (c *Config) clientSafeValue(base string, v interface{}) interface{}` — returns a copy of `v`: maps and slices are copied (input never mutated), every map key `direct_source` is dropped, and every string `s` for which `isFetchableURL(s)` is true becomes `base + c.proxyImagePath(s)`; strings already starting with `base + "/" + <custom endpoint> + "img?"` (i.e. this server's own `/img` links) are kept as they are. Other values are returned unchanged.
- Produces: `func isFetchableURL(s string) bool` — no whitespace, `url.Parse` succeeds, scheme `http` or `https`, non-empty `Host`.
- The uncached path replaces `c.rewriteImageFields(ctx, processedResp)` with `c.clientSafeValue(c.requestBaseURL(ctx), processedResp)`.

- [ ] **Step 1: Write the failing tests** (`client_urls_test.go`):
  - `TestClientSafeValueRewritesEveryURLField`: input modelled on the owner's data — `{"stream_icon": "http://lg.maxnow.cx:8080/logo/ucl.png", "cover": "https://image.tmdb.org/t/p/w600/a.jpg", "backdrop_path": ["https://image.tmdb.org/t/p/b.jpg"], "info": {"cover_big": "http://lg.maxnow.cx:8080/c.jpg", "kinopoisk_url": "https://www.kinopoisk.ru/film/1"}, "seasons": [{"cover_big": "https://image.tmdb.org/s.jpg"}], "direct_source": "http://prov/x.ts", "info2": {"direct_source": "http://prov/y.ts"}, "plot": "See https://example.com for more", "num": json.Number("3")}` with base `https://ss.example`: every URL value equals `"https://ss.example" + c.proxyImagePath(original)`; `plot` and `num` unchanged; no `direct_source` at any depth.
  - `TestClientSafeValueDoesNotMutateInput`: deep-compare the input (`reflect.DeepEqual` against a copy made before) after the call.
  - `TestIsFetchableURL` (Review Focus 1): true for `http://a.b/c.png`, `https://a.b:8080/x?y=1`; false for `http://`, `https:// broken`, `ftp://x/y`, `http://a b/c`, `/relative.png`, `""`.
  - `TestClientSafeValueKeepsOwnImgLinks` (Review Focus 2): a value equal to `base + c.proxyImagePath("http://x/y.png")` is unchanged.
  - Port `TestRewriteImageFields`' cases to `clientSafeValue` (keep their assertions where they still apply).
- [ ] **Step 2: Run** `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor ./pkg/server -run 'ClientSafe|IsFetchableURL'` → FAIL (undefined).
- [ ] **Step 3: Implement**; delete `rewriteImageFields`; switch the uncached path.
- [ ] **Step 4: Run** focused tests → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): rewrite every URL in player_api answers through /img`

---

### Task 2: Catalogue renders apply the rewrite, per base URL

**Files:**
- Modify: `pkg/server/xtream_handlers_api.go` (`renderCatalog`), `pkg/server/catalog_store.go` (`memoryCatalog`, `catalogStore` serve methods), `pkg/server/catalog_disk.go` (`diskCatalog` render, files, `removeCatalogFiles`), `README.md` (`PUBLIC_BASE_URL` recommendation)
- Test: `pkg/server/catalog_disk_test.go`, `pkg/server/catalog_storage_test.go` or `player_api_catalog_test.go`

**Interfaces:**
- Consumes: `clientSafeValue`, `isFetchableURL` (Task 1).
- Produces:
  - `const maxCatalogRenders = 4`.
  - `renderCatalog(action string, data interface{}, whole bool, base string) ([]byte, error)` — applies `clientSafeValue(base, item)` to every item after filtering and catchup flags (and before marshalling).
  - `memoryCatalog` keeps its rendered body and gzip per base URL (a small slice or map plus insertion order, guarded by a mutex; render under a per-base once so concurrent requests for one base render once); a fifth base drops the oldest.
  - `diskCatalog` keeps a `*diskRender` per base URL: render file `raw + ".render-" + <first 8 hex of sha256(base)>` (+ `.gz` sibling); evicting a base removes its two files; `removeCatalogFiles(raw)` also removes every `raw + ".render-*"` file (`filepath.Glob`).
  - Both stores' `serveWhole`/`serveSlice` take the base from `c.requestBaseURL(ctx)`.
- README: under `PUBLIC_BASE_URL`, one or two lines: set it behind a reverse proxy so every client gets the same links and catalogues are rendered once.

- [ ] **Step 1: Write the failing tests**:
  - `TestCatalogListsRewriteURLs` (both storage modes via `forEachCatalogStorage`): fake provider list with `stream_icon`, `cover`, `backdrop_path`, `direct_source`; whole list and one category slice via `callPlayerAPI` with `Host: ss.example` → every URL value starts with `http://ss.example` + the `/img?url=` path and its `sig` verifies; no `direct_source` (Review Focus 5).
  - `TestCatalogRenderPerBaseURL` (both modes): two requests with `Host: a.example` and `Host: b.example` → each body's links use its own host; the second request for `a.example` reuses the first render (disk: same render file, unchanged mtime; memory: count renders through a test hook or the channel-name harvest counter).
  - `TestCatalogRenderEvictsOldestBase` (both modes): five hosts → the first host's render is gone (disk: its two files removed) and a sixth request for it renders again correctly.
  - `TestCatalogRenderConcurrentBases` (disk mode, `-race`): 10 goroutines each for two hosts on a fresh entry → every response's links match its host; one render per host (Review Focus 4).
  - Extend the byte-identity tests (`TestDiskCatalogWholeIsByteIdentical`, `TestDiskCatalogSliceIsByteIdentical`) to pass a base URL: memory and disk bodies are equal per base.
- [ ] **Step 2: Run** `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./pkg/server -run 'CatalogListsRewriteURLs|CatalogRender|DiskCatalog'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** focused tests → PASS; full suite → all `ok`.
- [ ] **Step 5: Measure and commit.** Run `CATALOG_STORAGE=disk DB_HOST=/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad DB_PORT=5433 DB_NAME=streamshare DB_USER=postgres tools/memprobe/measure.sh a10-disk` (start Postgres first if `pg_isready` fails) and report the five RSS lines (baseline before A10: 90 MB settled, 83 MB after refresh). Commit — `fix(security): catalogue lists hand out only StreamShare URLs, rendered per base URL`

---

## After the last task (controller)

- Whole-branch review of the A10 range; re-run the live leak scan against the owner's dev image once the owner deploys it (no non-StreamShare URL in any player_api answer).
- Update PR #64's description; push.
