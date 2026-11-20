# Health Check Active-Session Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** The provider health check never cuts an active connection; it reads health from live own-provider streams and probes only through a free slot.

**Architecture:** The session manager records, per live stream, the own provider's current failure (slate or provider-error fallback) and exposes a read-only evidence summary. `ProbeUpstream` acquires a Background-class arbiter lease (never preempts, yields to any preemption). The server's `runProviderProbe` consults the evidence first and dials only when there is none.

**Tech Stack:** Go 1.27 (vendored, `-mod=vendor`), gin, existing `pkg/provider` arbiter.

**Spec:** `docs/superpowers/specs/2026-11-20-health-check-active-session-design.md`

## Global Constraints

- Watchdog contract unchanged: same endpoints, statuses (`healthy`/`blocked`/`error`/`unknown`/`disabled`), HTTP codes, `HEALTHCHECK_MIN_INTERVAL_SECONDS` throttling.
- `KeyAllInUse` (local connection limit) is never a provider failure: it never yields `blocked`/`error` from evidence.
- A status in `HEALTHCHECK_BLOCKED_CODES` (default `456`) on a failing stream → `blocked`; any other own-provider failure → `error`.
- A serving own-provider stream wins over a failing one.
- Evidence gathering does no I/O and holds `streamLock` only for the iteration.
- golangci-lint (errcheck) must pass: `docker run --rm -v "$PWD":/app -w /app -e GOFLAGS=-mod=vendor golangci/golangci-lint:latest golangci-lint run ./...`.
- Before each `git commit`, run a standalone `echo simplify-guard:complete '<worktree>'` (repo hook).

## Review Focus

