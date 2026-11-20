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
- No retry loop for a failed Discord login (today there is none either; it
  just aborts startup).

## Approaches

1. Remove the per-command delete loop: `BulkOverwrite` already replaces the
   whole set, removing commands not in the list.
2. Start the bot in the background so the HTTP server comes up first.

Owner chose both (2026-11-20).

## Design

- `pkg/discord`: delete `cleanupExistingCommands` and its call in `Start()`.
  `registerSlashCommands` (bulk overwrite) stays as is.
- `Bot.Start()` becomes non-blocking: it runs `session.Open` plus the bulk
  overwrite in its own goroutine and returns at once. A failure there is logged
  as an error ("Discord bot failed to start: …") and the instance keeps running
  without the bot. Behaviour change: today the same failure aborts startup.
  `server.go` keeps its plain `Start(); defer Stop()`.
- `Stop()` waits for an in-flight start to finish (mutex or done channel inside
  the bot) before unregistering and closing the session. The "start failed"
  case is already safe: `unregisterSlashCommands` returns early without
  registered commands.

## Open question for the owner

`Stop()` still deletes the commands one by one (`unregisterSlashCommands`)
whenever a guild scope is in use, including the auto-guild case. That makes
shutdown take ~40 s (past Docker's stop grace period, so the container gets
killed) and hides the commands until the next start. Removing that loop too —
the bulk overwrite on start already syncs the set — would fix both. Left out
until the owner decides.

## Verification

Time container start to "Server is ready and listening" on the local dev suite
with the bot enabled: expect well under 1 s, with "Slash commands registered"
following shortly after.
