# Timeshift keeps the provider slot — design

## Intent

StreamShare serves viewers first come, first served. A viewer who is
timeshifting (watching a channel from the local catchup buffer) is watching,
but today StreamShare does not count them as such: the live stream that
records the buffer has no clients, so its provider slot has the lowest
priority (background). On a provider that allows one connection, any other
viewer who starts a different channel takes that slot. Even a guest on a share
link does. The recording stops, and the timeshifting viewer runs into trouble
when they reach the end of the buffer:

- if the stream is still waiting for its slot, the reader waits at the gap and
  the player stalls (TiviMate then re-requests);
- if the stream was stopped, the reader hands over to live, and that live
  request takes the slot back from the other viewer, so the two can keep
  taking it from each other.

## Goals

- While a viewer reads a channel's catchup buffer, that channel's live stream
  keeps its provider slot, and its recording keeps running.
- Another viewer who starts a different channel and finds no free slot:
  - gets a fallback session when failover is enabled;
  - otherwise gets the existing "in use" error slate (`IN_USE`, "All provider
    connections are in use by other viewers.").
- When the last buffer reader leaves, the stream gets the normal pause grace
  and then stops, so it no longer records forever with no viewers.

## Non-goals

- Changing "newest request wins" between two viewers who both watch live.
  The new protection applies only to timeshift.
- Catchup the provider serves itself (channels the provider flags as
  archived). Those requests go straight to the provider, outside the slot
  arbiter.
- Protecting a paused stream. During the pause grace (TiviMate has
  disconnected and nobody reads the buffer), the stream stays background, as
  today.

## Anti-goals

- The timeshifting viewer losing their own ability to switch channels: their
  own channel switch still stops the old stream, as today.
- A request waiting forever for a protected slot. It must fail at once.

## How it works today (relevant parts)

- `StreamBuffer.audienceClass()` (`pkg/session/manager.go`) gives a stream
  Foreground while a household client watches, Share for guests only, and
  Background with no clients. Buffer readers are not clients.
- Live streams without failover acquire with `provider.Fresh`. A Fresh
  Foreground request preempts a lower holder, or else the oldest Foreground
  holder (`Arbiter.AcquireClass`, `pkg/provider/arbiter.go`).
- Failover streams first call `acquirePolite`
  (`pkg/session/failover.go`), which refuses at once with `IN_USE` when
  `WouldGrant(class)` is false, and then try the fallbacks. If the
  timeshifting stream counts as Foreground, this already gives the wanted
  behaviour.
- `xtreamStreamTimeshift` → `serveFromCatchupBuffer`
  (`pkg/server/xtream_handlers_stream.go`) reads the buffer.
  `NotifyCatchupActivity` cancels the pending pause-grace stop, and nothing
  schedules a new one once the reader leaves.

## Approaches

1. **Count buffer readers and protect their stream (chosen).** A buffer
   reader registers on the live stream. While at least one reads, the stream
   counts as Foreground, and the arbiter never preempts it for a request of
   the same class. A Fresh request that could only get a slot that way fails
   at once with `IN_USE`. Keeps "newest wins" for live-only cases.
2. **A new arbiter class above Foreground for timeshift.** The arbiter
   would skip it without new code, but a request that cannot preempt it would
   still wait forever, so it needs the same fail-fast. Also, a Timeshift-class
   stream waiting for its slot back would preempt live Foreground holders,
   which is a new behaviour nobody asked for.

Counting readers without the protection is not enough: Fresh would still
preempt the oldest Foreground holder, which is the timeshifting viewer.
Making readers real `StreamBuffer` clients is the wrong mechanism too:
`fanOut` drops clients that don't drain their channel, and a disk reader
never does.

The owner picked 1: it fits StreamShare's first-come-first-served principle.

## Design

- **Reader registration (session).** `SessionManager` gets
  `BeginCatchupRead(id) (end func())`, built from what exists: the bare-id
  matching of `ReleasePausedStream` (with or without extension) and
  `cancelPendingStop`. It replaces `NotifyCatchupActivity`. It increments an
  `atomic.Int32` reader count on the live `StreamBuffer`. `end` decrements
  it; at zero, with no clients left, it calls `schedulePendingStop` under the
  same condition `RemoveClient` uses (`diskBuffer != nil && pauseGrace > 0`).
- **Readers only count while the stream runs.** Once `stopStream` has run
  (`buffer.active == false`), the reader count is ignored. Otherwise the
  timeshifting viewer's own channel switch races: `RequestStream` stops the
  old stream, but the reader count only drops when the timeshift response
  ends, and TiviMate may open the new channel first.
- **Priority.** `audienceClass()` checks the reader count first and returns
  Foreground before it takes `clientsLock`.
- **Protection (arbiter).** A holder is protected when its stream has
  readers. This is not a second callback: the lease's one callback returns
  both values, e.g. `func() (Class, bool)`, so each decision makes one call.
  `oldestHolderOfClassLocked` skips protected holders. `lowerHolderLocked`
  needs no change: a protected stream is Foreground, never "lower".
- **Fail fast.** In Fresh mode, a Foreground request with no free slot that
  finds no victim (lower or same-class) returns at once with a
  `provider.ErrInUse` instead of waiting. A Share request still waits behind
  Foreground holders, as today. The non-failover live path maps
  `ErrInUse` to `KeyAllInUse` where it calls `AcquireClass`, without going
  through `classifyUpstream`. It then marks the stream ready with that error,
  which shows the existing slate (when slates are on) or fails the request as
  today.
- **Failover.** No change beyond the class: `acquirePolite` sees
  `WouldGrant(Foreground) == false` and moves on to the fallbacks.
- **Handler.** `xtreamStreamTimeshift` calls the begin function before
  `serveFromCatchupBuffer` and ends it when the response finishes (client
  gone or handed over to live). The hand-over to live goes through the normal
  stream path, so that viewer becomes an ordinary client.

## Left to the builder

- Exact names, and the shape of the combined class/protected callback.
- Log wording for "refused: slot protected by a timeshifting viewer".
- Tests: arbiter (Fresh does not preempt a protected holder and fails fast;
  it still preempts an unprotected one), session (class while reading; pause
  grace after the last reader; the viewer's own switch while reading still
  works), and a handler-level test that a second
  channel gets `IN_USE` or the fallback while the first one is timeshifting.
