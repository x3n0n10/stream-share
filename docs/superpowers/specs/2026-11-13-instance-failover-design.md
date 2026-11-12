# Live channel failover between StreamShare instances — design

Status: approved in conversation with the owner, 2026-11-13 (including the four
priority/login decisions from the builder check). Spans two repos:
stream-share (the failover itself) and stream-share-suite (setup and display).

## Intent

A household runs several StreamShare instances under one Suite, each on its own
provider account, and most accounts allow one connection. Today a viewer who
wants a second channel while the primary account's connection is taken, or whose
provider fails, has to keep several playlists in the player app and switch by
hand.

The owner wants one playlist in the app: the primary instance. When a channel
cannot be delivered there, StreamShare hands it to another instance that carries
the same channel, and the viewer should hardly notice.

Typical moment: someone in the household watches channel A on the primary; a
second person starts channel B on the same playlist. The primary's only provider
connection is in use, so B is served by a fallback instance's provider instead.

## Goals

- Live channels, at channel start: if the primary cannot deliver, try the
  configured fallback instances in order and stream the first that works.
- One playlist and one channel list for the viewer (the primary's).
- Every instance stays within its own provider account's connection limit. No
  connection multiplexing or other tricks: this only uses capacity the household
  already pays for.
- The Suite sets the chain up; nobody types addresses or credentials.
- The Suite's overview and history show when a stream came from a fallback
  (e.g. "NPO 1 via <instance name>"). The player app is not told.

## Non-goals

- Catchup (timeshift) and VOD. Recordings and catalogues differ per provider.
- Switching while a stream plays (a provider dropping mid-viewing). Only channel
  start fails over; a running stream behaves as today.
- Connection multiplexing / time-slicing one provider connection across
  channels. Considered and rejected by the owner: panels count a just-closed
  connection for seconds, rapid reconnects risk the account, and it would get
  more streams out of a subscription than it allows.
- Channels that exist only on a fallback appearing in the primary's list.
- Telling the player app anything.

## Anti-goals (would make it a failure even if it technically works)

- A fallback's error slate (HTTP 200, video) mistaken for a working stream, so
  the viewer gets a slate while another instance could have served the channel.
- Requests bouncing between instances (A → B → A).
- An instance using more provider connections than its account allows.
- Viewer or service credentials, or instance addresses, leaking into logs.
- A noticeable memory increase (the fork targets ~100 MB per instance in disk
  catalogue mode; catalogues run to 20,000+ channels per provider).

## Behaviour

### Who tries what

The primary (the instance the viewer's app uses) owns the attempt. It tries, in
order: itself, then each fallback in the order the Suite configured. Fallbacks
never forward further (see "Forward marker"). With no fallbacks configured,
everything behaves exactly as today.

### When the primary moves on

- **Own connections full:** StreamShare already counts its provider connections
  (`provider.Arbiter`). If none is free for a new live stream, go straight to the
  first fallback without asking the own provider: faster, and no pointless
  request to the provider. A background VOD download on the own provider is
  still preempted first (a free own slot beats a fallback); only household
  streams make the own provider count as full.
- **Who keeps the own connection (owner, 2026-11-13):** today "newest wins" — a
  new household stream takes the slot from the oldest one (`provider.Fresh`).
  With fallbacks configured this changes:
  - a **different household viewer** starting a channel while the slot is held
    goes to the fallbacks first; if every fallback fails, they get the error
    slate and the viewer already watching keeps their stream (nobody is cut);
  - the **same viewer** starting a channel on another device (a channel switch)
    keeps today's behaviour: their own old stream makes way, no failover.
  The builder decides how "same viewer" is recognised (it is the identity the
  session manager already uses for its own-stream/channel-switch logic).
  Without fallbacks configured, "newest wins" stays exactly as today.
- **Share-link guests get no failover (owner):** a guest who cannot be served by
  the primary's own provider gets today's behaviour (slate), so fallback
  connections stay free for the household.
- **Otherwise** ask the own provider. Fail over on: connection-limit answers
  (e.g. 456/461 and whatever the catalogue classes as a connection limit), 401,
  403, 5xx, timeout, unreachable/DNS/TLS failures — i.e. any start failure the
  session manager classifies today (`UpstreamError`).
- **404 for a live channel** (channel not available at this provider): fail over
  too; the fallback may carry it.
- **All attempts fail, or the channel has no match at any fallback:** the
  primary shows the usual error slate for its own failure, as today.

### Channel matching

The primary periodically fetches each fallback's live channel list (through the
fallback instance's own Xtream API, with the service account) and keeps only a
compact index:

- key: the EPG channel ID (`epg_channel_id`), compared case-insensitively
  (seen in practice: `npo1.nl` on one provider, `NPO1.nl` on another);
- fallback key: a normalised channel name (strip provider decoration such as
  country prefixes, symbols, quality tags) for channels without an EPG ID;
- value: the fallback's stream ID(s), with a quality class derived from the name
  (UHD/4K, FHD, HD, SD).

**Quality pick (my call):** among the fallback's matches, prefer the same quality
class as the channel the viewer picked on the primary, otherwise the closest.

Memory: store only the index (ID/name → stream IDs + class), never the full
catalogue; refresh on the existing catalogue refresh cadence or slower. In disk
catalogue mode consider keeping the index on disk as well.

### Streaming through the fallback

The primary requests the channel from the fallback instance's live endpoint
(`/live/<service user>/<pass>/<fallback stream id>.ts`), not from the fallback's
provider directly. The fallback instance thereby keeps owning its provider
connection, limit, learning, logging and history.

In the primary, the switch belongs in the session manager's stream start: the
shared stream for that channel simply gets a different upstream URL. Every
viewer of that channel on the primary shares the one fallback stream, exactly as
they share a provider stream today. If a second primary viewer starts the same
channel, they join the existing (fallback-backed) stream.

### Forward marker (slates, loops, speed)

Every request the primary sends to a fallback carries a marker (e.g. a header
`X-StreamShare-Forwarded: 1`, plus the service account identifying the caller).
An instance that receives a marked request:

- **never shows an error slate** for it; it answers immediately with the real
  failure status (connection limit, 403, 5xx, 502/504 for unreachable/timeout),
  skipping the slate-and-retry loop;
- **never forwards it** to its own fallbacks (no loops, no chains);
- otherwise serves it like any viewer (sharing, catchup buffer recording, etc.).

This is what lets the primary tell "fallback failed" from "fallback streaming".
Without it a fallback's slate (HTTP 200 + video) looks like success.

**Version requirement:** both sides must understand the marker. The Suite only
offers instances as fallbacks when their version supports it (the Suite already
reads `/api/internal/version`); an instance running an older version is not
selectable as a fallback.

### Fairness and priority on a fallback

A fallback only helps when it has a free provider connection itself. If its own
users are watching, it answers "connection limit" (no slate, see above) and the
primary tries the next fallback. No instance ever exceeds its account's limit.

**A fallback's own household wins (owner):** forwarded streams get a lower
priority than the fallback's own household viewers — the class share-link guests
have today (`provider.Share`: below the household, above background downloads).
If a household viewer starts watching on the fallback while it is lending its
connection, the forwarded stream is cut. The primary's viewer loses the picture;
their app reconnects, and that new channel start fails over afresh (next free
fallback, or the slate). Mid-stream switching stays out of scope.

### Logging and display

- Primary log (info): which fallback served a channel and why the primary could
  not (e.g. "Failover: NPO 1 served via <instance name> (own provider: 456
  connection limit)"). Never log credentials; addresses only masked as the rest
  of the code does.
- History / streams API: the primary records the viewer's stream as usual plus a
  "via" field naming the fallback instance (by its display name / key, not its
  address). The fallback records the stream under the service account; the Suite
  should not double-count it as a separate household viewer (builder: decide
  whether the Suite hides service-account rows or merges them via the "via"
  field).
- Suite overview and history pages show "via <instance name>" for such streams.

## Suite responsibilities

- **Configuration:** per instance, an ordered "fallback instances" list (UI on
  the instance page), choosing from the other instances the Suite manages.
  Default: empty (feature off).
- **Wiring:** the Suite computes and injects into each instance the fallback
  list as environment (e.g. one variable holding an ordered list of
  `name|internal URL|service user|service password`, or several variables —
  builder's choice), using the internal address it already computes
  (`instanceUrl`: gluetun's address with the VPN on, the container name with it
  off). Nobody types addresses or secrets.
- **Service login (owner, 2026-11-13):** StreamShare has no user store of its
  own (basic mode: one fixed login, viewers tracked by IP; LDAP mode: an
  external directory), so the "service account" is a **separate service login
  in stream-share**, generated by the Suite and injected as environment like
  `INTERNAL_API_KEY`. stream-share accepts it in both auth modes, identifies its
  sessions by that name (not by IP), gives its streams the forwarded-stream
  priority above, and marks them for display. The primary gets each fallback's
  service login with the fallback list. This is a small new path in
  upstream-owned auth code; keep it minimal and clearly separated.
- **Versions:** only offer instances whose version supports the forward marker.
- **Display:** "via" in overview and history.

## Constraints

- stream-share: Go 1.23, vendored modules, no new dependencies; house style and
  ponytail rules (smallest complete change, comments say why); logs never
  contain credentials; respect upstream (lucasduport) code.
- The ~100 MB RAM target per instance in disk catalogue mode.
- Suite: existing Node/React stack and its schema/reconcile patterns (computed
  env values like `INTERNAL_API_KEY` are the precedent).
- Instance addresses and credentials never appear in the repos (code, tests,
  docs, commits, PRs).

## Left to the builder

- Exact env variable format and names; the marker header name.
- The internal address one instance uses to reach another. With the VPN on, all
  instances share gluetun's network namespace, so `instanceUrl`'s gluetun
  address may need to become localhost plus the instance's port there; verify.
- Index refresh cadence and storage (memory vs disk), name normalisation rules.
- Exact status codes a fallback returns for each failure class.
- How the Suite presents service-account streams (hide vs merge).
- Splitting into PRs (likely: stream-share forward marker + failover; Suite
  wiring + display).
