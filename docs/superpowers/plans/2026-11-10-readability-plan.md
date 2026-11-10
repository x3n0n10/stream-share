# Readability and Cleanup (PR B) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the code read as "obviously the right way" (spec section "PR B", items B1–B7): no duplicate handlers or resolvers, files split by responsibility, long functions split by job, no dead code — with no player-visible behaviour change except B2 (one VOD extension rule) and B6 (a log line).

**Architecture:** Moves and deletions inside the existing packages; no new types, layers or interfaces. Order: small deletions first, then the two de-duplications, then file moves, then function splits (so splits happen in the files the code finally lives in).

**Tech Stack:** Go 1.23, Gin.

**Spec:** `docs/superpowers/specs/2026-11-08-security-and-readability-design.md` (section "PR B — readability and cleanup")

## Global Constraints

- Branch `refactor/readability` from master `f05e89d` (PR A merged). Commit per task; do not push (the controller pushes and opens the PR).
- Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`; tests need `CACHE_FOLDER=$(mktemp -d)`.
- Suite: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean; `gofmt -l pkg cmd tools` empty. CI runs golangci-lint (errcheck, staticcheck).
- Moves are moves: code is moved verbatim (only package-internal references adjusted); error handling, logging and comments move with it. Existing tests pass unchanged except tests of deleted dead code (B7) and B2's new test.
- No new dependencies, settings, types, interfaces or layers. House style: `interface{}`, GPLv3 header (copy from `pkg/server/helpers.go`) on new files, comments say why.
- Ponytail: smallest complete change; split by job, never by line count; never create helpers that exist only to make a function shorter.
- Disk-mode memory profile must not regress.

## Review Focus

1. A moved function that loses a comment, a log line or an error branch on the way. Every task: the reviewer diffs moved bodies (`git diff --color-moved=zebra` / `--color-moved-ws=ignore-all-space` style check) and the implementer reports "moved verbatim" per function.
2. Init order in `NewServer`: subsystems that depend on each other (database → session manager → catchup → slate → Discord → health) keep their order after the split. Test: existing server tests pass unchanged (Task 6).
3. A player request and a Discord download for the same movie, with `VOD_EXT_PROBE_ENABLED` on, share one cache file. Test in Task 3.
4. Viewer-credential stream routes keep their behaviour after the duplicate handlers are removed (filtered-live rejection, timeshift). Test in Task 2.
5. Dead-code removal never removes something reached only through reflection, routes or tests' intent (e.g. a route handler). The deadcode run is the source; anything it lists that is referenced by a route stays.

---

## File Structure (end state)

- `pkg/server/server.go`: `NewServer` (as a list of setup steps), its per-subsystem setup functions, `Serve` (router + worker start/stop), background refreshers it starts.
- `pkg/server/auth.go` / `pkg/server/routes.go`: `addProxyCredentialRoutes` (routes.go), `authWithPathCredentials`, `resolveRequestUsername` (auth.go).
- `pkg/server/handlers_vod.go`: `handleTemporaryLink`.
- `pkg/server/xtream_handlers_stream.go`: `multiplexedStream`, `classifyStreamType`.
- `pkg/server/m3u_playlist.go` (new): `playlistInitialization`, `marshallInto`, `replacePath`, `sanitiseFilename`.
- `pkg/server/request_log.go` (new): `quietRequestLogger`, `redactRequestURI`.
- `pkg/session/temp_links.go` (new), `pkg/session/cache_maintenance.go` (new): split out of `manager.go`.

---

### Task 1: Dead code and the startup log (B7, B6)

**Files:**
- Modify: `pkg/database/manager.go` (`DBManager.Close`), `pkg/database/temporary_links.go` (`IsUniqueViolation`), `pkg/filter/filter.go` (`Kind.String`, `Set.ExcludesAdult`), `pkg/server/vod_cache.go` (`connPacer.setSpacing`), `pkg/server/xmltv_icons.go` (`rewriteXMLTVIcons`), `pkg/utils/logging.go` (`Close`), their tests, `pkg/server/server.go` (`Serve` startup log)

**Interfaces:**
- Deletes exactly the functions deadcode lists today: `DBManager.Close`, `database.IsUniqueViolation`, `filter.Kind.String`, `filter.Set.ExcludesAdult`, `connPacer.setSpacing`, `Config.rewriteXMLTVIcons`, `utils.Close`. Tests that called them switch to the real API (e.g. `rewriteXMLTVIconsTo` into a `strings.Builder`) or drop only the assertion about the deleted function.
- B6: the `Serve` line logging hard-coded `session=1m, stream=1m, tempLink=1h` is deleted (the session manager exposes no getters; no new API for a log line).

- [ ] **Step 1: Re-run deadcode** — `PATH=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin:$PATH GOTOOLCHAIN=local GOFLAGS=-mod=vendor /tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad/bin123/deadcode . | grep -v '^tools/'` → expect exactly the seven functions above. If it lists more, delete those too only when they have no route/reflection use; report each.
- [ ] **Step 2: Delete** the functions and the log line; adjust tests.
- [ ] **Step 3: Verify** — deadcode prints nothing for pkg/; full suite, vet, gofmt clean.
- [ ] **Step 4: Commit** — `refactor: remove dead code and a startup log line with made-up values`

---

### Task 2: One stream handler per route kind (B1)

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (delete `xtreamProxyCredentials{Stream,LiveStream,Movie,Series}Handler`), `pkg/server/server.go` (`addProxyCredentialRoutes` points at `xtreamStreamHandler`, `xtreamStreamLive`, `xtreamStreamMovie`, `xtreamStreamSeries`)
- Test: `pkg/server/xtream_handlers_stream_test.go`

**Interfaces:**
- No new code; the viewer-credential routes use the existing handlers. The deleted handlers' extra debug lines are not recreated.

- [ ] **Step 1: Write the test** `TestViewerCredentialRoutesUseStreamHandlers` (Review Focus 4): through the router with viewer credentials, `/live/<u>/<p>/<filtered id>` is rejected exactly as the provider-credential route rejects it, and `/live/<u>/<p>/42.ts` reaches the fake provider at `/live/<prov user>/<prov pass>/42.ts`. It passes before and after (behaviour-preserving refactor) — run it before the change to prove it pins today's behaviour.
- [ ] **Step 2: Delete and re-point.**
- [ ] **Step 3: Verify** — the test and the full suite pass; vet, gofmt clean.
- [ ] **Step 4: Commit** — `refactor: viewer-credential routes reuse the stream handlers`

---

### Task 3: One VOD extension and path resolver (B2)

**Files:**
- Modify: `pkg/server/vod_cache.go` (`resolveVODCacheURL`), `pkg/server/handlers_vod.go` (`resolveVODCacheTarget`), their callers (`server.go` `handleTemporaryLink`, `xtream_handlers_stream.go` `streamVODWithCache`, `handlers_vod.go` download path)
- Test: `pkg/server/vod_cache_test.go` (or the existing VOD test file)

**Interfaces:**
- Produces: `func (c *Config) resolveVODTarget(basePath, defaultExt, id string, probe bool) (upstream, dest, idRaw string)` replacing both functions. Order: extension in `id` → catalogue (`findVODExtensionInCache`) → if `probe && c.VODExtProbeEnabled` then `pickVODExtension` → `defaultExt` (or `defaultVODExt(basePath)` when `defaultExt == ""`). Upstream via `c.providerStreamURL`; dest = `filepath.Join(utils.VODCacheDir(), idRaw+ext)`; keeps today's `os.MkdirAll` of the cache dir and the debug lines of both originals.
- Request-path callers pass `probe=false`; the background download path passes `probe=true`.

- [ ] **Step 1: Write the failing test** `TestVODResolverSameFileForPlayerAndDownload` (Review Focus 3): with `VODExtProbeEnabled = true`, a fake provider whose probe answers for `.mp4`, an id without extension and no catalogue entry → the request-path call (`probe=false`, default `.mkv` for series / `.mp4` for movie per today's defaults) and the download call (`probe=true`) — assert both resolve the same `dest` once the probed extension is known to the catalogue, and that with an explicit extension in the id both return identical `upstream` and `dest`. Also: no extension anywhere → both return the per-type default.
- [ ] **Step 2: Run** → FAIL (undefined `resolveVODTarget`).
- [ ] **Step 3: Implement** and switch callers; delete the two old functions.
- [ ] **Step 4: Verify** — focused test and full suite pass; vet, gofmt clean.
- [ ] **Step 5: Commit** — `refactor(vod): one extension and cache-path resolver for players and downloads`

---

### Task 4: `session/manager.go` split by job (B4)

**Files:**
- Create: `pkg/session/temp_links.go` (`GenerateTemporaryLink`, `GetTemporaryLink`, their state/types if only they use them), `pkg/session/cache_maintenance.go` (`runCacheMaintenance` and helpers only it uses)
- Modify: `pkg/session/manager.go`

**Interfaces:**
- Pure moves inside package `session`; exported API unchanged; struct fields stay in `SessionManager` (no new types).

- [ ] **Step 1: Move** verbatim; GPL header on new files.
- [ ] **Step 2: Verify** — `$G build`, full suite, vet, gofmt; report each moved function as "moved verbatim".
- [ ] **Step 3: Commit** — `refactor(session): temporary links and cache maintenance in their own files`

---

### Task 5: `server.go` keeps only startup (B5)

**Files:**
- Create: `pkg/server/m3u_playlist.go`, `pkg/server/request_log.go`
- Modify: `pkg/server/server.go`, `pkg/server/auth.go`, `pkg/server/routes.go`, `pkg/server/handlers_vod.go`, `pkg/server/xtream_handlers_stream.go`

**Interfaces:**
- Pure moves (targets in File Structure above): `addProxyCredentialRoutes` → routes.go; `authWithPathCredentials`, `resolveRequestUsername` → auth.go; `handleTemporaryLink` → handlers_vod.go; `multiplexedStream`, `classifyStreamType` → xtream_handlers_stream.go; `playlistInitialization`, `marshallInto`, `replacePath`, `sanitiseFilename` → m3u_playlist.go; `quietRequestLogger`, `redactRequestURI` → request_log.go. Also move the stranded `isVODPath` doc comment in helpers.go back onto `isVODPath`.

- [ ] **Step 1: Move** verbatim.
- [ ] **Step 2: Verify** — full suite, vet, gofmt; `server.go` contains only `NewServer`, `Serve`, their helpers and the refreshers it starts; report each moved function as "moved verbatim".
- [ ] **Step 3: Commit** — `refactor(server): move handlers, auth, playlist and logging code next to their siblings`

---

### Task 6: Long functions split by job (B3)

**Files:**
- Modify: `pkg/server/vod_cache.go` (`fetchToFile`), `pkg/server/server.go` (`NewServer`, `Serve`), `pkg/server/xtream_handlers_stream.go` (`multiplexedStream`, after Task 5)

**Interfaces:**
- `NewServer` becomes the ordered list of setup steps, one unexported method or function per subsystem it builds today (database + session manager, catchup, slate, Discord, health, provider info, …), same order (Review Focus 2). `Serve` separates background-worker start/stop from router setup. `fetchToFile` and `multiplexedStream` are split along the jobs their bodies already have (e.g. request + resume, copy with pacing, finalise / upstream session acquisition, client streaming, cleanup). Names say what each piece is for. No new types; parameters that already travel together may stay grouped as they are.

- [ ] **Step 1: Split** — code moved, not rewritten; error handling, logging and comments move with it. Each new function ≥ one real job (no helper that only shortens).
- [ ] **Step 2: Verify** — full suite (`-race`), vet, gofmt; report per original function the new functions and that bodies moved verbatim apart from parameter plumbing.
- [ ] **Step 3: Measure** — `CATALOG_STORAGE=disk DB_HOST=/tmp/claude-1000/-home-user-claude/8cdd110d-fcee-5dd5-b301-5ed36ce7d667/scratchpad DB_PORT=5433 DB_NAME=streamshare DB_USER=postgres tools/memprobe/measure.sh prb-disk` (start Postgres if `pg_isready` fails); report the five RSS lines (baseline 89 settled / 81 after refresh).
- [ ] **Step 4: Commit** — `refactor: split long functions by job`

---

## After the last task (controller)

- Whole-branch review; push; open the PR against master (template-free repo), owner merges.
