# Add fork-maintainer copyright lines and README attribution

## Problem

This repo is a GitHub fork of `lucasduport/stream-share` (GPLv3). Every
copyright line in it (113 occurrences) reads `Copyright (C) 2025  Lucas Duport`.
None name the fork's maintainer, even though the fork is 358 commits ahead of
upstream and 0 behind. Compared with upstream's `master`, the fork adds 18 Go
files and modifies 30. The README also links a PayPal donate button to
Lucas's account (`README.md:578`).

GPLv3 requires the existing copyright notices to stay intact in a modified
version. So the goal is not to replace Lucas's name but to add the
maintainer's name next to it where they wrote or changed code, and to point
the donation link at the maintainer.

## Scope

In:

- Copyright header changes in Go source files the fork added or modified
  relative to `lucasduport/stream-share` `master`.
- README: one fork-attribution line, and the PayPal link.

Out:

- The Go module path `github.com/lucasduport/stream-share` (133 import lines,
  `go.mod`). It stays so future merges from upstream do not conflict on
  imports. Renaming can be revisited if the fork stops tracking upstream.
- `LICENSE`, `vendor/`, and any file the fork did not change.
- Existing README credits to `jtdevops/iptv-proxy` and
  `pierre-emmanuelJ/iptv-proxy`.
- Files under `docs/`, which have no copyright headers.

## Design

Copyright holder name: `x3n0n10`. Year: `2026` (every fork commit is from 2026).
Header text below is the exact form already used in the repo, one holder per
line, two spaces after `(C) <year>`.

### Which files

The file list comes from GitHub's compare API, not from local git, because
the local `upstream` ref is unreliable:

```
gh api "repos/lucasduport/stream-share/compare/master...x3n0n10:master" --paginate
```

Only `*.go` files outside `vendor/` with status `added` or `modified` count.
Status `removed` files are ignored.

### Header rules

| File status | Has `Lucas Duport` header | Result |
| --- | --- | --- |
| `added` | yes | Replace the holder line with `Copyright (C) 2026  x3n0n10`. Lucas did not write it. |
| `added` | no | Prepend the standard GPL header with `Copyright (C) 2026  x3n0n10`. |
| `modified` | yes | Keep `Copyright (C) 2025  Lucas Duport` and add `Copyright (C) 2026  x3n0n10` on the next line. |
| `modified` | no | Leave alone. |

Expected counts, from the compare API: 17 added with a Lucas header (replaced),
1 added without a header (gets one), 29 modified with a Lucas header (line
added), 1 modified without a header (untouched). The GPL body text and the
`stream-share is a project to…` line are not changed.


### README

- Add a new paragraph directly after the intro paragraph (`README.md:7`): `Fork of [lucasduport/stream-share](https://github.com/lucasduport/stream-share), maintained by [x3n0n10](https://github.com/x3n0n10).`
- Replace the PayPal link target `https://www.paypal.me/lucasdup135` with
  `https://paypal.me/x3n0n10`. The badge image and surrounding text stay.
- Existing credits to `jtdevops/iptv-proxy` and `pierre-emmanuelJ/iptv-proxy`
  stay as they are.

### Mechanics

A one-off script applies the header rules to the list from the compare API. It
is not committed. The change is one commit, or two (headers, README) if that
reads better in review. It ships as one PR.

## Trade-offs

- New files lose Lucas's name in their header. This is deliberate: the fork's
  maintainer wrote them. If any of those 17 files was in fact copied from
  upstream code, the reviewer should say so and the file goes back to the
  `modified` rule.
- The year is a single year, not a range, because the fork's own commits all
  fall in 2026.
- This is a common-practice reading of GPLv3, not legal advice. Deleting
  Lucas's line anywhere is avoided on purpose.

## Testing

- `go build ./... && go test ./...` still pass (header-only edits).
- A check confirms no Go file lost its `Lucas Duport` line except the 17 `added`
  files: the count of `Lucas Duport` lines in non-vendor `*.go` files drops by
  exactly 17.
- A check confirms `x3n0n10` appears in exactly 47 headers (17 + 1 + 29).
- `git diff --stat` touches only `*.go` headers and `README.md`.
