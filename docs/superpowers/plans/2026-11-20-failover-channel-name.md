# Failover channel name Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a live stream fails over, the Suite shows which channel the fallback instance plays, on Overview and History.

**Architecture:** The fallback (`handleFailoverLive`) names its matched channel in an `X-Failover-Channel` response header. The calling instance stores it next to `Via` (in memory and in a new `stream_history.via_channel` column) and exposes `via_channel` in `/status`, `/api/streams` and the history API. The Suite renders it, falling back to today's text when it's absent.

**Tech Stack:** Go (gin, Postgres via database/sql) in `stream-share`; Node + React (Vite), `node --test` in `stream-share-suite`.

**Spec:** `docs/superpowers/specs/2026-11-20-failover-channel-name-design.md` (in `stream-share`)

## Global Constraints

- Header value is URL-encoded with `url.QueryEscape` and decoded with `url.QueryUnescape`; a value that fails to decode counts as absent.
- New JSON fields are `via_channel` with `omitempty`.
- Column: `via_channel TEXT NOT NULL DEFAULT ''`, added with `ALTER TABLE … ADD COLUMN IF NOT EXISTS`, right after the `via` migration.
- Extend `SetVia`/`GetVia`, `recordHistoryVia`, `SetStreamHistoryVia`; add no parallel functions.
- Copy: Overview `Showing {via}'s {via_channel} instead` / `Using {via}'s connection`; History `Used {via}'s {via_channel}` / `Used {via}'s connection`.
- stream-share: run tests with `-mod=vendor`; run `gofmt -l pkg/` and golangci-lint (errcheck) before pushing. Commit author `x3n0n10 <20641331+x3n0n10@users.noreply.github.com>`.
- No names, URLs or credentials from the owner's setup in code, tests or PR text: use "Spare", "Channel X", `example.com`.
- Postgres tests (`*_pg_test.go`) need `SS_TEST_DSN`: use a throwaway container (`docker run -d --rm --name ss-pg-test -e POSTGRES_PASSWORD=test -p 55432:5432 postgres:16`, then `SS_TEST_DSN=postgres://postgres:test@localhost:55432/postgres?sslmode=disable`), never the dev suite's database.

## Review Focus

- A channel name with spaces, `+`, `&` or non-ASCII (e.g. `Kanaal Één+ HD`) must arrive exactly as the fallback named it → test in Task 2.
- A fallback that sends no header (older version) → `Via` set, channel empty, Suite shows today's text → tests in Tasks 2 and 4.
- A malformed header (`%zz`) → channel empty, failover still works → test in Task 2.
- A viewer whose history row opens after failover started still gets the channel on their row → test in Task 3.
- History rows from before the migration have `via_channel = ''` and render the old text → covered by the column default plus Task 4's no-channel case.

---

### Task 1: The fallback names its channel

**Files:**
- Modify: `pkg/session/failover.go` (header constant)
- Modify: `pkg/server/failover.go` (`handleFailoverLive`)
- Test: `pkg/server/failover_test.go`

