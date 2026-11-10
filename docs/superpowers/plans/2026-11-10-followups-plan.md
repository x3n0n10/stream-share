# Follow-ups (PR C) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the follow-ups the PR A/B reviews left out of scope (spec section "PR C", C1–C6): one VOD cache-hit rule, matching download keys, session/server loose ends, routes under `CUSTOM_ENDPOINT` plus query-style catchup, a single retry path in `fetchToFile`, and `vod_cache.go` split by job.

**Architecture:** Small fixes in `pkg/server` with tests first, then two readability refactors (behaviour unchanged), then a pure file split last so earlier diffs stay readable.

**Tech Stack:** Go 1.23, Gin.

**Spec:** `docs/superpowers/specs/2026-11-08-security-and-readability-design.md` (section "PR C — follow-ups")

## Global Constraints

- Branch `fix/followups` from PR B's head `cdb5dec` (PR B not yet merged). Commit per task; do not push (the controller pushes).
- Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`; tests need `CACHE_FOLDER=$(mktemp -d)`.
- Suite: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean; `gofmt -l pkg cmd tools` empty. CI runs golangci-lint (errcheck, staticcheck).
- No new dependencies, settings, types or interfaces. House style: `interface{}`, GPLv3 header (copy from `pkg/server/helpers.go`) on new files, comments say why.
- Ponytail: smallest complete change; reuse existing helpers; split by job; moves are verbatim.
- Logs never contain credentials (central redaction exists; still never format credentials deliberately).
- Disk-mode memory profile must not regress.

## Review Focus

1. A cached VOD whose file was deleted from disk: every entry point (player stream, multiplexed route, download link, Discord "start cache") falls back instead of erroring. Test in Task 1.
2. A Discord request with an extension (`123.mkv`) and a player request for `123` share one download and one cache row. Test in Task 2.
3. Xtream players without a path prefix keep working when `CUSTOM_ENDPOINT` is set (root routes stay). Test in Task 4.
4. Query-style catchup with wrong credentials is refused (401), and with a missing parameter answers 400 without contacting the provider. Test in Task 4.
5. `fetchToFile` retry behaviour (attempt count, backoff, log lines) is unchanged after the restructure. Existing VOD tests plus one retry-count test in Task 5.

---

### Task 1: One VOD cache-hit rule (C1)

> Note: the implementation used `cachedVODEntry` (returns the cache entry), by ruling.

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (`streamVODWithCache`, `serveCachedVOD`), `pkg/server/handlers_vod.go` (`handleTemporaryLink`, `startCache`), a helper next to `touchVODCache` in `pkg/server/vod_cache.go`
- Test: `pkg/server/handlers_vod_test.go` (or the existing VOD test file)

**Interfaces:**
- Produces: `func (c *Config) cachedVODFile(idRaw string) (string, bool)` — `c.db.GetVODCache(idRaw)`; returns the entry's `FilePath` and true when it is a regular file on disk; when the row exists but the file is missing or a directory, logs `utils.WarnLog("VOD cache: %s missing on disk at %s; not serving from cache", c.vodLabel(idRaw), path)` and returns false. No status check (rationale in spec C1).
- The four call sites use it and keep their own logging, `touchVODCache`, `registerVODView` and serving calls as today.

- [ ] **Step 1: Write the failing tests**: `TestTemporaryLinkFallsBackWhenCachedFileMissing` — a ready cache row pointing at a deleted file; `/download/:token` must not answer an error from the cache branch (it starts the download/fallback path: assert the fake provider was contacted or the download path was taken). `TestCachedVODFile` — row + existing file → path,true; row + missing file → "",false and a warning logged; directory → false; no row → false.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement** and switch the four call sites.
- [ ] **Step 4: Verify** — focused tests, full suite, vet, gofmt.
- [ ] **Step 5: Commit** — `fix(vod): one cache-hit rule; a missing cached file falls back instead of failing`

---

### Task 2: Download key matches the player key (C2)

**Files:**
- Modify: `pkg/server/handlers_vod.go` (`startCache`)
- Test: `pkg/server/handlers_vod_test.go`

**Interfaces:**
- In `startCache`, `idOnly := strings.TrimSuffix(req.StreamID, path.Ext(req.StreamID))` becomes the key for `GetVODCache`, `UpsertVODCache` (`StreamID`), `SetVODCachePath`, `SetVODCacheTitle`, `findVODTitleInCache` and `startBackgroundDownload`'s stream id. The requested extension still decides the file name (via `resolveVODTarget`). The response JSON keeps `stream_id` as requested (callers display it).

- [ ] **Step 1: Write the failing test** `TestStartCacheKeysByBareID`: `startCache` with `stream_id: "123.mkv"` → the cache row is stored under `"123"` and `inProgressDownloads` holds `"123"`; a subsequent `cachedVODFile("123")` / player lookup sees it.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** (focused, full suite, vet, gofmt).
- [ ] **Step 5: Commit** — `fix(vod): key Discord downloads by the bare stream id players use`

---

### Task 3: Session and server loose ends (C3)

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (`joinStreamSession`, `multiplexedStream`), `pkg/server/server.go` (`Serve`)
- Test: `pkg/server/xtream_handlers_stream_test.go`

**Interfaces:**
- `joinStreamSession`: on `GetClientChannel` failure call `c.sessionManager.RemoveClient(streamID, clientKey)` before answering 500.
- `multiplexedStream`: delete the `c.sessionManager == nil` fallback branch.
- `Serve`: each `defer close(stop)` immediately follows its worker's start call.

- [ ] **Step 1: Write the failing test** `TestJoinStreamSessionRemovesClientOnChannelFailure` if `GetClientChannel` failure can be provoked through existing seams (e.g. removing the client between the two calls via a test hook); if no seam exists without new API, document that in the report and rely on reading + the suite (do not add production hooks).
- [ ] **Step 2: Implement** the three changes.
- [ ] **Step 3: Verify** — full suite, vet, gofmt.
- [ ] **Step 4: Commit** — `fix(stream): release the session client when its channel is missing; tidy Serve shutdown order`

---

### Task 4: Routes under `CUSTOM_ENDPOINT`; query-style catchup (C4)

**Files:**
- Modify: `pkg/server/routes.go` (`addProxyCredentialRoutes`), `pkg/server/server.go` / `newRouter` (call site), `pkg/server/xtream_handlers_stream.go` (timeshift handler reuse)
- Test: `pkg/server/routes_test.go`

**Interfaces:**
- `addProxyCredentialRoutes(r gin.IRoutes)` (or keep `*gin.Engine` plus a group parameter — builder's call, no new type) registers the same five routes on the root and, when `c.CustomEndpoint != ""`, on the custom endpoint group as well.
- New route `GET /streaming/timeshift.php` on the root and the custom group: reads `username`, `password`, `stream`, `start`, `duration` from the query; missing any → 400; `!c.credentialsValid(username, password)` → 401; otherwise registers the session like `authWithPathCredentials` does (reuse it by setting the path params, or extract the shared part — smallest correct choice) and serves through `xtreamStreamTimeshift` with `id = stream` (append `.ts` only if `xtreamStreamTimeshift` needs an extension — check how `/timeshift/...` ids look today).
- README: one line under the catchup/M3U section that query-style catchup templates are served.

- [ ] **Step 1: Write the failing tests**: `TestViewerRoutesUnderCustomEndpoint` — with `CustomEndpoint = "providerA"`, `/providerA/live/<u>/<p>/42.ts` and `/providerA/timeshift/<u>/<p>/60/<start>/42.ts` reach the fake provider; the root forms still work (Review Focus 3). `TestQueryStyleTimeshift` — `/streaming/timeshift.php?username=<u>&password=<p>&stream=42&start=<start>&duration=60` reaches the provider's timeshift path; wrong password → 401; missing `start` → 400 and no provider request (Review Focus 4); the custom-endpoint form works too.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** (focused, full suite, vet, gofmt).
- [ ] **Step 5: Commit** — `fix(routes): viewer stream routes and query-style catchup under the custom endpoint`

---

### Task 5: One retry path in `fetchToFile`; leaner `copyVODResponse` (C5)

**Files:**
- Modify: `pkg/server/vod_cache.go` (`fetchToFile`, `copyVODResponse`)
- Test: existing VOD tests; add `TestFetchToFileRetryCount` if none covers the attempt count

**Interfaces:**
- In the status switch each branch only logs its reason and signals "retry"; one place after the switch performs `cancelAttempt(); _ = resp.Body.Close(); attempt++; retrying = true; continue`. Log text, order and backoff unchanged.
- `copyVODResponse(dl, f, resp, cancelAttempt, watchDone, streamID, downloaded int64, total int64) (newDownloaded int64, ok bool, readErr error)` — `lastUpdate` is a local; the caller assigns the returned count.

- [ ] **Step 1: Pin behaviour**: ensure a test covers retries (e.g. a fake provider failing twice with 503 then serving → exactly 3 attempts, file complete). Add it if missing; it must pass before and after.
- [ ] **Step 2: Refactor.** **Step 3: Verify** (full suite `-race`, vet, gofmt).
- [ ] **Step 4: Commit** — `refactor(vod): one retry path in the download loop; copyVODResponse returns its byte count`

---

### Task 6: `vod_cache.go` split by job (C6)

> Note: the implementation used `vod_window.go` (a `_windows.go` suffix is a GOOS build constraint), by ruling.

**Files:**
- Create: `pkg/server/vod_pacer.go`, `pkg/server/vod_windows.go`
- Modify: `pkg/server/vod_cache.go`

**Interfaces:**
- Pure moves per spec C6. GPL header on new files.

- [ ] **Step 1: Move** verbatim; confirm with `git diff --color-moved=plain --color-moved-ws=ignore-all-space` (after `git add -N` for new files).
- [ ] **Step 2: Verify** — full suite, vet, gofmt; memprobe disk run (`CATALOG_STORAGE=disk DB_HOST=/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad DB_PORT=5433 DB_NAME=streamshare DB_USER=postgres tools/memprobe/measure.sh prc-disk`, start Postgres if needed) — report the five RSS lines.
- [ ] **Step 3: Commit** — `refactor(vod): connection pacer and tail/window fetching in their own files`

---

## After the last task (controller)

- Whole-branch review; push; open the PR (against master once PR B is merged, otherwise based on `refactor/readability`); owner merges.
