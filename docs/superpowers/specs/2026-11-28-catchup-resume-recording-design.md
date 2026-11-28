# Resume a channel's recording instead of replacing it — design

## Intent

Each live stream of a channel starts a fresh catchup recording
(`catchup.Manager.StartBuffer` stops and deletes the existing buffer of that
channel). When a channel's live stream stops and starts again shortly after
(a provider drop, a rewind that closed the paused stream to free the
provider connection, a player switching back to live), the minutes recorded
before the restart are thrown away. A player that then rewinds to them gets
the "no recording" slate.

Seen with a real player: a recording from 16:51 to 16:55 was replaced when the
channel restarted at 16:56:54; the player's next rewind to 16:53 found nothing.

## Goals

- When a channel's live stream starts while its previous recording still
  exists (stopped, inside its delete grace, or still read), the new stream
  continues that recording: the earlier minutes stay rewindable, with only a
  gap for the time nobody recorded.
- Stale cache files are still removed, and nothing leaves lingering files
  (owner constraint).

## Non-goals

- Filling the gap (nothing was recorded then).
- Keeping recordings longer than today's rules: the per-channel window
  (`CATCHUP_DURATION_HOURS`, two rotating files) and the delete grace stay.
- Resuming after the grace has deleted the files, or across restarts of the
  instance (`CleanupOldFiles` still clears the folder on start).

## Approaches

1. **Resume the existing DiskBuffer (chosen).** `StartBuffer` finds the
   channel's registered, stopped buffer and restarts writing on it: the
   current file becomes the previous one (as a rotation does) and a new
   current file is opened. One buffer object, the same two-file window and
   the same deletion rules; reads hold the same buffer, so `Hold` keeps
   working unchanged. The wall-clock index already handles a gap: a time in
   the gap maps to the last byte before it.
2. **Keep the old buffer next to the new one** (a list of buffers per
   channel, searched by time). More state, a second deletion path per
   channel, and a real risk of lingering files. Rejected.
3. **Append to the same file across the gap.** Byte-continuous but the
   MPEG-TS clock jumps inside one file, which some players handle badly, and
   the file can outgrow the rotation size. Rejected.

## Design

- `DiskBuffer.Restart(now time.Time) error`: only on a stopped buffer whose
  drain loop has finished. It rotates current → prev (the file evicted from
  prev is removed after `rotationGracePeriod`, exactly as a size rotation
  does; readers with an open handle keep reading it), opens a new current
  file, resets the stop state and starts a new drain loop. On failure the
  buffer stays stopped and `StartBuffer` falls back to today's behaviour
  (delete and start fresh).
- `StartBuffer(id)`: if a buffer for `id` is registered and stopped, cancel
  its pending grace and `Restart` it instead of deleting it; otherwise as
  today. A buffer that is still writing (a second live stream of the same
  channel cannot exist) keeps today's handling.
- Deletion stays single-owner in the catchup manager: the resumed buffer is
  the same object, so holds, grace and `Cleanup` apply as before. `Delete`
  already removes both files.
- No-lingering-files check: every file a buffer ever opened is either its
  current, its prev, or an evicted prev with a scheduled removal; tests
  assert the folder holds only those after resume, rotation, grace and
  Cleanup.

## Left to the builder

- Names; whether `Restart` takes the start time or reads the clock.
- Logging: one INFO line "resumed recording of <channel> (gap Xs)".
- Tests: catchup — stop → restart within grace keeps the old file as prev and
  answers `FileForTime` for a pre-gap time; restart while held; restart then
  grace/Cleanup leaves no files; a size rotation after a restart evicts and
  removes correctly. Session/server — a live restart of a channel within the
  grace keeps an earlier minute rewindable (the reported sequence).
