# Timeshift in UTC (player time adjustment always +0) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Catchup lands on the right moment with the player's time adjustment left at +0, all year round (no change at daylight-saving switches).

**Design (approved by the owner in chat, 2026-11-10/11):**
- StreamShare's Xtream login keeps reporting `"timezone": "UTC"` (`xtream_handlers_api.go`), so players (TiviMate) send timeshift start times in UTC (`YYYY-MM-DD:HH-MM`) when their adjustment is +0.
- Root cause found on the owner's instance: the local catchup buffer parses the start time with `time.Local` (`parseTimeshiftStart`, driven by `TZ`), so with `TZ=Europe/Amsterdam` a UTC time is read as Dutch time → off by 1 h (winter) / 2 h (summer); the owner compensated with +1/+2 in TiviMate. Their provider reports `server_timezone: Africa/Dakar` (UTC) and its EPG is correct UTC.
- Fix: (1) the local buffer reads the start time as UTC; (2) provider catchup converts the UTC start time to the provider's own time zone (from its login `server_info.timezone`, already cached as `AccountInfo.ServerTimezone`), DST included — a no-op for UTC providers like the owner's, correct for e.g. Europe/Berlin providers; unknown/empty zone → no conversion (UTC). Unix-timestamp start values pass through unchanged on both paths. Time-zone data embedded via `time/tzdata` (Alpine image has none).
- The startup warning telling operators to set `TZ` for catchup goes; README says to leave the player's time adjustment at +0.

**Tech Stack:** Go 1.23, stdlib (`time`, `time/tzdata`).

## Global Constraints

- Branch `fix/timeshift-utc` from master `30c95ca`. Commit per task; do not push (the controller pushes and opens the PR).
- Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`; tests need `CACHE_FOLDER=$(mktemp -d)`.
- Suite: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean; `gofmt -l pkg cmd tools` empty. CI runs golangci-lint (errcheck, staticcheck).
- No new dependencies or settings. House style: `interface{}`, GPLv3 header (copy from `pkg/server/helpers.go`) on new files, comments say why.
- Ponytail: smallest complete change; reuse existing helpers; validation at trust boundaries stays (`isValidStreamID` checks at the top of `xtreamStreamTimeshift` remain first).
- Logs never contain credentials.

## Review Focus

1. The night of a DST change (e.g. 2026-10-25 / 2026-03-29 in Europe/Berlin): a UTC start inside the repeated/skipped hour converts to the right provider wall time. Test in Task 2.
2. A provider time zone string Go does not know (`"GMT+1"`, `""`, garbage): no crash, no conversion, one warning. Test in Task 2.
3. The provider info cache not yet filled at the first catchup request after startup: no blocking provider call on the request path; falls back to no conversion. Test in Task 2.
4. Unix-timestamp start values (some apps) keep working on both paths. Tests in Tasks 1 and 2.
5. A start value the local buffer cannot parse still answers 400 as today. Existing test or Task 1.

---

### Task 1: Local catchup buffer reads the start time as UTC

**Files:**
- Modify: `pkg/server/xtream_handlers_stream.go` (`parseTimeshiftStart` and its doc comment), `pkg/server/server.go` (delete the `TZ env var is not set` startup warning and any code that only feeds it), `README.md` (catchup section: leave the player's time adjustment at +0; `TZ` is not needed for catchup)
- Test: `pkg/server/xtream_handlers_stream_test.go` (or the existing catchup test file)

**Interfaces:**
- `parseTimeshiftStart(start string) (time.Time, error)`: unix integer → `time.Unix(n, 0)` (unchanged); otherwise `time.ParseInLocation("2006-01-02:15-04", start, time.UTC)`.

- [ ] **Step 1: Write the failing test** `TestParseTimeshiftStartIsUTC`: with `time.Local` set to `Europe/Amsterdam` for the test (save/restore), `"2026-11-10:19-00"` parses to `2026-11-10T19:00:00Z`; `"1762801200"` parses to `time.Unix(1762801200, 0)`; `"bad"` errors.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** (+ delete the warning, README). **Step 4: Verify** focused, full suite, vet, gofmt.
- [ ] **Step 5: Commit** — `fix(catchup): read timeshift start times as UTC, matching the login's timezone`

---

### Task 2: Provider catchup in the provider's own time zone

**Files:**
- Create: `pkg/server/timeshift_tz.go` (GPL header)
- Modify: `pkg/server/xtream_handlers_stream.go` (proxy branch of `xtreamStreamTimeshift` uses the converted start)
- Test: `pkg/server/timeshift_tz_test.go`

**Interfaces:**
- `import _ "time/tzdata"` in `timeshift_tz.go` (comment: the Alpine image ships no zoneinfo).
- `func (c *Config) providerLocation() *time.Location` — reads the cached `c.providerInfo` snapshot under its mutex (never triggers a provider request); `nil` providerInfo, no info yet, or empty `ServerTimezone` → `time.UTC`; otherwise `time.LoadLocation(name)`, memoised per name; on error → `time.UTC` and `utils.WarnLog("Catchup: provider timezone %q unknown; treating provider catchup times as UTC", name)` once per name. Log `utils.InfoLog("Catchup: provider timezone %s", name)` once per newly resolved name.
- `func providerTimeshiftStart(start string, loc *time.Location) string` — unix integer → unchanged; parses as `2006-01-02:15-04` in UTC and formats the same layout in `loc`; unparseable → unchanged.
- Proxy branch: `c.providerStreamURL("timeshift", duration, providerTimeshiftStart(start, c.providerLocation()), idRaw)`. The local-buffer branch is untouched (Task 1).

- [ ] **Step 1: Write the failing tests**: `TestProviderTimeshiftStart` — UTC loc: `"2026-11-10:19-00"` unchanged; `Europe/Berlin`: winter `"2026-11-10:19-00"` → `"2026-11-10:20-00"`, summer `"2026-07-10:19-00"` → `"2026-07-10:21-00"`, DST-end night `"2026-10-25:00-30"` → `"2026-10-25:02-30"` and `"2026-10-25:01-30"` → `"2026-10-25:02-30"` (Review Focus 1: both map to 02:30 local, CEST then CET), DST-start `"2026-03-29:01-30"` → `"2026-03-29:03-30"`; unix `"1762801200"` unchanged; `"garbage"` unchanged. `TestProviderLocation` — nil providerInfo → UTC; info with `ServerTimezone: "Africa/Dakar"` → that zone; `"GMT+1"` and `""` → UTC without panic (Review Focus 2); empty cache → UTC and no provider request (Review Focus 3: use a providerInfo with no info and assert no HTTP call to a fake provider). `TestTimeshiftProxyConvertsStart` — a fake provider records the path; provider timezone `Europe/Berlin` cached; a `/timeshift/<u>/<p>/60/2026-11-10:19-00/42.ts` request with catchup proxying reaches the provider with `2026-11-10:20-00`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Verify** focused, full suite, vet, gofmt.
- [ ] **Step 5: Commit** — `fix(catchup): send provider catchup times in the provider's own time zone`

---

## After the last task (controller)

- Whole-branch review; push; open the PR; owner tests TiviMate at +0 (one channel with provider catchup, one with the local buffer) and merges.
