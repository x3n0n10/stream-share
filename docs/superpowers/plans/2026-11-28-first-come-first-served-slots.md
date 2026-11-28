# First Come, First Served Slots, Timeshift and Pause Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Provider connections go to whoever holds them first; timeshift readers and paused household viewers count as watching; recording never stops because a viewer stopped reading; the pause grace is a Suite setting.

**Architecture:** Remove "newest wins" (and the `Mode` parameter) from `provider.Arbiter`; route every live start through `acquirePolite`; movies get a 503 pre-check. `StreamBuffer` gains per-viewer catchup readers, a `lastStallAt` timestamp and a `paused` flag, all read lock-free by `audienceClass()`. `deliver` never blocks a cached channel's pump for longer than a short pause stall timeout.

**Tech Stack:** Go 1.27 (`-mod=vendor`), gin; Suite: Node (`node --test`), React i18n JSON.

**Spec:** `docs/superpowers/specs/2026-11-27-timeshift-keeps-provider-slot-design.md`

## Global Constraints

- Build and test with `-mod=vendor`: `go test -mod=vendor ./pkg/...`.
- CI runs golangci-lint (errcheck): use `_ = f.Close()` / `_, _ = w.Write(...)`, tests included. Before pushing: `docker run --rm -v "$PWD":/app -w /app -e GOFLAGS=-mod=vendor golangci/golangci-lint:latest golangci-lint run ./...`.
- `gofmt -l pkg/` must print nothing.
- New Go files carry the repo's GPL license header (copy from a neighbour).
- "In use" copy, verbatim: `All provider connections are in use by other viewers.`
- Pause grace default stays 5 minutes (`CATCHUP_PAUSE_GRACE_MINUTES`); proposed pause stall timeout 5 s.
- No provider, instance or channel names, URLs or credentials from the owner's setup in code, tests, commits or PR text; use placeholders.
- Commits: the controller runs the simplify review and commits (simplify-guard hook); implementers stage only.

## Review Focus

1. A viewer switching channel while their own old stream still holds the only slot: expects the new channel, never "in use" from themselves (Task 1, Task 3 tests).
2. Two household viewers, one slot, both live: the second gets `IN_USE`, the first keeps playing (Task 1 test).
3. A viewer pauses, and later another viewer of the same stream leaves while still reading: still a pause (Task 4 test `TestPauseSurvivesOtherViewerLeavingLast`).
4. A stream that stopped while readers or the pause flag were set: no stuck reader count, no stuck pause (Task 3 `TestEndAfterStopLeavesNoReader`, Task 4 `TestPauseGraceExpiryClearsPause`).
5. A guest (share link) pausing: not protected, the household can take the slot (Task 4 test).

---

### Task 1: First come, first served in the arbiter and for live starts

**Files:**
- Modify: `pkg/provider/arbiter.go` (remove `Mode`, `Fresh`, `Resume`, the same-class branch in `AcquireClass`, `oldestHolderOfClassLocked`, `waiter.mode`)
- Modify: `pkg/session/manager.go:850-866` (non-failover path calls `acquirePolite`)
- Modify: `pkg/session/failover.go:96-113` (drop the mode argument)
- Modify: `pkg/session/slate_fallback.go:143`, `pkg/server/vod_cache.go:96-123` (drop mode; delete `slotMode`)
- Test: `pkg/provider/arbiter_test.go`, `pkg/session/failover_test.go`, `pkg/session/provider_slots_test.go`, `pkg/server/handlers_health_test.go` (call sites)

**Interfaces:**
- Produces: `func (a *Arbiter) Acquire(ctx context.Context, label string, foreground func() bool) (*Lease, error)` and `func (a *Arbiter) AcquireClass(ctx context.Context, label string, class func() Class) (*Lease, error)`. A request preempts only holders of a lower class; otherwise it waits. Fail-fast stays with callers (`WouldGrant`).

- [ ] **Step 1: Replace `TestFreshForegroundWinsResumeWaits` in `pkg/provider/arbiter_test.go`**

