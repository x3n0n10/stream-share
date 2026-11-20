# Discord Bot Startup Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** An instance with the Discord bot reaches "Server is ready and listening" without waiting on Discord, and slash commands are synced with one bulk overwrite and never deleted one by one.

**Architecture:** Delete both per-command delete loops in `pkg/discord`. `Bot.Start()` launches a goroutine that opens the gateway with a retry loop (1 s doubling, capped at 10 min, stops on auth close codes or `Stop()`), then bulk-overwrites the commands. `server.go` keeps `Start(); defer Stop()`.

**Tech Stack:** Go 1.27, `github.com/bwmarrin/discordgo` v0.29.0, `github.com/gorilla/websocket` v1.4.2 (both vendored).

**Spec:** `docs/superpowers/specs/2026-11-20-discord-startup-design.md`

## Global Constraints

- Build and test with `-mod=vendor`.
- CI runs golangci-lint (errcheck): ignore results explicitly (`_ = b.session.Close()`), in tests too.
- No instance URLs, provider names or credentials in commits, code or PR text.
- Every commit needs a simplify review first; the commit hook blocks until `echo simplify-guard:complete '<worktree>'` is run as its own command.

## Review Focus

1. Discord unreachable at start (DNS/network failure on the gateway lookup): instance reaches "ready", the error is logged per attempt, retries continue — covered by Task 2's `TestConnectWithRetry_RetriesUntilSuccess` and Task 3's blocked-DNS run.
2. Bad token: Discord closes with 4004 after identify; logged once, no retry loop — `TestConnectWithRetry_StopsOnPermanentError`, `TestIsPermanentGatewayError`, Task 3 bad-token run.
3. `Open` returns `discordgo.ErrWSAlreadyOpen` (socket already up): counts as connected, never retried — `TestConnectWithRetry_AlreadyOpenIsSuccess`.
4. `Stop()` while the loop is sleeping between attempts: the loop exits promptly — `TestConnectWithRetry_StopEndsLoop`.
5. Bulk overwrite fails after a successful connect: logged, not retried, bot stays connected — no test (single existing call, unchanged behaviour); reviewer checks the log line remains.

---

### Task 1: Remove the per-command delete loops

**Files:**
- Modify: `pkg/discord/slash_commands.go` (delete `unregisterSlashCommands` ~L165-182, `cleanupExistingCommands` ~L185-212, the `b.registeredCommands = cmds` assignment ~L153)
- Modify: `pkg/discord/bot.go` (`Start()` ~L88-111: drop the `cleanupExistingCommands` call; `Stop()` ~L113-121: drop the `unregisterSlashCommands` call)
- Modify: `pkg/discord/types_internal.go:57` (delete `registeredCommands` field)

**Interfaces:**
- Produces: `registerSlashCommands() error` unchanged; `Stop()` = `close(b.stopChan)` + `_ = b.session.Close()`. `session.Close` can block on an in-flight `Open` (up to the 45 s handshake timeout); accepted, since `Stop()` only runs right before the process exits — mark it with a `// shortcut:` comment.

- [ ] **Step 1: Delete the code listed above.** If the `discordgo` import or a helper becomes unused, remove it.
- [ ] **Step 2: Verify**

Run: `go vet -mod=vendor ./pkg/discord/ && go test -mod=vendor ./pkg/discord/... ./pkg/server/...`
Expected: no output from vet, tests PASS. `grep -rn "registeredCommands\|ApplicationCommandDelete" pkg/` returns nothing.

- [ ] **Step 3: Commit** — `perf(discord): sync slash commands with one bulk overwrite only`

---

### Task 2: Non-blocking start with connect retry

**Files:**
- Create: `pkg/discord/connect.go`
- Create: `pkg/discord/connect_test.go`
- Modify: `pkg/discord/bot.go` (`Start()`)
- Modify: `pkg/server/server.go` (~L510-517, the `c.discordBot.Start()` block)
- Modify: `go.mod:32` (delete `// indirect` after `github.com/gorilla/websocket v1.4.2`; `vendor/modules.txt` already marks it explicit)

**Interfaces:**
- Consumes: `registerSlashCommands() error`, `b.stopChan` (Task 1 / existing)
- Produces:
  - `const connectRetryMin = time.Second`, `const connectRetryMax = 10 * time.Minute`
  - `func isPermanentGatewayError(err error) bool` — `websocket.IsCloseError(err, 4004, 4010, 4011, 4012, 4013, 4014)` (discordgo returns the close error unwrapped)
  - `func connectWithRetry(open func() error, stop <-chan struct{}, minWait time.Duration) bool` — calls `open` until it returns nil or `discordgo.ErrWSAlreadyOpen` (→ true); on a permanent error logs once with `utils.ErrorLog` and returns false; otherwise logs with `utils.WarnLog` and waits `wait` (start `minWait`, then `wait = min(wait*2, connectRetryMax)`) in a `select` with `stop`; returns false as soon as `stop` is closed (check before each attempt too)
  - `func (b *Bot) Start()` — no return value; `go` a func that runs `connectWithRetry(b.session.Open, b.stopChan, connectRetryMin)`, then on success logs the "registering … commands" line, calls `registerSlashCommands` (error → `utils.ErrorLog`, as today) and the existing global-scope warning

