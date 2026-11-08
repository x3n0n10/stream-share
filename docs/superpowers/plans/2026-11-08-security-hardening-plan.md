# Security Hardening (PR A) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Close the security findings of the 2026-11-08 audit (spec section "PR A", items A1–A9) with small, tested diffs.

**Architecture:** Each item is a local change in `pkg/server` (one in `pkg/xtream`): a trusted-proxy rule on the router, HMAC-signed `/img` links built in the one place that makes them, redacted logging through the existing redaction helpers, one provider-URL builder, one credential check, IP-auth on two more routes with a sweeper, a single internal-API panic handler, and one client-header allowlist for upstream requests.

**Tech Stack:** Go 1.23, Gin, stdlib only (`crypto/hmac`, `crypto/sha256`, `crypto/rand`, `encoding/base64`, `net/url`).

**Spec:** `docs/superpowers/specs/2026-11-08-security-and-readability-design.md`

## Global Constraints

- Branch `fix/security-hardening` (draft PR #64). Commit per task; do not push (the controller pushes).
- Go 1.23: `G=/home/linuxbrew/.linuxbrew/opt/go@1.23/bin/go`; always `-mod=vendor`; tests need `CACHE_FOLDER=$(mktemp -d)`.
- Suite: `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor -race ./...` all `ok`; `$G vet -mod=vendor ./...` clean. `TestXtreamGetServesStaleWhileRefreshing` is flaky (~1/60): rerun once if only it fails.
- CI runs golangci-lint errcheck: ignored errors use `_ = f.Close()`, `_, _ = w.Write(...)`.
- No new dependencies, no new settings/flags. House style: `interface{}`, GPLv3 header (copy from `pkg/server/helpers.go`) on new `.go` files, comments say why.
- Ponytail: smallest complete change, reuse existing helpers, no unrequested abstraction; known limits get `// shortcut: <limit>, <when to upgrade>`.
- Logs never contain credentials, signatures' key, or provider URLs unmasked.
- Existing tests keep their assertions unless they asserted the behaviour being fixed (say which, in the report).
- Disk-mode memory profile must not regress (no new per-request allocations of catalogue size).

## Review Focus

1. Image URLs with `&`, `%`-escapes or non-ASCII characters: a signed link must verify (signature over the decoded `url` value). Test in Task 2.
2. A path-prefixed deployment (`CUSTOM_ENDPOINT` set, the owner's setup behind a reverse proxy): signed `/img` links built with the prefix still verify. Test in Task 2.
3. A key file that exists but is empty or the wrong length: it is replaced with a fresh key, never used. Test in Task 2.
4. `X-Forwarded-For` with several hops (`client, proxy1`) from a trusted proxy: `ClientIP()` is the rightmost untrusted address, never a value a client can freely prepend past an untrusted hop. Test in Task 1.
5. Players seeking in VOD/streams after the header allowlist: `Range` and `If-Range` still reach the provider. Test in Task 8.

---

## File Structure

- `pkg/server/server.go` (modify): call `c.applyTrustedProxies(router)` (Task 1); compute the image key in `NewServer` (Task 2); debug request logging (Task 3); start the recent-auth sweeper (Task 6).
- `pkg/server/trusted_proxies.go` (new, Task 1): `applyTrustedProxies`.
- `pkg/server/image_proxy.go` (modify, Task 2): signing in `proxyImagePath`, verification in `assetProxy`, key loading.
- `pkg/server/routes.go` (modify): `/img` drops `requireRecentAuth` (Task 2); `/play`, `/hls` get it (Task 6).
- `pkg/server/auth.go` (modify): redacted debug lines (Task 3), `credentialsValid` (Task 5), sweeper (Task 6).
- `pkg/xtream/xtream_proxy.go` (modify, Task 3): masked request log.
- `pkg/server/provider_urls.go` (new, Task 4): `providerStreamURL`.
- `pkg/server/api.go` (modify, Task 7).
- `pkg/server/proxy_handlers.go`, `pkg/server/hls_handlers.go`, `pkg/server/helpers.go` (modify, Task 8).
- `README.md` (modify, Task 1).

---

### Task 1: Trust `X-Forwarded-For` only behind a reverse proxy (A9)

**Files:**
- Create: `pkg/server/trusted_proxies.go`, `pkg/server/trusted_proxies_test.go`
- Modify: `pkg/server/server.go` (router setup in `Serve`, after `gin.New()`), `README.md` (`REVERSE_PROXY_ENABLED` entry)

**Interfaces:**
- Produces: `func (c *Config) applyTrustedProxies(r *gin.Engine) error` — flag `c.ReverseProxyEnabled` off: `r.SetTrustedProxies(nil)`; on: `r.SetTrustedProxies(privateProxyRanges)` with `var privateProxyRanges = []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "::1/128", "fc00::/7"}`. `Serve` returns its error (startup fails loudly on a bad range list).

- [ ] **Step 1: Write the failing tests** — `TestClientIPIgnoresForwardedForWithoutReverseProxy`: engine from `applyTrustedProxies` with flag off, handler records `ctx.ClientIP()`; request `RemoteAddr = "192.0.2.10:5000"`, header `X-Forwarded-For: 1.2.3.4` → `"192.0.2.10"`. `TestClientIPFromTrustedProxy`: flag on, `RemoteAddr = "10.0.0.5:5000"`, `X-Forwarded-For: 1.2.3.4` → `"1.2.3.4"`; same header from `RemoteAddr = "203.0.113.7:5000"` → `"203.0.113.7"`; flag on, `RemoteAddr = "10.0.0.5:5000"`, `X-Forwarded-For: 6.6.6.6, 1.2.3.4` → `"1.2.3.4"` (Review Focus 4); flag on, `RemoteAddr = "[fd00::5]:5000"`, `X-Forwarded-For: 2001:db8::1` → `"2001:db8::1"`.
- [ ] **Step 2: Run** `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor ./pkg/server -run 'ClientIP'` → FAIL (undefined `applyTrustedProxies`).
- [ ] **Step 3: Implement** `applyTrustedProxies` and call it in `Serve`. README: one or two lines under `REVERSE_PROXY_ENABLED`: when on, `X-Forwarded-*` headers are trusted only from loopback/private addresses (a proxy on the same host or Docker network); when off they are ignored and the client IP is the TCP peer.
- [ ] **Step 4: Run** the focused tests → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): trust X-Forwarded-For only from a reverse proxy on a private network`

---

### Task 2: Signed `/img` links (A1)

**Files:**
- Modify: `pkg/server/image_proxy.go`, `pkg/server/server.go` (`NewServer`), `pkg/server/routes.go` (the `/img` route), `pkg/server/image_proxy_test.go` (`testImageProxyConfig` sets a fixed key; `TestAssetProxy` requests use signed links)

**Interfaces:**
- Produces:
  - `const imageSigningKeyFile = "image-signing.key"` (in `utils.CacheRoot()`), `const imageSigningKeySize = 32`
  - `func loadImageSigningKey(dir string) []byte` — reads `dir/image-signing.key`; when missing or not exactly 32 bytes, generates 32 bytes with `crypto/rand`, writes them (temp file + rename, mode 0600) and returns them; when writing fails, `utils.WarnLog("Image links: cannot store the signing key in %s (%v); links change on the next restart", dir, err)` and returns the in-memory key. Never returns nil.
  - `Config.imageKey []byte` set in `NewServer` from `loadImageSigningKey(utils.CacheRoot())`.
  - `func (c *Config) imageSignature(raw string) string` — `base64.RawURLEncoding` of the first 16 bytes of HMAC-SHA256(key=`c.imageKey`, msg=`raw`).
  - `proxyImagePath` returns `customEnd + "/img?url=" + url.QueryEscape(raw) + "&sig=" + c.imageSignature(raw)`.
  - `assetProxy` first checks `hmac.Equal([]byte(ctx.Query("sig")), []byte(c.imageSignature(ctx.Query("url"))))`; false → `403` before parsing or fetching.
- Route: `r.GET("/img", c.assetProxy)` (no `requireRecentAuth`).

- [ ] **Step 1: Write the failing tests** (in `image_proxy_test.go`):
  - `TestAssetProxyRejectsUnsignedURL`: `/img?url=<httptest upstream>` with no `sig` → 403 and the upstream saw 0 requests.
  - `TestAssetProxyRejectsSignatureOfAnotherURL`: `sig` computed for `http://a.example/x.png`, `url` = upstream → 403, 0 upstream requests.
  - `TestAssetProxyServesSignedURL`: a link from `c.proxyImagePath(upstream + "/logo.png?a=1&b=%20é")` (Review Focus 1) → 200, body equals upstream body.
  - `TestProxyImagePathSignsWithCustomEndpoint`: `c.CustomEndpoint = "providerA"`; the path starts with `/providerA/img?url=`, and serving it through a router built with `c.routes` verifies (Review Focus 2).
  - `TestRewriteM3U8SignsChildURLs`: every `/img?url=` line in the rewritten manifest carries a `sig` that verifies.
  - `TestLoadImageSigningKey`: first call in `t.TempDir()` creates the file (mode 0600, 32 bytes); second call returns the same bytes; a file holding `"short"` is replaced by a 32-byte key (Review Focus 3); a non-writable dir (`chmod 0500`, skip as root) returns a 32-byte key and no file.
  Update `TestAssetProxy` and any test that builds `/img` links to go through `proxyImagePath`.
- [ ] **Step 2: Run** `CACHE_FOLDER=$(mktemp -d) $G test -mod=vendor ./pkg/server -run 'AssetProxy|ProxyImagePath|RewriteM3U8|LoadImageSigningKey'` → FAIL.
- [ ] **Step 3: Implement** per Interfaces. Check with `grep -rn '/img?url=' pkg` that no other code builds `/img` links by hand.
- [ ] **Step 4: Run** focused tests → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): /img only fetches URLs this server signed`

---

### Task 3: No credentials in debug logs (A2)

**Files:**
- Modify: `pkg/server/auth.go` (lines 146 and 183), `pkg/xtream/xtream_proxy.go:206`, `pkg/server/server.go` (`Serve` logger selection and `quietRequestLogger`), plus any other site the grep in Step 3 finds
- Test: `pkg/server/request_log_test.go` (new), `pkg/xtream/xtream_proxy_test.go` or a new test file in `pkg/xtream`

**Interfaces:**
- Consumes: `c.redactRequestURI(u *url.URL, route string) string`, `utils.MaskURL(string) string`.
- Produces: `quietRequestLogger` logs every request with `utils.DebugLog` when debug logging is on (status < 400), keeping today's Warn/Error lines for ≥ 400; `Serve` always installs it (no `gin.Logger()`).

- [ ] **Step 1: Write the failing tests**:
  - `TestDebugLogsRedactCredentials`: enable debug logging for the test (restore after), capture the standard `log` output (`log.SetOutput` to a buffer, restore after); request `GET /player_api.php?username=u1&password=s3cret` through a router using `c.authenticate` and `c.quietRequestLogger()`; the captured output contains no `s3cret`.
  - `TestRedactRequestURICoversCredentialRoutes`: table over (path, route pattern) for: `/live/u1/s3cret/12.ts` + `/live/:username/:password/:id`; `/live/prov/provpass/12.ts` + the provider-credential route; `/hlsr/tok/prov/provpass/ch/hash/chunk.ts`; the M3U track route (`/<endpointAntiColision>/prov/provpass/3/file.ts`); `/share/abcdef/live.ts` + `/share/:token/live.ts`; `/download/abcdef` + `/download/:token`. None of `s3cret`, `provpass`, `abcdef` remains in the output.
  - `TestActionDebugLogMasksProviderPassword` (pkg/xtream): debug on, capture `log`, `client.Action` against an httptest server with password `provpass` → output contains no `provpass`.
- [ ] **Step 2: Run** the three tests → FAIL (at least the first and third).
- [ ] **Step 3: Implement**: the two auth lines log `c.redactRequestURI(ctx.Request.URL, ctx.FullPath())`; the provider line logs `utils.MaskURL(u.String())`; `Serve` uses `c.quietRequestLogger()` in all modes. Then `grep -rn 'Request.URL\b\|URL.String()\|XtreamPassword\|\.Password\.String()' pkg --include=*.go | grep -i 'log'` and route any other credential-bearing log through the same helpers; list them in the report. Fix any gap the route table reveals in `redactRequestURI`.
- [ ] **Step 4: Run** focused tests → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): keep credentials out of debug logs`

