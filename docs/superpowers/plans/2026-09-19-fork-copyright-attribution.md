# Fork Copyright Attribution Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add the fork maintainer's copyright line to the Go files this fork added or modified, and update the README fork credit and donation link.

**Architecture:** A one-off Python script (not committed) reads the changed-file list from GitHub's compare API (`lucasduport/stream-share` `master` vs `x3n0n10:master`) and applies the header rules from the spec. A second small edit updates `README.md`. Header edits are comment-only, so behavior is unchanged.

**Tech Stack:** Python 3 (stdlib only), `gh` CLI (already authenticated as x3n0n10), Go 1.23 for build/test checks.

## Global Constraints

Copied from the spec ([2026-09-19-fork-copyright-attribution-design.md](../specs/2026-09-19-fork-copyright-attribution-design.md)):

- Copyright holder name is `x3n0n10`, year `2026`. Header line form: ` * Copyright (C) 2026  x3n0n10` (two spaces after the year), one holder per line.
- Lucas's line ` * Copyright (C) 2025  Lucas Duport` is never deleted, except in the 17 `added` files.
- File rules: `added` + Lucas header → replace the holder line; `added` + no header → prepend the standard GPL header with `x3n0n10`; `modified` + Lucas header → keep Lucas's line and add `x3n0n10` on the next line; `modified` + no header → leave alone.
- Only `*.go` files outside `vendor/` with status `added` or `modified` are in scope.
- Go module path `github.com/lucasduport/stream-share` stays unchanged. `LICENSE`, `vendor/`, and `docs/` are not edited.
- README: fork line is `Fork of [lucasduport/stream-share](https://github.com/lucasduport/stream-share), maintained by [x3n0n10](https://github.com/x3n0n10).`, as its own paragraph directly after the intro paragraph at `README.md:7`. PayPal target `https://www.paypal.me/lucasdup135` becomes `https://www.paypal.com/donate/?hosted_button_id=7EL3L7PAWZCVW`. Existing credits to `jtdevops/iptv-proxy` and `pierre-emmanuelJ/iptv-proxy` stay.
- Commit trailer on every commit: `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`.

Work happens in the worktree `/Users/jorislankhorst/Source/stream-share-wt-copyright` on branch `claude/copyright-fork-attribution`. Run all commands from that directory. Commit author is already the noreply address (`20641331+x3n0n10@users.noreply.github.com`); do not change git config.

---

### Task 1: Copyright headers in changed Go files

**Files:**
- Modify: 47 `*.go` files chosen by the script (comment header only)
- Scratch, not committed: `.superpowers/sdd/fork-headers.py`

**Interfaces:**
- Consumes: `gh api repos/lucasduport/stream-share/compare/master...x3n0n10:master` (network; needs `gh auth status` to be logged in).
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Record the baseline**

Run:

```bash
git ls-files '*.go' | grep -v '^vendor/' | xargs grep -h "Lucas Duport" | wc -l
git ls-files '*.go' | grep -v '^vendor/' | xargs grep -h "Copyright (C) 2026  x3n0n10" | wc -l
```

Expected: `102` and `0`.

- [ ] **Step 2: Write the script**

Create `.superpowers/sdd/fork-headers.py` (`mkdir -p .superpowers/sdd` first). Do not `git add` it.

```python
#!/usr/bin/env python3
"""One-off: apply the fork copyright header rules to Go files changed vs upstream.

Run from the repo root. Not committed.
"""
import subprocess
import sys

LUCAS = " * Copyright (C) 2025  Lucas Duport\n"
ME = " * Copyright (C) 2026  x3n0n10\n"
UPSTREAM = "repos/lucasduport/stream-share/compare/master...x3n0n10:master"


def changed_go_files():
    out = subprocess.check_output(
        ["gh", "api", UPSTREAM, "--paginate", "--jq", ".files[]? | [.status,.filename] | @tsv"],
        text=True,
    )
    for line in out.splitlines():
        status, name = line.split("\t")
        if name.endswith(".go") and not name.startswith("vendor/") and status in ("added", "modified"):
            yield status, name


# Full GPL header block taken from main.go, holder line swapped.
with open("main.go") as f:
    block = f.read().split("*/\n", 1)[0] + "*/\n"
assert LUCAS in block, "main.go header template not found"
NEW_HEADER = block.replace(LUCAS, ME)

counts = {"added-replaced": 0, "added-prepended": 0, "modified-line-added": 0, "modified-skipped": 0, "already-done": 0}
for status, name in changed_go_files():
    with open(name) as f:
        src = f.read()
    if ME in src:
        counts["already-done"] += 1
        continue
    if status == "added" and LUCAS in src:
        src = src.replace(LUCAS, ME, 1)
        counts["added-replaced"] += 1
    elif status == "added":
        src = NEW_HEADER + "\n" + src
        counts["added-prepended"] += 1
    elif LUCAS in src:
        src = src.replace(LUCAS, LUCAS + ME, 1)
        counts["modified-line-added"] += 1
    else:
        counts["modified-skipped"] += 1
        continue
    with open(name, "w") as f:
        f.write(src)

print(counts)
```