1. A viewer starts a channel while a probe is mid-dial on a hanging provider → the viewer must start promptly (well under the arbiter's 10 s `preemptGrace`), not wait for the probe. *Pinned in Task 2 (`TestViewerPreemptsProbe`).*
2. A stream that started on the slate and later recovered keeps a non-nil `StartError()` forever → it must still count as serving. *Pinned in Task 1 (`TestEvidenceClearedOnRecovery`).*
3. A stream still dialing (lease held, no response yet) must count as neither serving nor failing. *Pinned in Task 1 (`TestEvidenceStartingStreamNotCounted`).*
4. Fallback chosen because another household viewer held the only slot must not report blocked. *Pinned in Task 1 (`TestEvidenceFallbackForConnectionLimit`).*
5. Several failing streams with different codes (one 456, one 503) → `blocked` wins. *Pinned in Task 3 (`TestClassifyFailuresPrefersBlocked`).*

---

### Task 1: Record own-provider failure and expose evidence

**Files:**
- Modify: `pkg/session/manager.go` (StreamBuffer fields ~L205; pump slate path ~L877-910)
- Modify: `pkg/session/failover.go` (`failoverAttempt`)
- Modify: `pkg/session/slate_fallback.go` (`serveSlateAndRetry`, after a failed `redial`)
- Create: `pkg/session/provider_evidence.go`
- Test: `pkg/session/provider_evidence_test.go`

**Interfaces:**
- Produces: `func (sm *SessionManager) ProviderEvidence() (serving int, failing []*UpstreamError)`
- Produces (internal): `StreamBuffer.ownErr atomic.Pointer[UpstreamError]`; `func (b *StreamBuffer) noteOwnErr(uerr *UpstreamError)` — stores nil when `uerr == nil || uerr.Key == KeyAllInUse`, else `uerr`.

Rules for `ProviderEvidence`, per buffer in `sm.streamBuffers`:
- **serving**: `ownSlot` true, `ready` closed, not `slating`, `ownErr` nil.
- **failing**: `ownErr` non-nil (appended in map order).
- Neither otherwise (e.g. still dialing).

Where `noteOwnErr` is called:
- `failoverAttempt`: own provider served → `noteOwnErr(nil)`; served via a fallback → `noteOwnErr(ownErr)`.
- Pump, when it starts slating → `noteOwnErr(uerr)`.
- `serveSlateAndRetry`, after each failed `redial` → `noteOwnErr(retryErr)`.
- Pump, after slate recovery on a **non-failover** stream → `noteOwnErr(nil)` (failover streams were already noted by `failoverAttempt`).

- [ ] **Step 1: Write the failing tests** in `provider_evidence_test.go`, reusing `failoverRig`, `slateManager`, `newFakeSlate`, `newFakeUpstream`, `hangingUpstream`, `aliceWatches`, `watch`, `waitReady`, `served`, `keepDraining`:
  - `TestEvidenceServingOwnStream`: `aliceWatches` on own (status 0) → `serving == 1`, `len(failing) == 0`.
  - `TestEvidenceSlatingStream`: `slateManager` + own upstream 456, viewer draining, `waitReady` non-nil, `sm.Slating` true → `serving == 0`, `len(failing) == 1`, `failing[0].StatusCode == 456`.
  - `TestEvidenceFallbackAfterOwnError`: `failoverRig` with target Spare, own 456; bob served `'F'` → `serving == 0`, `failing[0].StatusCode == 456`.
  - `TestEvidenceFallbackForConnectionLimit`: alice on own, bob served via Spare (as `TestFailoverWhenAnotherViewerHoldsTheSlot`) → `serving == 1`, `len(failing) == 0`.
  - `TestEvidenceClearedOnRecovery`: `TestSlatingFlag` pattern (503 then healthy); once `Slating` is false → `serving == 1`, `len(failing) == 0`.
  - `TestEvidenceStartingStreamNotCounted`: `failoverRig(t, nil)` (no targets), stream on `hangingUpstream`; before `Ready()` closes → `serving == 0`, `len(failing) == 0`.

- [ ] **Step 2: Run to verify they fail**
  Run: `go test -mod=vendor ./pkg/session/ -run TestEvidence -v`
  Expected: build failure, `sm.ProviderEvidence undefined`.

- [ ] **Step 3: Implement** the field, `noteOwnErr`, the call sites above, and `ProviderEvidence` (RLock `streamLock`, non-blocking `select` on `b.ready`).

- [ ] **Step 4: Run to verify they pass, plus the package**
  Run: `go test -mod=vendor ./pkg/session/ -race`
  Expected: `ok`.

- [ ] **Step 5: Commit**
  `git add pkg/session && git commit -m "feat(session): record own-provider failures and expose provider evidence"`

---

### Task 2: Probe through the arbiter without preempting

**Files:**
- Modify: `pkg/session/slate_fallback.go` (`ProbeUpstream`)
- Test: `pkg/session/provider_slots_test.go`

**Interfaces:**
- Produces: `ProbeUpstream(ctx, target) *UpstreamError` — signature unchanged. New result `&UpstreamError{Key: KeyAllInUse}` means "no free slot / yielded to a viewer; nothing was measured".

Behaviour:
- `!sm.providerSlots.WouldGrant(provider.Background)` → return `KeyAllInUse` at once (no waiting).
- Else `AcquireClass(ctx, "health probe", nil, provider.Resume)` (nil class = Background); error → `KeyAllInUse`. `defer lease.Release()`.
- Dial under a child context cancelled when `lease.Preempted()` fires; if preempted, return `KeyAllInUse` whatever the dial returned.

- [ ] **Step 1: Write the failing tests**
  - `TestProbeSkipsWhenSlotHeld`: `failoverRig(t, nil)`, `aliceWatches` own; `ProbeUpstream` against a second `newFakeUpstream` → `uerr.Key == KeyAllInUse`, that upstream's `requests == 0`, `own.requests == 1`, `streamActive(sm, "ch1")`.
  - `TestProbeUsesFreeSlot`: `failoverRig(t, nil)`, no streams; probe a 200 upstream → nil; afterwards `slots.WouldGrant(provider.Background)` true.
  - `TestViewerPreemptsProbe`: probe `hangingUpstream` in a goroutine (ctx 5 s); wait until `len(slots.Snapshot().Held) == 1`; `watch` alice on own → `receiving(sm, "ch1", key, 3*time.Second)` true; probe returns `KeyAllInUse` within 3 s.

- [ ] **Step 2: Run to verify they fail**
  Run: `go test -mod=vendor ./pkg/session/ -run 'TestProbe|TestViewerPreemptsProbe' -v`
  Expected: `TestProbeSkipsWhenSlotHeld` fails (probe dials; requests == 1), `TestViewerPreemptsProbe` fails.

- [ ] **Step 3: Implement** as described.

- [ ] **Step 4: Run** `go test -mod=vendor ./pkg/session/ -race` → `ok`.

- [ ] **Step 5: Commit**
  `git commit -am "fix(session): health probe takes a background slot and yields to viewers"`

---

### Task 3: Health check uses evidence first; docs

**Files:**
- Modify: `pkg/server/handlers_health.go` (`runProviderProbe`)
- Modify: `README.md` (L544 "Not arbitrated" list; "How it works" step 1, ~L722)
- Test: `pkg/server/handlers_health_test.go`

**Interfaces:**
- Consumes: `ProviderEvidence()` (Task 1), `ProbeUpstream` `KeyAllInUse` result (Task 2), `session.KeyAllInUse`.
- Produces: `func (c *Config) classifyFailures(failing []*session.UpstreamError) (status, code, detail string)` — first failure whose status `isBlockedStatus` → blocked (same detail format as today: `"provider returned %d: egress IP is blocked"`); else `failing[0]` → `healthStatusError`, `uerr.Error()`. `runProviderProbe` uses it for both evidence and a dial result (`[]*session.UpstreamError{uerr}`).

`runProviderProbe` order, after the existing nil-manager check:
1. `serving > 0` → `healthy`, `""`, `fmt.Sprintf("own provider serving %d active stream(s)", serving)`.
2. `len(failing) > 0` → `classifyFailures(failing)`.
3. Build URL, `ProbeUpstream`; nil → healthy as today; `Key == session.KeyAllInUse` → `healthy`, `""`, `"provider connection in use"`; else `classifyFailures`.

- [ ] **Step 1: Write the failing tests** (Config built as in `provider_urls_test.go:91-99`, `HealthCheckStreamID: "42"`, `HealthCheckTimeoutSeconds: 5`, upstream = counting `httptest` server):
  - `TestClassifyFailuresPrefersBlocked`: `[{StatusCode:503,Key:"503"},{StatusCode:456,Key:"456"}]` → `blocked`, code `"456"`; `[{StatusCode:503,Key:"503"}]` → `error`.
  - `TestProbeReportsActiveStreamHealthy`: `sm.RequestStream("viewer","s1","live","S1", <streaming 200 upstream>)`, drain via `GetClientChannel`, wait `Ready()`; `runProviderProbe()` → `healthy`, detail contains `"serving 1"`, probe upstream requests == 0.
  - `TestProbeBusyIsHealthy`: `sm.SetProviderSlots(provider.New(func() int { return 1 }))`, hold a Foreground lease via `AcquireClass`; → `healthy`, detail `"provider connection in use"`, upstream requests == 0.
  - `TestProbeIdleDialsProvider`: no streams, upstream answers 456 → `blocked`, requests == 1.

- [ ] **Step 2: Run** `go test -mod=vendor ./pkg/server/ -run 'TestClassifyFailures|TestProbe' -v` → fails (`classifyFailures undefined`).

- [ ] **Step 3: Implement** the order above; README: remove the probe from "Not arbitrated", and add to step 1 that a check first reads active streams (own-provider stream playing → healthy without dialing; slate or provider-error fallback → that error, blocked when listed) and otherwise probes only through a free connection slot, never cutting a viewer.

- [ ] **Step 4: Verify everything**
  Run: `go test -mod=vendor ./... -race` → all `ok`; golangci-lint command from Global Constraints → no issues.

- [ ] **Step 5: Commit and push**
  `git commit -am "fix(health): read health from active streams before probing" && git push`