---

### Task 4: Provider stream URLs built once, credentials escaped (A3)

**Files:**
- Create: `pkg/server/provider_urls.go`, `pkg/server/provider_urls_test.go`
- Modify every call site of `grep -rn 'c.XtreamBaseURL, .*c.XtreamUser, c.XtreamPassword' pkg --include=*.go`: `xtream_handlers_stream.go` (102, 115, 142, 195, 398, 422, 437), `vod_cache.go:791`, `vod_ext.go:55`, `handlers_vod.go` (198, 314, 512), `xtream_generate.go:139`, `live_shares.go` (521, 522)

**Interfaces:**
- Produces: `func (c *Config) providerStreamURL(kind string, segments ...string) string` — `strings.TrimRight(c.XtreamBaseURL, "/")`, then `"/"+kind` when `kind != ""`, then `"/"+c.XtreamUser.PathEscape()+"/"+c.XtreamPassword.PathEscape()`, then `"/"+url.PathEscape(s)` for each segment. Sites needing a `*url.URL` call `url.Parse` on the result.

- [ ] **Step 1: Write the failing tests**:
  - `TestProviderStreamURLEscapesCredentials`: user `u s`, password `a#b/c?d%`, base `http://prov.example:8080/`: for kinds `""`, `"live"`, `"movie"`, `"series"`, `"timeshift"` with segments (`"123.ts"`) and for timeshift (`"60", "2026-11-08:20-00", "123.ts"`), `url.Parse(got)` succeeds, `Fragment == ""`, `RawQuery == ""`, and splitting `u.EscapedPath()` and unescaping each segment gives `[kind?, "u s", "a#b/c?d%", segments...]`.
  - `TestLiveStreamReachesProviderWithEscapedPassword`: httptest provider records `r.URL.EscapedPath()`; config with password `a#b`; `GET /live/<local user>/<local pass>/42.ts` through the router → provider saw `/live/<user>/a%23b/42.ts`.