- [ ] **Step 3: Run the script**

Run: `python3 .superpowers/sdd/fork-headers.py`

Expected output exactly:

```
{'added-replaced': 17, 'added-prepended': 1, 'modified-line-added': 29, 'modified-skipped': 1, 'already-done': 0}
```

If the numbers differ, stop and report BLOCKED with the output. The compare list may have changed because upstream or the fork moved; do not edit the script to force the numbers.

- [ ] **Step 4: Verify the script is idempotent**

Run it again: `python3 .superpowers/sdd/fork-headers.py`

Expected: `{'added-replaced': 0, 'added-prepended': 0, 'modified-line-added': 0, 'modified-skipped': 1, 'already-done': 47}`

- [ ] **Step 5: Verify the counts**

Run:

```bash
git ls-files '*.go' | grep -v '^vendor/' | xargs grep -h "Lucas Duport" | wc -l
git ls-files '*.go' | grep -v '^vendor/' | xargs grep -h "Copyright (C) 2026  x3n0n10" | wc -l
git diff --name-only | grep -vc '\.go$'
git diff --name-only | wc -l
```

Expected: `85` (102 minus the 17 replaced), `47`, `0`, `47`.

- [ ] **Step 6: Build and test**

Run: `go build ./... && go vet ./... && go test ./...`

Expected: no build or vet output; every package `ok` or `no test files`. The prepended header on `pkg/database/stream_history_pg_test.go` must not break the build (verified on a scratch copy while planning).

- [ ] **Step 7: Commit**

```bash
git add -u
git diff --cached --stat | tail -1
git commit -m "Add fork maintainer copyright line to changed Go files

Files this fork added get the maintainer's copyright line in place of
Lucas Duport's, since he did not write them. Files it modified keep
Lucas Duport's line and gain the maintainer's below it. Comment-only
change; no behavior change.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git log --format='%h %ae %s' | head -1
```

Expected: stat line `47 files changed`; the commit author email is `20641331+x3n0n10@users.noreply.github.com`. `git add -u` stages only tracked files, so `.superpowers/` and `docs/superpowers/.DS_Store` stay out.

---

### Task 2: README fork credit and donation link

**Files:**
- Modify: `README.md` (after line 7, and the PayPal link near line 578)

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces: nothing later tasks depend on.

- [ ] **Step 1: Confirm the anchors**

Run:

```bash
sed -n 7p README.md | cut -c1-60
grep -n "paypal.me" README.md
```

Expected: line 7 starts `StreamShare is a comprehensive IPTV management solution`; exactly one match: `[![paypal](https://www.paypalobjects.com/en_US/i/btn/btn_donateCC_LG.gif)](https://www.paypal.me/lucasdup135)`.

- [ ] **Step 2: Edit README.md**

Insert a new paragraph directly after line 7 (blank line before and after it):

```markdown
Fork of [lucasduport/stream-share](https://github.com/lucasduport/stream-share), maintained by [x3n0n10](https://github.com/x3n0n10).
```

In the same file, change only the link target `https://www.paypal.me/lucasdup135` to `https://www.paypal.com/donate/?hosted_button_id=7EL3L7PAWZCVW`. The badge image URL (`paypalobjects.com/...`) and the text around it stay.

- [ ] **Step 3: Verify**

Run:

```bash
git diff --stat
git diff README.md | grep '^[+-]' | grep -v '^+++\|^---'
grep -c "lucasdup135" README.md
grep -c "jtdevops/iptv-proxy" README.md
```

Expected: only `README.md` changed; the diff shows exactly the added fork paragraph (plus its blank line) and one removed/added PayPal line; `0` for `lucasdup135`; `jtdevops/iptv-proxy` count unchanged from before the edit (at least `1`).

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "README: credit upstream fork, point donate link to maintainer

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>"
git log --format='%h %ae %s' | head -1
```

Expected: author email `20641331+x3n0n10@users.noreply.github.com`.
