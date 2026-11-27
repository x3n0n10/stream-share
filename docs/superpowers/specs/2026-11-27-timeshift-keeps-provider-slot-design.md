# First come, first served provider slots, timeshift and pause — design

## Intent

StreamShare serves viewers first come, first served: whoever holds a provider
connection keeps it, and a later viewer gets a fallback session (when
failover is on) or the "in use" error. Today three things break that:

1. **Newest wins.** Between two watched streams, a new request takes the
   slot of the oldest one (`provider.Fresh` in `Arbiter.AcquireClass`).
2. **Timeshift is not watching.** A viewer reading the local catchup buffer
   is not a client of the live stream that records it, so that stream is
   "background" and anyone takes its slot, even a guest on a share link. Its
   recording stops, and the timeshifting viewer stalls or loops at the end of
   the buffer (the hand-over to live then takes the slot back, and the two
   viewers keep taking it from each other).
3. **Pause is not recognised, and recording stops during it.** TiviMate
   pauses by reading on until its own buffer is full (about a minute), then
   stopping reading, then closing the connection; on resume it plays its own
   buffer and asks the server for a timeshift from where that ends. With one
   viewer, the pump waits for that viewer forever (`deliver` with
   `sole == true` never gives up), and the same loop writes the catchup
   buffer, so recording stops while TiviMate does not read. When the
   connection finally closes, the stream is background during the pause
   grace, so anyone can take it.

## Goals