**Interfaces:**
- Produces: `session.FailoverChannelHeader = "X-Failover-Channel"` (exported const in `pkg/session/failover.go`, since the calling side lives in `session` and can't import `server`).

- [ ] **Step 1: Write the failing assertion** in the existing `TestFailoverEndpointStreams` (`pkg/server/failover_test.go`), after the status check: `url.QueryUnescape(resp.Header.Get(session.FailoverChannelHeader))` equals `lookupAPIChannelName("10")` and is non-empty.
- [ ] **Step 2: Run it:** `go test -mod=vendor ./pkg/server -run TestFailoverEndpointStreams` → FAIL (undefined `session.FailoverChannelHeader`).
- [ ] **Step 3: Implement:** add the const; in `handleFailoverLive`, after the match and before `multiplexedStream`, set the header to `url.QueryEscape(name)` when `lookupAPIChannelName(id)` returns a non-empty name.
- [ ] **Step 4: Run** `go test -mod=vendor ./pkg/server -run 'TestFailover'` → PASS.
- [ ] **Step 5: Commit** `feat(failover): fallback names the channel it serves`.

### Task 2: The calling instance keeps it and reports it live

**Files:**
- Modify: `pkg/types/types.go` (`StreamSession.ViaChannel`, `SetVia`, `GetVia`)
- Modify: `pkg/session/failover.go` (`failoverAttempt`)
- Modify: callers of `GetVia`: `pkg/session/manager.go:983-985`, `pkg/session/history.go:285`, `pkg/server/handlers_provider.go:270`, `pkg/server/handlers_streams.go` (`activeStreamItem`, `buildActiveStreamItem`)
- Test: `pkg/session/failover_test.go`, `pkg/server/stream_numbers_test.go`, plus test call sites of `SetVia`/`GetVia` (`pkg/session/history_via_test.go`, `pkg/server/handlers_provider_test.go`, `pkg/server/failover_targets_test.go`)

**Interfaces:**
- Consumes: `session.FailoverChannelHeader` (Task 1).
- Produces: `func (s *StreamSession) SetVia(instance, channel string)`; `func (s *StreamSession) GetVia() (instance, channel string)`; `activeStreamItem.ViaChannel string \`json:"via_channel,omitempty"\``. Task 3 passes the channel from `GetVia` into `recordHistoryVia`; until then `history.go` keeps calling `recordHistoryVia(streamID, via)`.

- [ ] **Step 1: Write the failing tests.**
  - In `pkg/session/failover_test.go`, give `fakeUpstream` an optional `channel string` field sent as the `X-Failover-Channel` header (raw, so a test can send a malformed value), and change the `via` helper to return both values from `GetVia`. New `TestFailoverRecordsFallbackChannel`, rig as `TestFailoverWhenAnotherViewerHoldsTheSlot`, with three subtests:
    - header `url.QueryEscape("Kanaal Één+ HD")` → `GetVia()` == `("Spare", "Kanaal Één+ HD")`
    - no header → `("Spare", "")`
    - header `%zz` → `("Spare", "")`, and bob is still served `'F'`
  - In `pkg/server/stream_numbers_test.go`, next to the existing via-omitted test: `s.SetVia("Spare", "Channel X")` → JSON contains `"via_channel":"Channel X"`; `s.SetVia("Spare", "")` → JSON has no `"via_channel"`.
- [ ] **Step 2: Run** `go test -mod=vendor ./pkg/session ./pkg/server` → FAIL (wrong number of arguments to `SetVia`).
- [ ] **Step 3: Implement.** Change `SetVia`/`GetVia` as in Interfaces. In `failoverAttempt`, decode `resp.Header.Get(FailoverChannelHeader)` with `url.QueryUnescape` (on error: `""`) and call `ss.SetVia(target.Name, channel)`. Callers that only need the instance use `via, _ := ss.GetVia()`. `buildActiveStreamItem` fills both fields from one `GetVia()` call. Update every test call site to the new signatures.
- [ ] **Step 4: Run** `go test -mod=vendor ./...` → PASS. Run `go vet -mod=vendor ./...`.
- [ ] **Step 5: Commit** `feat(failover): keep the fallback's channel and report it in streams`.

### Task 3: History stores it

**Files:**
- Modify: `pkg/database/schema.go` (migration after the `via` one)
- Modify: `pkg/database/stream_history.go` (`SetStreamHistoryVia`, `HistoryEntry.ViaChannel`, the `SELECT`s in `GetUserHistory` and `GetRecentHistory`)
- Modify: `pkg/session/history.go` (`recordHistoryVia` and its call at `:285`), `pkg/session/failover.go` (call at `:198`)
- Modify: `pkg/server/handlers_history.go` (both response structs `:100` and `:200`)
- Test: `pkg/database/stream_history_pg_test.go`, `pkg/session/history_via_test.go`, `pkg/server/handlers_history_pg_test.go`

**Interfaces:**
- Consumes: `GetVia() (instance, channel string)` (Task 2).
- Produces: `func (m *DBManager) SetStreamHistoryVia(streamID, via, channel string) error`; `func (sm *SessionManager) recordHistoryVia(streamID, via, channel string)`; `HistoryEntry.ViaChannel string`; history JSON `via_channel` (`omitempty`).

- [ ] **Step 1: Write the failing tests** (extend the existing ones, don't add parallel tests):
  - `TestSetStreamHistoryVia`: call with `("10", "Spare", "Channel X")`; the open rows read back `ViaChannel == "Channel X"` in both `GetUserHistory` and `GetRecentHistory`; rows without via read `""`.
  - `TestHistoryViaBothOrders`: change `historyVia` to check both values; subtest "via set before the insert" uses `ss.SetVia("Spare", "Channel X")` and expects the late-opened row to carry `("Spare", "Channel X")`; subtest "insert first, pump second" calls `recordHistoryVia(ch, "Spare", "Channel X")`.
  - `TestHistoryEndpointsCarryVia`: row a set with `"Channel X"` → body contains `"via_channel":"Channel X"`; row b has no `"via_channel"` key, in both the user history and the feed.
- [ ] **Step 2: Start the throwaway Postgres (Global Constraints) and run** `SS_TEST_DSN=… go test -mod=vendor ./pkg/database ./pkg/session ./pkg/server -run 'Via'` → FAIL (wrong number of arguments).
- [ ] **Step 3: Implement.** Migration line: `ALTER TABLE stream_history ADD COLUMN IF NOT EXISTS via_channel TEXT NOT NULL DEFAULT ''`. The `UPDATE` sets `via = $2, via_channel = $3`. Both `SELECT`s read `via_channel` after `via`. `history.go:285` passes both values from one `GetVia()` call; `failover.go:198` passes the decoded channel.
- [ ] **Step 4: Run** with `SS_TEST_DSN` set: `go test -mod=vendor ./...` → PASS. Without it, also PASS (pg tests skip). Then `gofmt -l pkg/` (empty) and the golangci-lint command from the repo memory/CI → no findings. Stop the container.
- [ ] **Step 5: Commit** `feat(history): store the fallback's channel`. Push `feat/failover-channel-name`; update PR #82's description.

### Task 4: The Suite shows it (in `stream-share-suite`, new branch `feat/failover-channel-name` from `origin/main`)

**Files:**
- Modify: `web/src/lib/failover.js` (exists: FallbackPicker helpers; add `failoverNote` next to its exports)
- Modify: `web/test/failover.test.js` (exists; add the new tests and the import)
- Modify: `web/src/pages/Overview.jsx:80`, `web/src/pages/History.jsx:39` (search) and `:153`

**Interfaces:**
- Consumes: `via`, `via_channel` on `/status` summary items and history rows (Tasks 2–3).
- Produces: `export function failoverNote(row, live)` → string; `row` has `via` (non-empty) and optional `via_channel`; `live` is `true` on Overview, `false` on History.

- [ ] **Step 1: Write the failing tests**, appended to `web/test/failover.test.js`:

```js
test("failoverNote: live, with channel", () =>
  assert.equal(failoverNote({ via: "Spare", via_channel: "Channel X" }, true), "Showing Spare's Channel X instead"));
test("failoverNote: live, older fallback without channel", () =>
  assert.equal(failoverNote({ via: "Spare" }, true), "Using Spare's connection"));
test("failoverNote: history, with channel", () =>
  assert.equal(failoverNote({ via: "Spare", via_channel: "Channel X" }, false), "Used Spare's Channel X"));
test("failoverNote: history, old row without channel", () =>
  assert.equal(failoverNote({ via: "Spare", via_channel: "" }, false), "Used Spare's connection"));
```

- [ ] **Step 2: Run** `cd web && npm test` → FAIL (`failoverNote` is not exported).
- [ ] **Step 3: Implement** `failoverNote` with the copy from Global Constraints. Overview line 80 and History line 153 render `failoverNote(s, true)` / `failoverNote(e, false)` inside the existing `<p>`. History search adds `|| e.via_channel?.toLowerCase().includes(q)`.
- [ ] **Step 4: Run** `cd web && npm test` → PASS; `npm run build` → succeeds.
- [ ] **Step 5: Commit** `feat(failover): show the fallback's channel on Overview and History`.

### Task 5: Recreate log gives the real reason (same Suite branch)

**Files:**
- Modify: `server/src/reconcile/reconciler.js:346-350` (`why` in `applyPlan`)
- Test: `server/test/reconciler.test.js`

- [ ] **Step 1: Write the failing test** `"a recreate after an image pull says so, not that the configuration changed"` next to the "stopped outside the Suite" test: build a running container whose labels match `computeSpecHash(SPEC)`, take `planComponent(NODE, SPEC)`, then `applyPlan({ ...plan, action: "recreate", reason: "A newer image was pulled", previousHash: plan.desiredHash }, { log })`. Assert some line includes `A newer image was pulled` and none includes `configuration changed`.
- [ ] **Step 2: Run** `cd server && npm test` → FAIL (the log says "configuration changed").
- [ ] **Step 3: Implement:** `plan.reason` wins when set, before the existing branches.
- [ ] **Step 4: Run** `cd server && npm test` and `cd web && npm test` → PASS.
- [ ] **Step 5: Commit** `fix(reconcile): log the real reason for a recreate after an image pull`. Push and open a draft PR referencing stream-share PR #82.