- [ ] **Step 2: Run** `... -run 'ProviderStreamURL|EscapedPassword'` → FAIL.
- [ ] **Step 3: Implement** the helper and switch all 15 sites (keep each site's surrounding error handling). `xtream_generate.go:139` builds a track URI with a prefix: map it onto `kind`/segments without changing the URI for credentials that need no escaping.
- [ ] **Step 4: Run** focused tests → PASS; full suite → all `ok`; `grep -rn 'c.XtreamBaseURL, .*c.XtreamUser, c.XtreamPassword' pkg` → no hits.
- [ ] **Step 5: Commit** — `fix: build provider stream URLs in one place with escaped credentials`

---

### Task 5: One credential check (A4)

**Files:**
- Modify: `pkg/server/auth.go` (`authenticate`, `appAuthenticate`), `pkg/server/server.go` (`authWithPathCredentials`)
- Test: `pkg/server/auth_test.go`, `pkg/server/auth_path_credentials_test.go`

**Interfaces:**
- Produces: `func (c *Config) credentialsValid(user, pass string) bool` — `c.LDAPEnabled` → `c.ldapAuthenticateCached(user, pass)`; else `subtle.ConstantTimeCompare` on both values, combined with `&` (both always compared).
- `appAuthenticate`'s `log.Printf("[stream-share] %v | %s |App Auth\n", …)` becomes `utils.DebugLog("App auth from %s", ctx.ClientIP())`.

- [ ] **Step 1: Write the failing tests**: `TestCredentialsValidLocal` (table: right user+pass → true; wrong user; wrong pass; both empty with configured creds → false). `TestPathCredentialsWrongPasswordIsUnauthorized` (use `newPathAuthRig`): right user, wrong password → 401.
- [ ] **Step 2: Run** `... -run 'CredentialsValid|PathCredentials'` → FAIL (undefined).
- [ ] **Step 3: Implement**; the three middlewares keep their parsing, status codes, `recordRecentAuth` calls and session handling, and call `credentialsValid` for the decision.
- [ ] **Step 4: Run** focused → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `refactor(auth): one constant-time credential check for every login path`

---

### Task 6: IP auth on `/play` and `/hls`, and the IP list swept (A5, A7)

**Files:**
- Modify: `pkg/server/routes.go` (`xtreamRoutes`), `pkg/server/auth.go` (sweeper), `pkg/server/server.go` (`Serve` starts/stops it)
- Test: `pkg/server/auth_test.go`, `pkg/server/routes_test.go`

**Interfaces:**
- Produces: `func sweepRecentAuth(now time.Time)` — deletes entries older than `recentAuthTTL`; `Serve` runs it from a `time.NewTicker(time.Hour)` goroutine stopped by a channel closed on return (same pattern as the LDAP sweeper there).
- Routes: `r.GET("/hls/:token/:chunk", requireRecentAuth, c.xtreamHlsStream)`, `r.GET("/play/:token/:type", requireRecentAuth, c.xtreamStreamPlay)`.

- [ ] **Step 1: Write the failing tests**: `TestPlayAndHLSRequireRecentAuth`: router from `c.routes` with a fake provider; `GET /play/tok/ts` from a fresh IP → 403 and the provider saw nothing; after `recordRecentAuth` for that IP → not 403. Same for `/hls/tok/1_2`. `TestSweepRecentAuth`: an entry 25 h old is removed, a 1 h old entry stays.
- [ ] **Step 2: Run** `... -run 'PlayAndHLS|SweepRecentAuth'` → FAIL.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** focused → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): require recent auth on /play and /hls; sweep the recent-auth list`

---

### Task 7: One panic handler on the internal API (A6)

**Files:**
- Modify: `pkg/server/api.go:50-63`
- Test: `pkg/server/api_test.go` (new if absent)

**Interfaces:**
- The group keeps only the custom recover middleware: it logs `API PANIC RECOVERED: %v` plus the stack (as today) and answers `500` with `types.APIResponse{Success: false, Error: "Internal server error"}`. The group's `api.Use(gin.Recovery())` is removed; the router-wide `gin.Recovery()` stays.

- [ ] **Step 1: Write the failing test** `TestInternalAPIPanicHidesDetails`: engine with the internal API group set up (or the same middleware chain), a test route that panics with `"secret detail"`; response 500, body lacks `secret detail`, captured log contains it.
- [ ] **Step 2: Run** → FAIL (body contains the detail).
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run** focused → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(api): one panic handler, no panic text in responses`