```go
// A watched request never takes the slot of a watched holder: it waits.
func TestForegroundNeverPreemptsForeground(t *testing.T) {
	a := New(fixed(1))
	ctx := context.Background()
	fgFn, _ := flag(true)
	older, _ := a.Acquire(ctx, "older", fgFn)
	newer := acquireAsync(t, a, ctx, "newer", fgFn)
	if preempted(older, 400*time.Millisecond) {
		t.Fatal("a foreground request preempted a foreground holder")
	}
	older.Release()
	if l := got(newer, time.Second); l == nil {
		t.Fatal("newer did not get the freed slot")
	}
}
```

Update `acquireAsync` and every other `Acquire`/`AcquireClass` call in the package tests to the signatures above.

- [ ] **Step 2: Replace `TestWithoutTargetsNewestStillWins` in `pkg/session/failover_test.go`**

```go
func TestWithoutTargetsFirstComeKeepsSlot(t *testing.T) {
	own := newFakeUpstream(t, 'O', 0)
	sm, _ := failoverRig(t, nil)
	aliceWatches(t, sm, own)

	buffer, _ := watch(t, sm, "bob", "ch2", "live", own.URL)
	if uerr := waitReady(t, buffer); uerr == nil || uerr.Key != KeyAllInUse {
		t.Fatalf("bob's start = %v, want %s", uerr, KeyAllInUse)
	}
	if waitStopped(sm, "ch1", 500*time.Millisecond) {
		t.Fatal("bob's start must not cut alice's stream")
	}
}

// A viewer's own switch waits for the slot their previous channel frees.
func TestOwnSwitchGetsTheSlot(t *testing.T) {
	own := newFakeUpstream(t, 'O', 0)
	sm, _ := failoverRig(t, nil)
	aliceWatches(t, sm, own)

	_, key := watch(t, sm, "alice", "ch2", "live", own.URL)
	if !served(sm, "ch2", key, 'O', 5*time.Second) {
		t.Fatal("alice's new channel should play")
	}
}
```


- [ ] **Step 3: Run tests to verify they fail**

Run: `go test -mod=vendor ./pkg/provider/ ./pkg/session/ -run 'TestForegroundNeverPreemptsForeground|TestWithoutTargetsFirstComeKeepsSlot|TestOwnSwitchGetsTheSlot'`
Expected: compile errors (old signatures), then FAIL on preemption once compiling.

- [ ] **Step 4: Implement**

- Arbiter: delete `Mode`, its constants, `waiter.mode`, the `if victim == nil && mode == Fresh { ... }` block and `oldestHolderOfClassLocked`. Update the package doc comment: watched requests never preempt each other.
- `manager.go` non-failover branch: `lease, slotErr = sm.acquirePolite(ctx, buffer)` for both forwarded and household streams (the `if buffer.forwarded` split goes).
- `vod_cache.go`: drop `slotMode`; `acquire()` calls `c.providerSlots.Acquire(ctx, label, dl.foreground)`.
- Fix every remaining call site and test the compiler reports.

- [ ] **Step 5: Run the whole suite**

Run: `go test -mod=vendor ./pkg/...`
Expected: PASS. A test that asserted "newest wins" elsewhere is updated to first come, first served, never deleted silently.

- [ ] **Step 6: Stage for the controller**

```bash
git add pkg/provider pkg/session pkg/server
```
Commit message: `feat(slots): first come, first served for watched streams`

---

### Task 2: Movies and series answer 503 when every slot is watched

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (`streamVODWithCache`, before `startBackgroundDownload`)
- Test: `pkg/server/vod_churn_test.go` (next to the harnesses that call `streamVODWithCache`)

**Interfaces:**
- Consumes: `(*provider.Arbiter).WouldGrant(provider.Foreground) bool`; `c.inProgressDownloads` (`sync.Map`, key = bare stream id).

- [ ] **Step 1: Write the failing test** `TestVODAnswers503WhenEverySlotIsWatched`: config with VOD cache on, `providerSlots = provider.New(func() int { return 1 })` already holding a lease from `AcquireClass(ctx, "viewer", func() provider.Class { return provider.Foreground })`; request `/movie/<user>/<pass>/<id>.mp4` for an uncached id. Assert status `503`, body contains `All provider connections are in use by other viewers.`, and the fake upstream got no request. Second case: the same request while `c.inProgressDownloads` already holds a download for that id joins it (status is not 503). Build on the harness in `vod_churn_test.go:157` / `vod_progressive_test.go:227` with `fakeVODProvider` and `waitDownloadsIdle` (`handlers_vod_test.go`); the held lease is the `AcquireClass(... "viewer" ...)` line from `handlers_health_test.go:219`.

