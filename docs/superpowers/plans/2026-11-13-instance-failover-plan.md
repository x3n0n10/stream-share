# Live Channel Failover Between Instances Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A live channel the viewer's instance (the primary) cannot start is served through another StreamShare instance of the same Suite (a fallback), with one playlist in the player app.

**Architecture:** The fallback exposes one key-protected endpoint that takes a channel's EPG ID and name, picks its own matching channel and streams it as a low-priority guest that never gets an error slate. The primary's session manager, at channel start only, tries its own provider and then the fallback endpoints in order, and remembers which instance served the stream ("via"). The Suite stores an ordered fallback list per instance and injects addresses and keys as environment.

**Tech Stack:** Go 1.23 (stdlib, gin, vendored modules); Node 22 ESM, React 18, Vite, Tailwind (Suite).

**Spec:** `docs/superpowers/specs/2026-11-13-instance-failover-design.md` (read its "Builder decisions" section first: it records where this plan deviates from the earlier spec text).

## Global Constraints

- Two repos. Tasks 1-5: `/home/user/stream-share`, branch `docs/instance-failover-design` (draft PR #72 becomes the feature PR). Tasks 6-7: `/home/user/stream-share-suite`, new branch `feat/instance-failover` from `main`. Commit per task; do not push (the controller pushes).
- Go: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`. Full check: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean; `gofmt -l pkg cmd tools` empty. CI runs golangci-lint (errcheck, staticcheck). Never `pkill -f server.test`.
- Suite: server tests `node --test "test/*.test.js"` in `server/`; web tests `node --test "test/*.test.js"` in `web/`; `npm run build` in `web/` must pass. Before the first Suite commit: `git config user.name "Joris Lankhorst"` and `git config user.email "20641331+x3n0n10@users.noreply.github.com"` (repo-local).
- Before every `git commit`, run this as its own separate Bash call: `echo simplify-guard:complete '<repo path>'`. Commit messages end with `Co-Authored-By: Claude <noreply@anthropic.com>` and never name a model.
- No new dependencies. Go house style: `interface{}`, GPLv3 header on new files (copy from `pkg/server/helpers.go`), comments say why. Smallest complete change; reuse existing helpers; do not remove exported code.
- Live channels only (stream type `"live"`), at channel start only. Catchup (`"timeshift"`), VOD and mid-stream switching behave exactly as today. With no fallbacks configured, an instance behaves exactly as today.
- No instance ever opens more provider connections than its arbiter allows; the own provider is never dialled without a lease.
- Logs name instances by display name only: never an instance address, never a key. Never log `UpstreamError.Err` for a fallback dial (it contains the address). Keys are registered with `utils.RegisterSecrets`.
- Nothing real in the repos: tests and docs use `httptest` addresses, `example.test`, names like `Main` / `Spare`, keys like `test-key`.

## Review Focus

1. A fallback's error slate (HTTP 200 + video) taken for a working stream: a forwarded request must get the failure status both when it starts a stream and when it joins a household stream that is showing a slate. Tests in Tasks 1 and 3.
2. The lent viewer zaps: the previous forwarded stream is still being torn down on the fallback when the next request arrives. The fallback waits briefly for that slot instead of answering "busy". Test in Task 1.
3. A fallback that is down, hangs, runs an old version (endpoint answers 404) or has a wrong key (401): the primary moves on within its timeout, and no log line contains the fallback's address. Tests in Tasks 2 and 4.
4. A stream showing the slate must not hold the own provider slot, and must recover when either the own provider or a fallback becomes available. Tests in Task 2.
5. Suite: a fallback key that points at a deleted instance, at the instance itself, or appears twice is skipped; an instance not involved in any fallback list gets no new environment (so its container is not recreated). Tests in Task 6.

---

### Task 1: Session manager: forwarded (lent) streams

**Files:**
- Create: `pkg/session/failover.go`, `pkg/session/failover_test.go`
- Modify: `pkg/session/manager.go` (`audienceClass`, `StreamBuffer` fields, buffer creation in `RequestStream`, pump `streamToClients`, `RemoveClient`), `pkg/session/slate_fallback.go` (`dialUpstream`), `pkg/session/upstream_error.go`, `pkg/session/error_catalog.go`

**Interfaces (produces):**
- `const FailoverIdentityPrefix = "failover:"`; `func IsFailoverIdentity(username string) bool`. `audienceClass` treats share and failover identities alike (guests: `provider.Share`).
- `const KeyAllInUse = "IN_USE"`; catalog entry `{Meaning: "In Use", Message: "All provider connections are in use by other viewers."}`.
- `func (sm *SessionManager) dial(ctx context.Context, u *url.URL, header http.Header) (*http.Response, *UpstreamError)`: today's `dialUpstream` body plus the extra headers; `dialUpstream(ctx, u)` stays as `sm.dial(ctx, u, nil)`.
- `func (sm *SessionManager) dialWithin(ctx context.Context, d time.Duration, u *url.URL, header http.Header) (*http.Response, *UpstreamError)`: `d <= 0` is `dial`. Otherwise a child cancel context plus `time.AfterFunc(d, cancel)`; when the timer fired first the result is `&UpstreamError{Key: KeyTimeout}`. On success the body stays readable until the parent `ctx` ends.
- `StreamBuffer.forwarded bool`: set at creation when the creator is a failover identity; such a buffer has `slateOK = false`.
- `StreamBuffer.slating atomic.Bool` and `func (sm *SessionManager) Slating(streamID string) bool`: true from just before `markReady(uerr)` when a slate will be served (`sm.slateEnabled(buffer)`) until `serveSlateAndRetry` returns.
- SessionManager field `slotWait time.Duration` (default `1 * time.Second`).
- `func (sm *SessionManager) acquirePolite(ctx context.Context, buffer *StreamBuffer) (*provider.Lease, *UpstreamError)`:

```go
class := buffer.audienceClass()
// A guest-class request also waits when a guest-class or background holder
// has the slot: that is usually the stream this caller just dropped.
wait := class < provider.Foreground && sm.providerSlots.WouldGrant(provider.Foreground)
if !wait && !sm.providerSlots.WouldGrant(class) {
	return nil, &UpstreamError{Key: KeyAllInUse}
}
actx, cancel := context.WithTimeout(ctx, sm.slotWait)
defer cancel()
lease, err := sm.providerSlots.AcquireClass(actx, "live "+sm.streamLabel(buffer.streamID),
	func() provider.Class { return buffer.audienceClass() }, provider.Resume)
if err != nil {
	return nil, &UpstreamError{Key: KeyAllInUse}
}
return lease, nil
```

- Pump: a `forwarded` buffer gets its lease from `acquirePolite` instead of `AcquireClass(..., provider.Fresh)`; a `*UpstreamError` from it goes to `markReady` and stops the stream. Everything else in the pump is unchanged for it (it has no slate, so a failed dial stops it as today). Non-forwarded buffers keep today's path, apart from maintaining the `slating` flag. `WouldGrant`/`AcquireClass` are called on a possibly nil arbiter: confirm both are nil-safe (a nil arbiter grants everything).
- `RemoveClient`: when the last client leaves and the departing `username` is a failover identity, stop the stream at once (no pause grace), so the provider slot frees for the next forwarded request.

- [ ] **Step 1: Write the failing tests** in `pkg/session/failover_test.go` (reuse `newTestManager`, `streamingUpstream`, `countingUpstream`, `mustURL`, `receiving`, `streamActive`, `slateManager`, `newFakeSlate`; arbiter `provider.New(func() int { return 1 })` + `sm.SetProviderSlots`; set `sm.slotWait = 200 * time.Millisecond`):
  - `TestFailoverIdentityIsGuestClass`: a buffer whose only client is `failover:Main/1` has `audienceClass() == provider.Share`; with a household client added, `provider.Foreground`.
  - `TestDialWithin`: an upstream that sleeps 500 ms before headers, `dialWithin(ctx, 50*time.Millisecond, ...)` returns `Key == KeyTimeout` in under 300 ms; `d = 0` against a fast upstream succeeds and the upstream saw the extra header `X-Test: 1`; after a successful `dialWithin(ctx, 200ms, ...)` the body still reads data sent 400 ms later.
  - `TestForwardedStreamNeverSlates` (Review Focus 1): `slateManager` with an upstream answering 456; `RequestStream("failover:Main/1", ...)` as live: `buffer.StartError().StatusCode == 456`, `sm.ServesSlateFor(id) == false`, `sm.Slating(id) == false`, the stream ends.
  - `TestSlatingFlag`: household start on a failing upstream with slates on: `sm.Slating(id)` is true once `buffer.Ready()` closes; after the upstream recovers (scaffolding of `TestSlateResumesWhenUpstreamRecovers`) it is false.
  - `TestForwardedStreamYieldsToHousehold`: limit 1, a household stream active; a forwarded start of another channel gets `StartError().Key == KeyAllInUse` in under 100 ms and its upstream is never opened (`countingUpstream`).
  - `TestForwardedStreamWaitsForDepartingGuest` (Review Focus 2): limit 1, forwarded stream X active; stop X 50 ms after starting forwarded stream Y: Y starts (`StartError() == nil`). Second case: X never stops: Y gets `KeyAllInUse` after about `slotWait`.
  - `TestHouseholdPreemptsForwardedStream`: as `TestHouseholdPreemptsShareStream`, with a failover identity.
  - `TestForwardedLastClientStopsAtOnce`: with a pause grace set and a buffer that would normally get it, `RemoveClient` of the last failover-identity client stops the stream immediately. If no existing test builds a pause-grace stream, set `buffer.diskBuffer`/`sm.pauseGrace` the way `RemoveClient` reads them, in the smallest way that compiles.
  - `TestCatalogHasInUse`: `LoadCatalog("")` has the `IN_USE` entry with the text above.
- [ ] **Step 2: Run** `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./pkg/session/ -run 'Failover|Forwarded|DialWithin|Slating|CatalogHasInUse'` → FAIL (undefined names).
- [ ] **Step 3: Implement** the interfaces above.
- [ ] **Step 4: Verify** the focused run passes, then the full check from Global Constraints.
- [ ] **Step 5: Commit** — `feat(failover): lend a provider connection to another instance as a guest without slates`

---

### Task 2: Session manager: the primary fails over at channel start

**Files:**
- Modify: `pkg/session/failover.go`, `pkg/session/manager.go` (`RequestStream` channel-switch block and buffer creation, pump), `pkg/session/slate_fallback.go` (`serveSlateAndRetry`), `pkg/types/types.go`
- Test: `pkg/session/failover_test.go`

**Interfaces:**
- Consumes (Task 1): `acquirePolite`, `dialWithin`, `KeyAllInUse`, `slating`, `IsFailoverIdentity`, `IsShareIdentity`.
- Produces:
  - `type FailoverTarget struct { Name string; URL *url.URL; Header http.Header }` (`Name` = the fallback's display name).
  - `func (sm *SessionManager) SetFailoverTargets(fn func(streamID string) []FailoverTarget)`; `fn` gets the buffer's stream key and must do no I/O.
  - `types.StreamSession.Via string` (the fallback instance serving this stream; empty = own provider), written under the same lock as the session's other fields.
  - SessionManager fields `failoverOwnTimeout` and `failoverPeerTimeout` (both default `3 * time.Second`; with `slotWait` they stay under the handler's 8 s start window, say so in a comment).
  - `StreamBuffer.failover bool`: set at creation when a targets function is set, `streamType == "live"`, and the creator is neither a share nor a failover identity.
  - `StreamBuffer.ownSlot atomic.Bool`: true while the pump holds an own-provider lease (set on every path, also the unchanged one). `StreamBuffer.takeover bool`: set on the new buffer when this `RequestStream` call stopped the same viewer's previous stream and that buffer's `ownSlot` was true. `acquirePolite`'s rule becomes `wait := buffer.takeover || (class < provider.Foreground && ...)`.
  - `serveSlateAndRetry(ctx, buffer, redial func() (*http.Response, *UpstreamError), uerr)`: `redial` replaces the `sm.dialUpstream(ctx, upstreamURL)` call; the unchanged path passes exactly that call.
  - `func (sm *SessionManager) failoverReason(uerr *UpstreamError) string`: with a status code, `"<code> <catalog meaning, lower case>"` (just the code when the catalog has no meaning); otherwise the catalog meaning of the key in lower case (`"in use"`, `"timed out"`). Never `uerr.Err`.
- Pump for a `failover` buffer (replaces Fresh acquire + dial for it; one attempt is this, in order):
  1. `lease, uerr := acquirePolite(...)`. On success `dialWithin(ctx, ownTimeout, upstreamURL, nil)`; on dial success start today's preemption watcher for this lease and use the response. On dial failure `lease.Release()` (a failed or slating stream holds no slot).
  2. If the own step failed and the stream is not stopped: for each target of `sm.failoverTargets(buffer.streamID)` in order, `dialWithin(ctx, sm.failoverPeerTimeout, target.URL, target.Header)`. First success: set `Via` on the stream session, `utils.InfoLog("Failover: %s served via %s (own provider: %s)", label, target.Name, sm.failoverReason(ownErr))`, use the response. Failure: `utils.WarnLog("Failover: %s not served by %s (%s)", label, target.Name, sm.failoverReason(err))`, next target.
  3. All failed: the attempt's error is the own step's error (the slate shows the own failure).
  - First attempt: `ownTimeout = sm.failoverOwnTimeout`. The slate's `redial` runs the same attempt with `ownTimeout = 0` (a viewer on the slate is not inside the 8 s start window, and a slow provider must still be able to recover).
  - `markReady`, slate, stop and the read loop stay as today. The pump releases whatever lease it still holds when it ends (`Lease.Release` is nil-safe and idempotent). Log the existing `Upstream failed for ...` error line only when the whole attempt failed.

- [ ] **Step 1: Write the failing tests** (limit-1 arbiter unless said; own upstream and fallback are separate `httptest` servers; targets via `sm.SetFailoverTargets` returning `{Name: "Spare", URL: fallbackURL, Header: {"X-Failover-Key": {"test-key"}}}`; shorten the three timeouts):
  - `TestFailoverWhenAnotherViewerHoldsTheSlot`: alice watches ch1; bob starts ch2: bob receives the fallback's bytes, the fallback saw the header, alice's stream is still active, the own upstream was opened once, ch2's stream session has `Via == "Spare"`.
  - `TestFailoverOnOwnUpstreamError`: own upstream answers 456: the fallback serves, and `slots.WouldGrant(provider.Foreground)` is true while it does (lease released).
  - `TestFailoverTargetsInOrder` (Review Focus 3): targets `[refused connection, 404, hangs past failoverPeerTimeout, healthy]`: the healthy one serves, `Via` is its name, and a captured log (see how `pkg/server/log_redact_test.go` captures output; use the same seam or the smallest equivalent) contains none of the targets' `host:port`.
  - `TestOwnDialTimeoutFailsOver`: the own upstream hangs past `failoverOwnTimeout`: the fallback serves.
  - `TestAllFallbacksFailShowsOwnSlate` (Review Focus 4): slates on; own 456, fallback 503: viewers get the slate, `StartError().StatusCode == 456`, `sm.Slating(id)` true, `slots.WouldGrant(provider.Foreground)` true. Variant: alice holds the slot, bob's fallback fails: bob's `StartError().Key == KeyAllInUse`, alice still active.
  - `TestSlateRecoversViaFallback` (Review Focus 4): as above, then the fallback turns healthy: bob's stream continues with the fallback's bytes, `Via == "Spare"`, `Slating` false. Variant: the own upstream turns healthy instead: served by it, `Via == ""`.
  - `TestSameViewerSwitchTakesOwnSlot`: targets set; alice on ch1 then alice on ch2: ch2 comes from the own upstream (opened twice in total), the fallback got 0 requests, ch1 is stopped.
  - `TestNoFailoverForGuestsAndCatchup`: with the slot held, a share-identity start, a failover-identity start (a lent stream is never forwarded on) and a `"timeshift"` start each send 0 requests to the fallback.
  - `TestSecondViewerJoinsFallbackStream`: carol starts ch2 while bob watches it via the fallback: the fallback still has exactly 1 request.
  - `TestWithoutTargetsNewestStillWins`: no targets function: bob's start cuts alice (today's behaviour; reuse the existing test if one already pins it and name it in the report).
- [ ] **Step 2: Run** the focused tests → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify** focused run, then the full check.
- [ ] **Step 5: Commit** — `feat(failover): try fallback instances when a live channel cannot start on the own provider`

---

### Task 3: Fallback endpoint and channel matching

**Files:**
- Create: `pkg/server/failover_match.go`, `pkg/server/failover_match_test.go`, `pkg/server/failover.go`, `pkg/server/failover_test.go`
- Modify: `pkg/config/config.go` + `cmd/root.go` (`FailoverKey string`, flag `failover-key`, env `FAILOVER_KEY`, help: `Key other StreamShare instances present to borrow a provider connection for a live channel; empty = this instance never lends`), `pkg/server/server.go` (route; `utils.RegisterSecrets(config.FailoverKey)`), `pkg/server/xtream_handlers_stream.go` (`awaitUpstreamStart`)

**Interfaces:**
- Consumes: `session.FailoverIdentityPrefix`, `session.IsFailoverIdentity`, `sessionManager.Slating`, `session.KeyAllInUse`; existing `c.shareUpstream`, `c.multiplexedStream`, `c.liveStreamFiltered`, `lookupAPIChannelName`, the `epgIndex` / `apiChannelIndex` maps with their mutexes (confirm in `m3u_index.go` that both hold live channels only; if not, report it as a concern).
- Produces:
  - Route `GET /api/failover/live?epg=<epg id>&name=<channel name>`, registered next to the share route only when `c.FailoverKey != ""`. Headers: `X-Failover-Key`, `X-Failover-From` (constants `failoverKeyHeader`, `failoverFromHeader`).
  - `func normalizeChannelName(name string) string`: lower case; drop a leading 2-3 letter country tag only when it is followed by `|` or `:` or wrapped in `[]`/`()`; drop the whole-word tokens `uhd 4k fhd hd sd hevc h265 2160p 1080p 720p`; keep only letters and digits, excluding Unicode modifier letters (`unicode.Lm`).
  - `func qualityClass(name string) int`: `uhd`/`4k`/`2160p` → 3; `fhd`/`1080p` → 2; `sd` → 0; otherwise 1 (whole-word tokens, case-insensitive).
  - `func (c *Config) matchFailoverChannel(epgID, name string) (string, bool)`: candidates are the stream IDs whose EPG ID equals `epgID` case-insensitively (when `epgID != ""`); when there are none, the IDs whose normalised name equals the normalised `name` (never when that is empty). Skip IDs with `c.liveStreamFiltered`. Pick the smallest distance to `qualityClass(name)`, then the lower class, then the smaller ID string.
  - `func failoverStatus(uerr *session.UpstreamError) int`: `StatusCode >= 400` → that code; `KeyAllInUse` → 503; `KeyTimeout` → 504; otherwise 502.
  - `func (c *Config) handleFailoverLive(ctx *gin.Context)`: (1) `subtle.ConstantTimeCompare` on the key header, mismatch → 401; (2) the caller's name from the From header, sanitised: keep letters, digits, space, `.`, `_`, `-`, at most 64 runes, empty → `instance`; (3) match, none → 404; (4) `targetURL, _ := c.shareUpstream(id)`; (5) identity `session.FailoverIdentityPrefix + from + "/" + id`, then as `handleShareStream`: `RegisterUser`, `ctx.Set("username", identity)`, `utils.InfoLog("Failover: serving %s for %s", c.streamLabel(id), from)`, `c.multiplexedStream(ctx, targetURL)`.
  - `awaitUpstreamStart`, after `buffer.Ready()`: for a failover identity, when `c.sessionManager.Slating(streamID)` or (`startErr != nil` and the stream serves no slate): remove the client and `ctx.AbortWithStatus(failoverStatus(startErr))`. Other viewers: unchanged.

- [ ] **Step 1: Write the failing tests.**
  - `TestNormalizeChannelName` (table): `"NL | NPO 1 HD"`→`npo1`; `"NPO 1 FHD ⏺ʳᵉᶜ"`→`npo1`; `"NPO 1 4K"`→`npo1`; `"UK: BBC One HD"`→`bbcone`; `"[NL] RTL 4"`→`rtl4`; `"RTL 4 UHD HEVC"`→`rtl4`; `"NL-TV"`→`nltv`; `"Channel 4 HD"`→`channel4`; `"HD"`→`""`; `"★ ★"`→`""`.
  - `TestQualityClass` (table): `"NPO 1 UHD"`→3, `"NPO 1 4K"`→3, `"RTL 4 FHD"`→2, `"X 1080p"`→2, `"NPO 1 HD"`→1, `"NPO 1"`→1, `"NPO 1 SD"`→0.
  - `TestMatchFailoverChannel`: index `10 "NPO 1 HD" epg "NPO1.nl"`, `11 "NPO 1 UHD" epg "npo1.nl"`, `12 "RTL 4 FHD" (no epg)`, `13 "NPO 1 SD" epg "npo1.nl"` filtered out. `("npo1.NL", "NL | NPO 1 4K")`→`11`; `("npo1.nl", "NPO 1 FHD")`→`10` (tie, lower class); `("", "RTL 4 HD")`→`12`; `("unknown.id", "RTL 4")`→`12`; `("", "HD")`→no match; `("", "")`→no match; `("npo1.nl", "NPO 1 SD")`→`10` (13 is filtered).
  - `TestFailoverStatus` (table): 456→456, 404→404, `IN_USE`→503, `TIMEOUT`→504, `UNREACHABLE`→502.
  - Handler tests (build the server the way `live_shares_test.go` does, with a fake provider): no `FailoverKey` → the route answers 404; missing or wrong key → 401; no match → 404; a match streams the fake provider's bytes with status 200 and the session manager has a viewer named `failover:Main/10`; From `"Ma\nin<b>"` becomes `Mainb`; provider answers 456 with slates enabled → the response status is 456 and no slate bytes (Review Focus 1); all slots held by a household stream → 503; a household stream on the matched channel that is showing a slate → the forwarded request gets the failure status, not 200 (Review Focus 1).
- [ ] **Step 2: Run** `... ./pkg/server/ -run 'Failover|NormalizeChannelName|QualityClass'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Verify** focused run, then the full check.
- [ ] **Step 5: Commit** — `feat(failover): endpoint that serves a matching live channel to another instance`

---

### Task 4: Primary wiring, capability flag, docs

**Files:**
- Create: `pkg/server/failover_targets.go`, `pkg/server/failover_targets_test.go`
- Modify: `pkg/config/config.go` + `cmd/root.go` (`FailoverInstances string`, flag `failover-instances`, env `FAILOVER_INSTANCES`, help: `Ordered JSON list of the instances to try when a live channel cannot start here: [{"name":"...","url":"http://...","key":"..."}]`), `pkg/server/server.go` (parse, secrets, `SetFailoverTargets`), `pkg/server/handlers_version.go` (+ its test), `README.md`

**Interfaces:**
- Consumes: `session.FailoverTarget`, `sessionManager.SetFailoverTargets`, `failoverKeyHeader`, `failoverFromHeader`, `normalizeStreamID`, `lookupEPGChannelID`, `lookupAPIChannelName`.
- Produces:
  - `func parseFailoverInstances(raw string) ([]failoverInstance, error)` with `type failoverInstance struct { Name, URL, Key string }` (JSON `name`, `url`, `key`): empty string → none, no error; any entry with an empty field or a URL that is not absolute `http`/`https` makes the whole value invalid.
  - Startup: invalid → one `utils.WarnLog("Failover: FAILOVER_INSTANCES is not valid; failover is off")` (never the value or the parse error text); valid and non-empty → register every key with `utils.RegisterSecrets`, `utils.InfoLog("Failover: fallback instances in order: %s", names)`, `SetFailoverTargets(c.failoverTargets)`. Empty → nothing is set.
  - `func (c *Config) failoverTargets(streamID string) []session.FailoverTarget`: EPG ID and name of `normalizeStreamID(streamID)`; both empty → nil; per instance the URL `<base>/api/failover/live?epg=..&name=..` (keep a path prefix in the base; `url.Values` encoding) and headers key + From = `c.InstanceName`. Base URL and header are prepared once at startup.
  - Version endpoint data gains `"failover": true`.
- README: a section "Failover between instances" (what it does, live only and at channel start, the two variables, that the Suite sets them, the limits: same viewer = today's channel switch, a lent stream is cut when the lender's own household starts watching, a provider that takes longer than 3 s to answer fails over) and the `IN_USE` row in the slate key table next to `NO_RECORDING`.

- [ ] **Step 1: Write the failing tests.**
  - `TestParseFailoverInstances` (table): two valid entries keep their order; `""` → none; `"not json"`, an entry without `key`, and a `ftp://` URL → error.
  - `TestFailoverTargets`: a channel with EPG ID `npo1.nl` and name `NPO 1 HD`, instances `Spare` (`http://127.0.0.1:1/base`) and `Third`: two targets in order; the first URL has path `/base/api/failover/live` and query `epg=npo1.nl`, `name=NPO 1 HD`; headers carry the key and the instance name; a stream ID with a `.ts` suffix resolves the same; an unknown channel → nil.
  - `TestVersionReportsFailover`: the version response has `data.failover == true`.
  - `TestFailoverEndToEnd` (Review Focus 3): two servers in one test, each with its own fake provider; the primary's `FailoverInstances` points at the fallback's `httptest` URL. Primary provider answers 456 → `GET` of the primary's live URL returns the fallback provider's bytes and the primary's stream session has `Via == "Spare"`. Variants: wrong key (fallback answers 401) and a fallback without `FailoverKey` (404) → the primary answers as it does today without fallbacks for a 456 (same status/slate as the no-fallback control case in the same test).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** focused run, then the full check.
- [ ] **Step 5: Commit** — `feat(failover): configure fallback instances and report the capability`

---

### Task 5: "via" in the streams and history API

**Files:**
- Modify: `pkg/database/schema.go`, `pkg/database/stream_history.go`, `pkg/database/stream_history_pg_test.go`, `pkg/session/history.go`, `pkg/session/failover.go` (where `Via` is set), `pkg/server/handlers_streams.go`, `pkg/server/handlers_history.go`, `pkg/server/handlers_dashboard.go`, `pkg/server/handlers_status.go`, `README.md` (API field lists, if they list fields)
- Test: the matching `_test.go` files

**Interfaces:**
- Consumes: `types.StreamSession.Via` (Task 2).
- Produces:
  - Migration next to the existing ones: `ALTER TABLE stream_history ADD COLUMN IF NOT EXISTS via TEXT NOT NULL DEFAULT ''`. `HistoryEntry.Via string`, selected by `GetUserHistory` and `GetRecentHistory`.
  - `func (m *DBManager) SetStreamHistoryVia(streamID, via string) error`: `UPDATE stream_history SET via = $2 WHERE stream_id = $1 AND end_time IS NULL`.
  - Session manager (only when `sm.db != nil`): call it when the pump sets `Via`, and in `recordLiveHistory`'s goroutine after a successful insert when the stream session has a `Via` (covers viewers who join later and inserts that race the pump). Errors are logged, never fatal.
  - JSON: `via` (`omitempty`) on `activeStreamItem` and on both history feed items.
  - Viewer counts `active_viewers` (`handlers_dashboard.go`) and `users_count_active` (`handlers_status.go`) skip failover identities: the viewer is already counted on their own instance.

- [ ] **Step 1: Write the failing tests**: streams API item of a stream session with `Via: "Spare"` has `"via":"Spare"`, and no `via` key without it; history feed and user history items carry `via`; Postgres tests in the style of `stream_history_pg_test.go` (same skip rule): `SetStreamHistoryVia` marks only the open rows of that stream ID, and the two getters return it; a `failover:Main/10` session is not counted in either viewer count while a household session is.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** focused run, then the full check.
- [ ] **Step 5: Commit** — `feat(failover): show which instance served a stream in the streams and history API`

---

### Task 6: Suite server: fallback list, environment, capability lookup

**Files (in `/home/user/stream-share-suite`):**
- Create: `server/src/reconcile/failover.js`, `server/test/instance-failover.test.js`
- Modify: `server/src/schema/instance.js` (field + the error slate help text names `IN_USE`), `server/src/reconcile/instance.js` (`renderInstanceSpec`), `server/src/instanceClient.js`, `server/src/routes/stack.js`, `docs/instances.md`

**Interfaces (produces):**
- Schema field on the instance: `{ key: "fallbacks", envVar: null, type: "fallbacks", label: "Fallback instances", group: "Failover", default: "", help: "When a live channel cannot start on this instance (its provider fails, or all its connections are in use by other viewers), try these instances in order. Each one serves the channel only while it has a free provider connection of its own." }`. Stored like other list fields: one comma-separated string of instance keys. Saving does not validate the keys.
- `failover.js`:
  - `failoverKey(apiKey)`: lower-case hex SHA-256 of `"failover:" + apiKey` (derived, so nothing new is stored or migrated).
  - `peerUrl(key, values)`: VPN on (`isVpnEnabled()`) → `http://127.0.0.1:<port>` (all instances share gluetun's network namespace); off → `http://<instanceContainerName(key, values)>:<port>`. Port as `instanceUrl` reads it.
  - `failoverEnv(key, values, instances)` (`instances` = the rows of `listComponents("instance")`): returns `{}` plus `FAILOVER_INSTANCES` = `JSON.stringify([{ name, url, key }])` for this instance's fallbacks in order (`name` = the peer's `displayName || key`; skip its own key, unknown keys, repeats, and peers without `_apiKey`; omit the variable when the list ends up empty) and `FAILOVER_KEY` = `failoverKey(values._apiKey)` only when another instance lists this key in its fallbacks.
- `renderInstanceSpec` merges `failoverEnv(...)` into the env.
- `fetchFailoverSupport(instance, { timeoutMs })` in `instanceClient.js`: same request as `fetchVersion`, resolves `body.data.failover === true`.
- `GET /failover-support` on the stack router (next to `/versions`): `{ supported: [instance keys] }`, asking all instances in parallel with a 3000 ms timeout each; an instance that fails or does not answer is left out; the route itself always answers 200.
- If the plan/diff output or any log prints environment values, treat the two new variables like `INTERNAL_API_KEY` there.
- `docs/instances.md`: a "Failover" section (what the list does, order matters, live channels only, each instance stays within its own connection limit, a fallback needs a StreamShare version with failover support) and `IN_USE` next to `NO_RECORDING`.

- [ ] **Step 1: Write the failing tests** in `server/test/instance-failover.test.js` (set-up as `instance.test.js` / `helpers.js`):
  - `failoverKey("abc")` is 64 hex characters, stable, and not `"abc"`.
  - Instances `a` (fallbacks `"b,c"`), `b`, `c`: `a`'s env has `FAILOVER_INSTANCES` with `b` then `c`, names from `displayName`, keys equal to `failoverKey` of each peer's `_apiKey`, and no `FAILOVER_KEY`; `b` and `c` have `FAILOVER_KEY` and no `FAILOVER_INSTANCES`.
  - Review Focus 5: fallbacks `"a,ghost,b,b"` on `a` renders only `b`; with no fallbacks anywhere, no instance's env has either variable and `renderInstanceSpec` output equals the output before the field existed (compare against a spec rendered with the field absent).
  - VPN on → peer URL `http://127.0.0.1:<b's port>`; VPN off → `http://<b's container name>:<b's port>`.
  - `GET /failover-support` with a stubbed instance fetch: one instance answering `failover: true`, one answering only a version, one unreachable → `{ supported: [first key] }`, status 200.
- [ ] **Step 2: Run** `node --test "test/*.test.js"` in `server/` → the new file FAILS. **Step 3: Implement.** **Step 4: Verify** all server tests pass.
- [ ] **Step 5: Commit** — `feat(failover): per-instance fallback list wired into the instances' environment`

---

### Task 7: Suite web: fallback picker and "via" display

**Files (in `/home/user/stream-share-suite`):**
- Create: `web/src/lib/failover.js`, `web/test/failover.test.js`, `web/src/components/FallbackPicker.jsx`
- Modify: `web/src/components/SchemaForm.jsx` (render type `fallbacks`, add it to `multiControl`), `web/src/lib/api.js` (if calls go through named helpers), `web/src/pages/Overview.jsx`, `web/src/pages/History.jsx`, `web/src/pages/Leaderboard.jsx`, `web/src/components/ErrorSlateEditor.jsx` (`SYNTHETIC_CODES` gains `"IN_USE"`), plus any Suite server code that copies stream or history fields one by one (follow `epg_channel_id` and `stream_title` from the instance API to the page and add `via` wherever they are copied)

**Interfaces:**
- Consumes: `GET /stack/instances` (`{ instances: [{ key, displayName }] }`), `GET /stack/failover-support` (Task 6), `parseItems` / `joinItems` / `removeItem` from `web/src/lib/listItems.js`, stream-share's `via` field and `failover:<instance name>/<stream id>` viewer names.
- Produces:
  - `moveItem(items, index, delta)`: a new array with the item moved by `delta` (-1/+1); out of range → the same order.
  - `fallbackChoices(instances, selfKey, selected, supported)`: the instances that can still be added: not `selfKey`, not already selected, and in `supported` (an array of keys; `null` while it is loading → none).
  - `lentTo(name)`: for `failover:Main/123` → `"Main"`; anything else → `null`.
  - `FallbackPicker({ value, onChange, componentKey })`: loads the two endpoints itself (as `ChannelSearchInput` loads its data); shows the selected instances in order by display name with move up / move down / remove buttons (real `<button>`s with `aria-label`s), and a select to add one of `fallbackChoices`. A selected key with no such instance shows as the raw key with "(removed)"; a selected instance not in `supported` stays listed with the note "needs a newer StreamShare version". Styling follows `ListInput.jsx`.
  - Overview: next to the stream title, `via <instance name>` when `s.via` is set; a viewer whose id/name is a failover identity shows as `Lent to <name>` and is not offered for aliasing. History and Leaderboard: the same label for such usernames; History shows `via <name>` when `e.via` is set.

- [ ] **Step 1: Write the failing tests** in `web/test/failover.test.js`: `moveItem(["a","b","c"], 0, 1)` → `["b","a","c"]`; `moveItem(["a","b"], 0, -1)` and `moveItem(["a","b"], 1, 1)` → unchanged order; `fallbackChoices` leaves out self, selected and unsupported keys, and returns `[]` for `supported === null`; `lentTo("failover:Main/123")` → `"Main"`, `lentTo("failover:Living room/12.ts")` → `"Living room"`, `lentTo("share:abc")` and `lentTo("192.168.1.5")` → `null`.
- [ ] **Step 2: Run** `node --test "test/*.test.js"` in `web/` → FAIL. **Step 3: Implement.**
- [ ] **Step 4: Verify** web tests pass, server tests still pass, `npm run build` in `web/` succeeds.
- [ ] **Step 5: Commit** — `feat(failover): pick fallback instances and show which instance served a stream`

---

## After the last task (controller)

- Whole-branch review of each repo on the most capable model; one fix round.
- Screenshot check of the instance page's Failover group and an Overview row with `via` (local Suite dev build with stubbed data), light and dark.
- Leak check on both diffs and the PR texts (no real hostnames, logins or keys), then push both branches. Retitle draft PR #72 to the feature; open a draft Suite PR that names the minimum StreamShare version.
- Not verifiable here, to tell the owner: the VPN-on peer address (`127.0.0.1:<port>` inside gluetun's namespace) on a real stack; a real two-instance run (needs the owner's instances; ask before using the main provider).