---

### Task 8: Only allowlisted client headers go upstream (A8)

**Files:**
- Modify: `pkg/server/proxy_handlers.go` (`buildUpstreamRequest`), `pkg/server/hls_handlers.go` (`xtreamHlsStream`, ~line 55-62), `pkg/server/helpers.go` (`prepareVODHeaders` reuses the allowlist)
- Test: `pkg/server/proxy_handlers_test.go`

**Interfaces:**
- Produces: `var forwardedClientHeaders = []string{"Range", "If-Range", "Accept", "Accept-Language"}` and `func copyForwardedHeaders(dst, src http.Header)` — copies only those keys. `buildUpstreamRequest`'s non-VOD branch uses it instead of `mergeHttpHeader(req.Header, ctx.Request.Header)`, then keeps today's forced `User-Agent`, `Accept-Encoding: identity`, `Accept` default and `Connection` default. `prepareVODHeaders` keeps its current output (check it already matches the allowlist plus its own defaults; use `copyForwardedHeaders` if it shortens it). `xtreamHlsStream` builds its request with `c.buildUpstreamRequest(ctx, target)` (keeping its own no-redirect client).

- [ ] **Step 1: Write the failing tests**: `TestUpstreamRequestForwardsOnlyAllowlistedHeaders`: client request with `Cookie: a=b`, `Authorization: Basic x`, `X-Forwarded-For: 1.2.3.4`, `Range: bytes=100-`, `If-Range: "e"`, `User-Agent: Player/1` → the built request has `Range` and `If-Range` (Review Focus 5) and the IPTV `User-Agent`, and no `Cookie`, `Authorization`, `X-Forwarded-For`. `TestHLSUpstreamUsesAllowlist`: fake provider records headers for an `/hls/...` request (after `recordRecentAuth`) → no `Cookie`, IPTV `User-Agent`.
- [ ] **Step 2: Run** `... -run 'AllowlistedHeaders|HLSUpstream'` → FAIL.
- [ ] **Step 3: Implement.** `mergeHttpHeader` uses in the provider → client direction stay unchanged.
- [ ] **Step 4: Run** focused → PASS; full suite → all `ok`.
- [ ] **Step 5: Commit** — `fix(security): forward only allowlisted client headers to the provider`

---

## After the last task (controller)

- Whole-branch review; disk-mode `tools/memprobe/measure.sh` run (no regression vs 88–91 MB).
- Update PR #64's description (what changed, tests, the one-time image-link 403 after upgrade, the `REVERSE_PROXY_ENABLED` effect), mark it ready for review, push. The owner merges.