- [ ] **Step 2: Run it to verify it fails**

Run: `go test -mod=vendor ./pkg/server/ -run TestVODAnswers503WhenEverySlotIsWatched`
Expected: FAIL (no 503).

- [ ] **Step 3: Implement** in `streamVODWithCache`, right before `startBackgroundDownload`: if `c.inProgressDownloads` has no entry for `idRaw` and `!c.providerSlots.WouldGrant(provider.Foreground)`, answer `ctx.String(http.StatusServiceUnavailable, "All provider connections are in use by other viewers.\n")`, log at INFO, and return. The race between the check and the acquire is accepted (spec).

- [ ] **Step 4: Run the package tests**

Run: `go test -mod=vendor ./pkg/server/`
Expected: PASS.

- [ ] **Step 5: Stage** — commit message: `feat(vod): answer 503 when every provider slot is watched`

---

### Task 3: Timeshift readers count as watching

**Files:**
- Modify: `pkg/session/manager.go` (`StreamBuffer` fields, `audienceClass`, `watchedBeyond`, replace `NotifyCatchupActivity` with `BeginCatchupRead`)
- Modify: `pkg/server/xtream_handlers_stream.go:177-182` (handler)
- Test: `pkg/session/catchup_readers_test.go` (new)

**Interfaces:**
- Consumes: Task 1's `acquirePolite` path (a second watched start gets `KeyAllInUse`).
- Produces: `func (sm *SessionManager) BeginCatchupRead(username, id string) (end func())` — `id` with or without extension; returns a no-op `end` when no live stream matches. Fields on `StreamBuffer`: `readersMu sync.Mutex`, `readers map[string]int` (for `watchedBeyond`), `readerCount atomic.Int32` (for `audienceClass`, lock-free). `func (sm *SessionManager) lastViewerGone(buffer *StreamBuffer, username string)` — caller holds `streamLock`; the one place that decides what happens when a stream's last client or reader leaves (Task 4 extends it).