- [ ] **Step 1: Write the failing tests** in `connect_test.go`

```go
func TestIsPermanentGatewayError(t *testing.T) {
	for _, code := range []int{4004, 4010, 4011, 4012, 4013, 4014} {
		if !isPermanentGatewayError(&websocket.CloseError{Code: code}) {
			t.Errorf("code %d: want permanent", code)
		}
	}
	for _, err := range []error{&websocket.CloseError{Code: 4000}, &websocket.CloseError{Code: 1006}, errors.New("dial tcp: no such host"), nil} {
		if isPermanentGatewayError(err) {
			t.Errorf("%v: want retryable", err)
		}
	}
}

func TestConnectWithRetry_RetriesUntilSuccess(t *testing.T) {
	calls := 0
	open := func() error { calls++; if calls < 3 { return errors.New("unreachable") }; return nil }
	if !connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 3 {
		t.Fatalf("got calls=%d, want success after 3", calls)
	}
}

func TestConnectWithRetry_StopsOnPermanentError(t *testing.T) {
	calls := 0
	open := func() error { calls++; return &websocket.CloseError{Code: 4004} }
	if connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 1 {
		t.Fatalf("got calls=%d, want 1 attempt and false", calls)
	}
}

func TestConnectWithRetry_AlreadyOpenIsSuccess(t *testing.T) {
	calls := 0
	open := func() error { calls++; return discordgo.ErrWSAlreadyOpen }
	if !connectWithRetry(open, make(chan struct{}), time.Millisecond) || calls != 1 {
		t.Fatalf("got calls=%d, want 1 attempt and true", calls)
	}
}

func TestConnectWithRetry_StopEndsLoop(t *testing.T) {
	stop := make(chan struct{})
	open := func() error { return errors.New("unreachable") }
	done := make(chan bool)
	go func() { done <- connectWithRetry(open, stop, time.Hour) }()
	close(stop)
	select {
	case ok := <-done:
		if ok { t.Fatal("want false after stop") }
	case <-time.After(time.Second):
		t.Fatal("loop did not exit after stop")
	}
}
```

- [ ] **Step 2: Run them to see them fail**

Run: `go test -mod=vendor ./pkg/discord/ -run 'ConnectWithRetry|PermanentGateway'`
Expected: FAIL — `undefined: isPermanentGatewayError`, `undefined: connectWithRetry`

- [ ] **Step 3: Implement `connect.go`** (signatures above; license header like `bot.go`), then change `Start()` and in `server.go` replace the error-checked call with:

```go
utils.InfoLog("Starting Discord bot...")
c.discordBot.Start()
defer c.discordBot.Stop()
```

Delete `// indirect` on the gorilla/websocket line in `go.mod`.

- [ ] **Step 4: Run tests, vet and lint**

Run: `go test -mod=vendor ./pkg/discord/... ./pkg/server/... && docker run --rm -v "$PWD":/app -w /app -e GOFLAGS=-mod=vendor golangci/golangci-lint:latest golangci-lint run ./...`
Expected: tests PASS, lint reports 0 issues.

- [ ] **Step 5: Commit** — `perf(discord): connect in the background and retry until Discord is reachable`

---

### Task 3: Verify on the local dev suite

No code. Build the branch image locally and run it next to the dev suite (test-provider credentials from the local dev suite's env file; never the production provider).

- [ ] **Step 1:** `docker build -t stream-share:discord-startup .`
- [ ] **Step 2: Bad token.** Run the image with the dev test instance's env plus `DISCORD_BOT_TOKEN=invalid`, on a free port, on the suite network, reusing the dev test instance's database (dev data only).
Expected in `docker logs -t`: "Server is ready and listening" < 1 s after "Logging initialized"; one error line mentioning close code 4004; no further Discord attempts over 30 s.
- [ ] **Step 3: Discord unreachable.** Same, with a token-shaped value and `--add-host discord.com:127.0.0.1 --add-host gateway.discord.gg:127.0.0.1`.
Expected: "ready" < 1 s; warning lines for retries at roughly 1, 2, 4, 8 s apart; `curl -s -o /dev/null -w '%{http_code}' localhost:<port>/healthz` → 200 throughout.
- [ ] **Step 4:** Remove the test containers. Report timings in the PR. Real-token timing on the owner's instance is the owner's check after the dev image ships.
