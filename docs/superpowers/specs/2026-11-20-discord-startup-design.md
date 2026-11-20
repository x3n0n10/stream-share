# Discord bot: stop delaying instance startup

## Why

An instance with the Discord bot enabled needs ~42 s from container start to
"Server is ready and listening". ~41 s of that is the bot: before the HTTP
server starts, `Bot.Start()` deletes each existing slash command one by one
(`cleanupExistingCommands`, 11 commands, each delete rate-limited by Discord to
~4 s), then replaces the whole set with one `ApplicationCommandBulkOverwrite`
call. Instances without Discord reach "ready" in ~0.1 s.

## Goals

- "Server is ready and listening" no longer waits on Discord. A Discord outage
  or bad token must not stop the instance from serving streams.
- Slash command registration on start takes one Discord call instead of ~12.

## Non-goals

- No change to which commands exist, their scope (global / dev guild /
  auto-guild) or their handlers.

## Approaches

1. Remove the per-command delete loop: `BulkOverwrite` already replaces the
   whole set, removing commands not in the list.
2. Start the bot in the background so the HTTP server comes up first.

Owner chose both (2026-11-20).

## Design

- `pkg/discord`: delete `cleanupExistingCommands` and its call in `Start()`.
  `registerSlashCommands` (bulk overwrite) stays as is.
- `Bot.Start()` becomes non-blocking: it runs `session.Open` plus the bulk
  overwrite in its own goroutine and returns at once. `server.go` keeps its
  plain `Start(); defer Stop()`.
- If the first `session.Open` fails because Discord is unreachable, the
  goroutine logs it and retries with discordgo's own reconnect backoff (1 s,
  doubling, capped at 10 min) until it connects or `Stop()` is called; after
  that, discordgo's built-in reconnect takes over. Today the same failure
  aborts startup and Docker's `unless-stopped` restart loops the whole
  container until Discord is back.
- A failure that retrying can't fix — Discord closing the gateway with an
  auth/config close code (4004 bad token, 4010-4014) — is logged once as an
  error and not retried; the instance runs without the bot.
- A failed bulk overwrite is logged and not retried (as today); Discord keeps
  the commands from the previous registration.
- `Stop()` ends the retry loop. It must not hang on a connect attempt already
  in flight (the default websocket handshake timeout is 45 s): give the bot's
  session a short dial timeout, or let `Stop()` not wait on that attempt.
  The "start failed" case is already safe: `unregisterSlashCommands` returns
  early without registered commands.

## Open question for the owner

`Stop()` still deletes the commands one by one (`unregisterSlashCommands`)
whenever a guild scope is in use, including the auto-guild case. That makes
shutdown take ~40 s (past Docker's stop grace period, so the container gets
killed) and hides the commands until the next start. Removing that loop too —
the bulk overwrite on start already syncs the set — would fix both. Left out
until the owner decides.

## Verification

- On the local dev suite with the bot enabled: container start to "Server is
  ready and listening" well under 1 s, "Slash commands registered" shortly
  after.
- With a bad token: still reaches "ready", logs the auth error once, no retries.
- Retry loop and `Stop()` during a retry: a unit test with a stub opener that
  fails N times, rather than blocking Discord in a container.
