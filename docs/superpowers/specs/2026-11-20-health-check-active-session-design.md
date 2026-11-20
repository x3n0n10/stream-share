# Health check must not hijack an active session

## Intent

A provider health probe (scheduled, at startup, or forced via
`/api/internal/health`) dials the provider directly through
`SessionManager.ProbeUpstream`, bypassing the provider slot arbiter. On an
account limited to one connection the provider cuts the oldest connection, so
a viewer's stream is destroyed and restarts through the slate/retry path.

The probe exists to answer "is our egress IP blocked?". A stream that is
receiving video from our own provider already answers that, better than the
probe does. So: use the live evidence when there is some, and probe only when
the provider is idle, never at a viewer's expense.

## Goals

- A health check never interrupts a stream, download, or other provider connection.
- An active video session on the **own** provider counts as **healthy**.
- Fallback and error slates do **not** count as healthy. When the own provider
  is failing on a live stream, the watchdog sees that failure: **blocked** when
  the code is in `HEALTHCHECK_BLOCKED_CODES`, otherwise **error**.
- Fallback because of a local connection limit (`KeyAllInUse`: our own slot was
  taken by the household) is not a provider failure and does not report blocked.

## Non-goals

- No continuous/passive health tracking from every dial (approach B below).
- No change to the watchdog contract: same endpoints, statuses, HTTP codes, throttling.
- No change to how streams pick the own provider vs. fallback.

## Anti-goals

- Reporting healthy while a stream sits on a 456 slate or on fallback after a 456.
- Any path where the probe preempts or cuts an existing connection.

## Approaches

- **A. Evidence first, probe only when idle (chosen).** At each check, inspect
  the live streams; derive the result from them when they say something;
  otherwise probe through the arbiter without preempting. Contained to the
  health check plus recording why a stream fell back.
- **B. Passive health.** Every own-provider dial outcome updates the cache;
  the probe runs only when nothing plays. Instant block detection, but touches
  the streaming hot paths and needs result-expiry rules. Rejected: larger and
  more invasive than the problem needs.
- **C. Skip the probe while anything plays.** Rejected: reports healthy while a
  stream is slating on a 456, exactly the case the watchdog must see.

## Design

When `probeHealth` would run a real probe (its throttling and in-flight logic
stay as they are), `runProviderProbe` decides in this order:

1. **Healthy stream present → healthy, no dial.** Any stream buffer that holds
   the own provider slot (`ownSlot`), is past its start (`ready` with no start
   error) and is not `slating`. Lent (forwarded) streams count: they are served
   by the own provider. Detail names the evidence, e.g. "own provider serving N
   active stream(s)".
2. **Failing stream present → its error.** A stream that is `slating`, or that is
   being served via a fallback instance after the own provider returned an error
   (not `KeyAllInUse`). Its last own-provider `UpstreamError` is classified like
   a probe result: blocked when the status is in the blocked list, else error.
3. **Otherwise → probe, politely.** Acquire a **Background**-class arbiter lease
   (never preempts anything), bounded by the probe timeout, then dial as today and
   release. If no slot frees up, a holder is using the own provider, so report
   healthy ("provider connection in use"), since failing holders were already
   caught by step 2.

Precedence: step 1 wins over step 2. A working connection proves the egress IP
is not blocked; a slate next to it is channel-specific (e.g. a removed channel).

### Recording the fallback reason

`failoverAttempt` already knows `ownErr` when it serves via a fallback. Store it
on the `StreamBuffer` (cleared when the own provider serves again, including on
a slate-retry recovery). Slating streams likewise keep their latest
`UpstreamError` (the initial one, updated by each failed retry).

### 456 ambiguity

`dialOwn` retries 456/461 as a possible connection-limit answer, while 456 is
also the default blocked code. A 456 that still ends in fallback or a slate is
reported as **blocked** when listed in `HEALTHCHECK_BLOCKED_CODES`: a VPN
reconnect is cheap, a missed block is not.

## Left to the builder

- The exact `SessionManager` method shape exposing this evidence to the server
  package (it must do no I/O and hold `streamLock` only briefly).
- Log wording, and tests covering: healthy stream short-circuits the dial;
  slating stream with 456 → blocked; fallback after 456 → blocked; fallback
  after `KeyAllInUse` → probe/healthy, not blocked; probe never preempts a held lease.