- First come, first served for every watched stream: a request that finds
  every slot watched is refused at once (live: fallback when failover is
  on, otherwise the existing `IN_USE` slate, "All provider connections are
  in use by other viewers."; movies and series: HTTP 503).
- A viewer who switches channel still gets the new channel: their own old
  slot counts as theirs.
- A viewer reading a channel's catchup buffer counts as watching it.
- A paused household viewer counts as watching for the pause grace: their
  stream keeps its slot and keeps recording, with no gap while TiviMate stops
  reading.
- A viewer who stops watching frees the slot at once for anyone who watches.
- The pause grace is configurable in the Suite, default 5 minutes (as now).

## Non-goals

- Catchup the provider serves itself (channels it flags as archived): those
  requests go to the provider outside the slot arbiter. Unchanged.
- Unwatched background work (VOD cache downloads nobody watches, health
  probes): still preempted by anything watched, and never preempting
  anything. Unchanged.
- Changing how long the catchup buffer file outlives a stopped stream (60 s).
- Protecting a paused guest (share link): their pause stays unprotected, as
  today.

## Anti-goals

- A viewer getting "in use" from their own previous stream when switching.
- A request waiting forever for a slot it can never get.
- A pause that blocks everyone else for longer than the configured grace.
- A rewind from live failing because the live stream stopped: TiviMate
  closes the live connection before it opens the timeshift request.

## How it works today (relevant parts)

- `provider.Arbiter` (`pkg/provider/arbiter.go`): classes Background < Share
  < Foreground. A request preempts a lower holder; in `Fresh` mode a
  Foreground request also preempts the oldest Foreground holder
  (`oldestHolderOfClassLocked`). `WouldGrant(c)` is true for a free slot or a
  lower holder. `Fresh` is used by live streams without failover
  (`pkg/session/manager.go`, `AcquireClass(... provider.Fresh)`) and by VOD
  cache downloads for their first acquire (`pkg/server/vod_cache.go`).
- `acquirePolite` (`pkg/session/failover.go`), used by failover and forwarded
  streams: `IN_USE` at once when `WouldGrant` is false, except that it waits
  up to `slotWait` when `buffer.takeover` is set (the viewer's own previous
  stream held the slot); it acquires with `Resume`.
- `live_shares.go` already answers 503 after a `WouldGrant` pre-check.
- `StreamBuffer.audienceClass()`: Foreground while a household client
  watches, Share for guests only, Background with no clients. The arbiter
  calls it under `a.mu` for every holder and waiter on every decision and
  every 250 ms poll.
- `RemoveClient`: when the last client leaves a stream with a disk buffer, it
  calls `schedulePendingStop` (pause grace, `CATCHUP_PAUSE_GRACE_MINUTES`,
  default 5; the stream stays Background meanwhile); otherwise `stopStream`.
- `deliver`: a sole client blocks the pump until it reads; with several
  clients a client that does not read for `clientStallTimeout` (30 s) is
  closed and removed. The multi-client path allocates a timer per chunk.
- `xtreamStreamTimeshift` → `serveFromCatchupBuffer`
  (`pkg/server/xtream_handlers_stream.go`) reads the buffer;
  `NotifyCatchupActivity` cancels a pending stop, and nothing schedules a new
  one when the reader leaves, so the stream then records with no viewers
  until someone switches channel.

## Approaches

1. **First come, first served everywhere, plus "watching" covering timeshift
   and pause (chosen).** Remove "newest wins" from the arbiter, send every
   live start through the existing polite acquire, and make
   `audienceClass()` count buffer readers and paused streams as Foreground.
   One rule for everything, and less code than an exception.
2. **Keep "newest wins", protect timeshift streams with a mark the arbiter
   respects.** Narrower, but a second priority system next to classes, and
   live-vs-live stays "newest wins", against the principle.
3. **A new class above Foreground for timeshift.** Still needs a fail-fast,
   and a Timeshift-class stream waiting for its slot would preempt live
   viewers.

Making buffer readers real `StreamBuffer` clients is the wrong mechanism:
`fanOut` drops clients that do not drain their channel, and a disk reader
never does.

## Design

### Arbiter and live starts: first come, first served

- Delete the `mode == Fresh` same-class branch in `AcquireClass`, and
  `oldestHolderOfClassLocked`. A request then only preempts lower classes.
  With `Fresh` gone from every caller (below), remove the `Fresh` mode;
  `Resume` is the only one left, and the `Mode` parameter can go if nothing
  else needs it.
- The non-failover live path (`manager.go`, where it calls `AcquireClass`
  with `Fresh`) calls `acquirePolite` instead, as failover and forwarded
  streams already do. That gives, with no new code:
  - `IN_USE` at once when every slot is watched (`WouldGrant` false), so the
    viewer gets the slate or the failure as today;
  - the viewer's own switch waits up to `slotWait` for their old slot
    (`buffer.takeover`, already computed in `RequestStream`), so they never
    get "in use" from themselves.
  - Note: `acquirePolite` also lets guest-class requests wait for a slot the
    household would get. That is today's behaviour for lent streams and is
    fine for guests on the normal path too.

### Movies and series: "in use" as HTTP 503

- A watched movie or episode is Foreground. Before its first acquire,
  `vod_cache.go` checks `WouldGrant(Foreground)`, as `live_shares.go` does.
  When it is false and no bytes were sent yet, the viewer's request gets
  **HTTP 503** with a short plain-text body ("All provider connections are
  in use by other viewers."). The owner chose this over a slate (movie
  players handle a fake video badly) and over keeping "newest wins" for VOD.
- After the check it acquires with `Resume`, as every other caller. A slot
  taken between the check and the acquire makes the download wait instead
  of answering 503; that window is small and accepted.
- An unwatched download stays Background: it waits, and is preempted by
  anything watched, as today.

### Timeshift counts as watching

- `SessionManager.BeginCatchupRead(id) (end func())` replaces
  `NotifyCatchupActivity`. It finds the live `StreamBuffer` with the bare-id
  matching of `ReleasePausedStream`, cancels a pending stop
  (`cancelPendingStop`), and counts the reader per viewer on the buffer:
  a `map[username]int` under its own small leaf lock (taken after `a.mu`,
  like `clientsLock` is today). "Any reader" is `len(map) > 0`.
- Readers count per viewer because `RequestStream`'s switch logic must keep
  a stream alive for *another* viewer's reader (timeshift counts as
  watching), but not for the switching viewer's own reader (anti-goal:
  "in use" from yourself). So `watchedBeyond` also counts readers of other
  viewers.
