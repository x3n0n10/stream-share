# Failover: show the fallback's channel name — design

A small change across two repos: stream-share (this repo) and stream-share-suite.

## Why
When a live stream fails over, the Suite says "Using B's connection" (Overview)
and "Used B's connection" (History). The fallback instance picks its own best
match for the channel (`matchFailoverChannel`: guide ID first, then name, then
the closest quality class), so what it plays can differ from the original —
e.g. an FHD variant where the own provider had HD. The user wants to see which
channel the fallback is actually playing, live and afterwards in History.

## Goals / non-goals
- Goal: Overview reads "Showing B's <channel> instead"; History reads
  "Used B's <channel>".
- Goal: mixed versions keep working. A fallback that doesn't send the name, or
  history rows from before this change, show today's text.
- Non-goal: changing how the fallback picks its channel.

## Approaches
1. **The fallback names its channel in a response header (picked).** No extra
   request; old fallbacks simply don't send it.
2. The calling instance asks the fallback afterwards — an extra round trip and
   endpoint for the same answer.
3. The calling instance guesses from its own channel list — wrong exactly when
   it matters (the fallback picked a different variant).

## Design
**stream-share, fallback side** (`handleFailoverLive`): before streaming, set
`X-Failover-Channel` to the matched channel's name (`lookupAPIChannelName(id)`),
URL-encoded (`url.QueryEscape`) because channel names can hold non-ASCII.
Omitted when the name is unknown.

**stream-share, calling side** (`failoverAttempt`): read and decode the header
from the fallback's response. Store it next to `Via` on the stream session by
extending the existing `SetVia`/`GetVia` (`pkg/types/types.go`) to take and
return both values, so the two are set and read together and can't drift apart. A header that fails to decode is
treated as absent. Extend `recordHistoryVia` and
`SetStreamHistoryVia` to carry the channel too, rather than adding parallel
functions. Both callers of `recordHistoryVia` pass it (`failover.go` and
`history.go`, which re-applies `via` when a history row opens after failover).
- `activeStreamItem` gains `via_channel` (`omitempty`) for `/status` and
  `/api/streams`.
- `stream_history` gains `via_channel TEXT NOT NULL DEFAULT ''`, added the same
  way as `via` (`ALTER TABLE … ADD COLUMN IF NOT EXISTS`). The update that sets
  `via` on open rows sets `via_channel` with it. History API rows gain
  `via_channel` (`omitempty`).

**stream-share-suite:**
- Overview: with `via_channel`, "Showing {via}'s {via_channel} instead";
  without it, "Using {via}'s connection" as today.
- History: with `via_channel`, "Used {via}'s {via_channel}"; without it, as
  today. The History search also matches `via_channel` (my call: it already
  matches `via`).
- In the same Suite PR (separate commit, unrelated fix): when a container is
  recreated after an image pull, the log reason reads
  "configuration changed (X -> X)". `applyPlan` in
  `server/src/reconcile/reconciler.js` should use `plan.reason` when the plan
  has one ("A newer image was pulled").

## Left to the builder
Header constant name next to the other `failover*Header` constants; test
shape, following the existing failover and history tests.