- [ ] **Step 1: Write the failing tests** in `pkg/session/catchup_readers_test.go`, each with `slots := provider.New(func() int { return 1 })`, `sm.SetProviderSlots(slots)`, `sm.SetCatchupManager(catchup.New(true, t.TempDir(), 1))`, `sm.SetPauseGrace(time.Minute)`, upstreams from `countingUpstream`, and `alice` watching `42.ts` then leaving (`removeKey`):
  - `TestCatchupReaderKeepsSlot`: `end := sm.BeginCatchupRead("alice", "42")`; bob's `RequestStream("bob", "43.ts", ...)` buffer readies with `KeyAllInUse`; `streamActive(sm, "42.ts")` stays true.
  - `TestCatchupReaderEndStartsPauseGrace`: with `SetPauseGrace(200*time.Millisecond)`, after `end()` the stream `42.ts` stops within 2 s.
  - `TestOtherViewersSwitchKeepsReadersStream`: capacity 2; alice reads `42` (after leaving); bob watches `42.ts`, then bob requests `43.ts` → `42.ts` stays active.
  - `TestOwnSwitchIgnoresOwnReader`: alice reads `42`, then alice requests `43.ts` → `42.ts` stops and `43.ts` plays (`receiving`).
  - `TestEndAfterStopLeavesNoReader`: alice reads `42`; `sm.streamLock.Lock(); sm.stopStream("42.ts"); sm.streamLock.Unlock()`; `end()` twice → the buffer's `readerCount` is 0 and `readers` is empty (never negative).

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -mod=vendor ./pkg/session/ -run 'TestCatchupReader|TestOtherViewersSwitchKeepsReadersStream|TestOwnSwitchIgnoresOwnReader|TestEndAfterStopLeavesNoReader'`
Expected: compile error (`BeginCatchupRead` undefined).

- [ ] **Step 3: Implement**
  - `BeginCatchupRead`: under `streamLock`, find the active buffer by bare id (same matching as `ReleasePausedStream`, which loops over `pendingStops`; here loop over `streamBuffers`), `cancelPendingStop`, and under `readersMu` increment `readers[username]` and `readerCount`.
  - `end` (idempotent via `sync.Once`): take `streamLock` first, then under `readersMu` decrement only if the entry exists (delete at `<= 0`, never create one; adjust `readerCount` with it). Then, if `sm.streamBuffers[id] == buffer && buffer.active`, call `lastViewerGone(buffer, username)`.
  - `lastViewerGone`: when the buffer has no clients and `readerCount == 0`, apply today's rule moved out of `RemoveClient` (`diskBuffer != nil && pauseGrace > 0 && !IsFailoverIdentity(username)` → `schedulePendingStop`, else `stopStream`). `RemoveClient`'s `remaining == 0 && buffer.active` branch calls it instead of its inline rule, so a stream with a reader left is not stopped.
  - `stopStream` does not touch `readers`: a stopped stream's pump releases its lease, so the arbiter never asks that buffer again.
  - `audienceClass()`: return `provider.Foreground` first when `b.readerCount.Load() > 0`.
  - `watchedBeyond(username, guests)`: also true when `readers` has an entry for a user other than `username`.
  - Delete `NotifyCatchupActivity`.
  - Handler: where `NotifyCatchupActivity` was called, keep the `if c.sessionManager != nil` guard: `defer c.sessionManager.BeginCatchupRead(c.resolveRequestUsername(ctx), idRaw)()` inside it.

- [ ] **Step 4: Run the session and server suites**

Run: `go test -mod=vendor ./pkg/session/ ./pkg/server/`
Expected: PASS.

- [ ] **Step 5: Stage** — commit message: `feat(catchup): timeshift readers count as watching`

---

### Task 4: Pause keeps recording and keeps the slot

**Files:**
- Modify: `pkg/session/manager.go` (`deliver`, `RemoveClient`, `cancelPendingStop`, `audienceClass`, `StreamBuffer` fields, `SessionManager.pauseStallTimeout` + setter)
- Test: `pkg/session/pause_test.go` (new)

**Interfaces:**
- Consumes: Task 3's `audienceClass` shape (lock-free checks before `clientsLock`).
- Produces: `func (sm *SessionManager) SetPauseStallTimeout(d time.Duration)`, modelled on `SetClientStallTimeout` (ignores `d <= 0`; default `defaultPauseStallTimeout = 5 * time.Second`). Fields on `StreamBuffer`: `lastStallAt atomic.Int64` (unix nanos, 0 = never), `paused atomic.Bool`.

- [ ] **Step 1: Write the failing tests** in `pkg/session/pause_test.go`. Setup: catchup on (`catchup.New(true, t.TempDir(), 1)`), `SetPauseGrace(time.Minute)`, `SetPauseStallTimeout(200 * time.Millisecond)`, a streaming upstream (`streamingUpstream` with enough chunks to outlast the test).
  - `TestStalledViewerOnCachedChannelIsClosedAndRecordingContinues`: alice requests `42.ts` and never reads. Within 2 s her client is gone (`GetClientChannel` false), and `catchupManager.GetBuffer("42").BytesBuffered()` keeps growing over the next 500 ms.
  - `TestPausedViewerKeepsSlot`: capacity 1; alice stalls out as above → `buffer.paused` true; bob's `43.ts` readies with `KeyAllInUse`; `42.ts` stays active.
  - `TestStoppedViewerFreesSlot`: capacity 1; alice reads (`keepDraining`) then `removeKey` → `paused` false, `audienceClass()` Background; bob's `43.ts` plays.
  - `TestPauseSurvivesOtherViewerLeavingLast`: carol and alice on `42.ts`; alice stalls out; carol leaves while reading → `paused` true.
  - `TestPausedGuestNotProtected`: `ShareIdentityPrefix+"x"` stalls out on `42.ts` → `paused` false.
  - `TestResumeClearsPause`: after a pause, `end := sm.BeginCatchupRead("alice", "42")` → `paused` false (and the pending stop is gone); `end()`.
  - `TestPauseGraceExpiryClearsPause`: `SetPauseGrace(300*time.Millisecond)`; alice stalls out → after the grace the stream stops and `paused` is false.
  - `TestStopAfterJitterIsNotAPause`: alice reads normally (`keepDraining`) for 1 s, then leaves → `paused` false.

- [ ] **Step 2: Run them to verify they fail**

Run: `go test -mod=vendor ./pkg/session/ -run 'TestStalledViewerOnCachedChannel|TestPaused|TestStoppedViewerFreesSlot|TestPauseSurvives|TestResumeClearsPause|TestPauseGraceExpiryClearsPause|TestStopAfterJitterIsNotAPause'`
Expected: compile error (`SetPauseStallTimeout` undefined).

- [ ] **Step 3: Implement**
  - `deliver`: first `select { case cl.ch <- chunk: return false; case <-cl.done: return false; default: }` (no timer, no clock read on this path). Channel full and `buffer.diskBuffer != nil`: wait at most `sm.pauseStallTimeout` (sole or not), on expiry `cl.close()` and return true. Without a disk buffer keep today's behaviour (sole: block; several: `clientStallTimeout`). The `fanOut` sole branch needs no `RemoveClient` call: `cl.close()` ends the handler, which calls `RemoveClient` itself.
  - `RemoveClient`: before deleting the leaving client, if it is household (not a share/failover identity) and `cl.stalled()` (its channel still full: stall timeout, or TiviMate closing while full), store `buffer.lastStallAt` = now. `deliver` never writes it.
  - `lastViewerGone` (from Task 3): before `schedulePendingStop`, if `time.Since(lastStallAt) < sm.pauseGrace` → `buffer.paused.Store(true)`. Guests never write `lastStallAt`, so a guest-only stall stays a stop.
  - Clear `paused` in `stopStream` (next to `active = false`), in `BeginCatchupRead` (resume), and where `RequestStream` cancels a pending stop on join (`manager.go:618`).
  - `audienceClass()`: Foreground first when `b.paused.Load()` (after the reader check).
  - Update the `deliver` doc comment and the multiplexing comment block to describe the cached-channel rule.

- [ ] **Step 4: Run all tests, race detector on session**

Run: `go test -mod=vendor ./pkg/... && go test -mod=vendor -race ./pkg/session/`
Expected: PASS.

- [ ] **Step 5: Stage** — commit message: `feat(catchup): detect pause, keep recording and the slot`

---

### Task 5: Pause grace in the Suite (repo `stream-share-suite`, own branch and PR)

**Files:**
- Modify: `server/src/schema/instance.js` (after `catchupDurationHours`)
- Modify: `web/src/locales/{en,nl,de,fr,es}/translation.json` (next to the `catchupDurationHours` entry, same shape)
- Test: `server/test/instance.test.js`

**Interfaces:**
- Produces: schema field `catchupPauseGraceMinutes`, `envVar: "CATCHUP_PAUSE_GRACE_MINUTES"`, `default: "5"`, `group: "Caching"`, `dependsOn: { key: "catchupEnabled", equals: "true" }`, label `Minutes a paused channel is kept`, help: `How long a paused channel keeps its provider connection and keeps recording. While it is paused, other viewers get a fallback or an "in use" message.`

- [ ] **Step 1: Write the failing test** in `server/test/instance.test.js`: `renderEnv` of an instance with `catchupEnabled: "true"` contains `CATCHUP_PAUSE_GRACE_MINUTES=5`; with `catchupEnabled: "false"` it does not. Follow how the file tests `CATCHUP_DURATION_HOURS` or another `dependsOn` field.
- [ ] **Step 2: Run** `cd server && npm test` — expected FAIL.
- [ ] **Step 3: Add the field and the five translations** (nl/de/fr/es translated, not English copies).
- [ ] **Step 4: Run** `cd server && npm test` — expected PASS, including `i18n-coverage.test.js`.
- [ ] **Step 5: Stage** — commit message: `feat(instance): pause grace setting for catchup`

---

### Task 6: Live check on the dev suite (controller, with the owner)

- [ ] Build a dev image of the branch (`gh workflow run dev-build.yml --ref <branch>`), point the dev instance at it, catchup on.
- [ ] Repeat run 3 of the investigation (4 min live, rewind 3 min, 7 min timeshift): still smooth.
- [ ] One slot, viewer A timeshifting, viewer B starts another channel: B gets the slate, A keeps playing past the end of the buffer.
- [ ] Ask the owner to pause TiviMate on the dev instance for ~3 minutes and resume: the log shows the stall close, `paused`, and the resume served from the buffer with no gap. If TiviMate does not stall before closing, report back before changing the rule.