- `end` decrements and drops the entry at zero. When no readers and no
  clients are left, it calls `schedulePendingStop` under `RemoveClient`'s
  condition, as a pause (TiviMate may resume).
- `xtreamStreamTimeshift` calls it before `serveFromCatchupBuffer` and ends
  it when the response finishes (client gone, or handed over to live, where
  the viewer becomes an ordinary client).
- Readers of a stopped stream (`buffer.active == false`) do not count.

### Pause: detect it, keep recording, keep the slot

- **Never block the pump on a cached channel's viewer.** For a stream with a
  disk buffer, `deliver` first tries a non-blocking send; only when the
  client's channel is full does it arm a timer, with a short pause stall
  timeout (proposal: 5 s) for every client, sole or not (otherwise one
  pausing TiviMate freezes the shared pump and the recording for everyone).
  When it fires, the client is closed. Trying the non-blocking send first
  also removes today's per-chunk timer from the multi-client path. Streams
  without a disk buffer keep today's behaviour.
- **Remember stalls.** `deliver` stores `buffer.lastStallAt` (an atomic)
  when a client's channel is full: on the stall timeout, or when the
  client's connection closes while its channel is full.
- **Tell pause from stop.** In `RemoveClient`, when the last household
  client leaves a stream with a disk buffer:
  - `lastStallAt` falls within the last pause grace: **pause**. Set
    `buffer.paused` (an atomic bool), then `schedulePendingStop` as today.
    This also covers a viewer who paused while someone else left last.
  - otherwise: **stop**. Exactly as today: `schedulePendingStop`, stream
    Background. Anyone watching can take the slot at once, and a rewind
    from live still finds the stream recording.
  - A stream whose last clients were guests takes the stop path (a paused
    guest is not protected).
- `buffer.paused` is cleared when the pending stop is cancelled or the
  stream stops.
- `audienceClass()` returns Foreground, before taking `clientsLock`, when
  `paused` is set or the stream has readers. Both reads take no global lock
  (never `streamLock`, which would sit inside `a.mu`).
- **Resume.** TiviMate asks for a timeshift from where its own buffer ends:
  a buffer read, so `BeginCatchupRead` cancels the pause grace and the stream
  keeps its slot. A plain live request for the same channel joins the
  stream, as today.
- This rests on how TiviMate pauses (as described by the owner). Before
  relying on the stall rule, the builder confirms it once against a real
  TiviMate on the dev suite.

### Pause grace in the Suite

- `stream-share-suite`: add a field to the instance schema
  (`server/src/schema/instance.js`, Caching group, next to
  `catchupDurationHours`): `catchupPauseGraceMinutes` →
  `CATCHUP_PAUSE_GRACE_MINUTES`, default `5`, shown only when catchup is on.
  Help text: how long a paused channel keeps its provider connection and
  keeps recording; while paused, other viewers get a fallback or "in use".
- StreamShare already reads `CATCHUP_PAUSE_GRACE_MINUTES` (default 5); no
  change there.

## Left to the builder

- Names, and whether the `Mode` parameter survives.
- The exact pause stall timeout (5 s is a proposal), and whether it becomes
  a setting (not required).
- Log wording for: refused with `IN_USE`/503, pause detected.
- Tests:
  - arbiter: a watched holder is never preempted by a watched request; lower
    classes still are;
  - session: a second viewer on another channel gets `IN_USE` (or the
    fallback) while the first watches, timeshifts or is paused; own switch
    waits and gets the slot; another viewer's switch keeps a timeshift
    reader's stream alive; stopped streams' readers and pause do not count;
    a stalled client on a cached channel is closed and recording continues;
    pause vs. stop; pause grace after the last reader;
  - VOD: a movie request gets 503 while every slot is watched;
  - suite: the schema field renders the env var only when catchup is on.
