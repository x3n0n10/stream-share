# Security Hardening and Readability Design

## Intent

The owner wants StreamShare to read, to an experienced developer, as "yes,
this is obviously the right way to do it": security first, readable code,
related functions together and files split by responsibility, names that say
what things are for, no duplicate code, shallow nesting, and logging at the
moments that matter (never credentials).

A whole-repo audit on 2026-11-08 (master `5cd7236`, after PR #63) produced
15 findings. The owner chose to fix all of them, in the fork only (nothing is
offered upstream), as **two separate PRs**:

- **PR A — security and behaviour** (findings 1, 2, 3, 5, 7, 12, 14, 15,
  plus A9 from the design review: trusted proxies).
  Small diffs that change behaviour, each with its own test.
- **PR B — readability and cleanup** (findings 4, 6, 8, 9, 10, 11, 13).
  Mostly moves and deletions; no player-visible behaviour change except
  finding 6 (one extension rule instead of two) and finding 11 (a log line).

PR A comes first. Reason (owner agreed): a reviewer of a moves-only PR expects
"nothing changes"; a security fix hidden among moved lines is easy to miss.

Upstream code is changed where a finding lives in it. Merge friction with
future upstream merges is accepted.

## Constraints (both PRs)

- Go 1.23 (`go.mod`, Docker builder), vendored dependencies (`-mod=vendor`),
  no new dependencies.
- CI runs `golangci-lint` (errcheck): use `_ = f.Close()`,
  `_, _ = w.Write(...)` idioms for ignored errors.
- House style: `interface{}`, GPLv3 header on new `.go` files (copy from
  `pkg/server/helpers.go`), comments say why.
- Ponytail rules (owner): smallest complete change, no unrequested
  abstraction, option or config; reuse existing helpers; a known limit gets a
  `// shortcut: <limit>, <when to upgrade>` comment.
- Disk catalogue mode keeps its memory profile (≤ 100 MB settled at the
  owner's catalogue size; `tools/memprobe/measure.sh` must not regress).
- `go test -mod=vendor -race ./...` and `go vet` pass; existing tests keep
  their assertions unless a test exercised the bug being fixed.
- The repo's simplify-guard hook requires a simplify review before every
  commit (`echo simplify-guard:complete '<worktree>'` after the review).

## PR A — security and behaviour

Branch `fix/security-hardening` from master.

### A1. Signed `/img` URLs (finding 1, SSRF)

**Problem.** `assetProxy` (`pkg/server/image_proxy.go:176`) fetches any
http(s) URL; the only gate is `requireRecentAuth` (the caller's IP logged in
within `recentAuthTTL`). A viewer, or anyone behind the same NAT, can make the
server fetch LAN or localhost addresses and read the reply.

**Design.**
- Every `/img` link the server hands out is built in one place,
  `proxyImagePath` (`image_proxy.go:38`). It adds `&sig=<signature>`, where
  the signature is the first 16 bytes of HMAC-SHA256 over the exact `url`
  value, base64url-encoded without padding.
- `assetProxy` recomputes the signature and compares it with `hmac.Equal`
  before doing anything else; missing or wrong → `403`. A valid signature is
  the whole authorisation: `/img` drops `requireRecentAuth`, which also lets
  a second device (e.g. a cast target) load images the player was given.
- **Key.** 32 random bytes from `crypto/rand`, stored once in
  `CACHE_FOLDER` (file mode 0600; the file name is the builder's call) and
  read back on every start. Reasons (controller call): players such as
  TiviMate cache catalogue and EPG image URLs for days, so the key must
  survive restarts; deriving it from the passwords is unsafe because the
  local password defaults to the public `passwordtest` (and is often unused
  under LDAP) and the provider password is empty in M3U-only mode. If the
  file cannot be read or written, generate an in-memory key and log a
  warning that image links will change on the next restart (no startup
  failure). A malformed key file is replaced.
- **Migration (owner accepted):** links players cached before this change
  are unsigned and return 403 until the player refreshes its catalogue; no
  transition period.
- All producers already go through `proxyImagePath`: `rewriteImageFields`
  (player_api), `rewritePlaylistHostsTo` (tvg-logo), `rewriteXMLTVIconsTo`
  (EPG icons), `rewriteM3U8` (HLS manifests proxied through `/img`, including
  nested manifests). Verify none builds `/img?url=` by hand.
- The signature covers the decoded `url` query value (what `ctx.Query("url")`
  returns), so encoding differences cannot change the outcome.
- Tests: unsigned → 403; tampered `url` with a valid `sig` for another URL →
  403; valid → proxied (httptest upstream); a manifest's rewritten child URLs
  carry valid signatures; the key file is created once and reused (second
  load returns the same key); unwritable `CACHE_FOLDER` → in-memory key,
  no error.

### A2. No credentials in debug logs (finding 2)

**Problem.** With `LOG_DEBUG_ENABLED`:
- `auth.go:146` and `auth.go:183` log `ctx.Request.URL` including
  `?username=…&password=…`;
- `pkg/xtream/xtream_proxy.go:206` logs the provider request URL with the
  provider's username and password;
- `server.go:492` installs `gin.Logger()`, which prints stream paths such as
  `/live/<user>/<pass>/<id>`.

**Design.**
- The two auth log lines log `c.redactRequestURI(ctx.Request.URL, ctx.FullPath())`.
- The provider request line logs `utils.MaskURL(u.String())` (it already masks
  path credentials and `username`/`password` query values).
- Replace `gin.Logger()` in debug mode with the existing
  `quietRequestLogger`, extended so that in debug mode it also logs
  successful requests (at debug level). One request logger, always redacted.
- Grep the tree for other log calls that print a raw request URL, a provider
  URL, or a `CredentialString` value, and route them through the same
  redaction. Report what was found.
- Audit `redactRequestURI` against every route that carries credentials:
  the provider-credential stream routes and `/hlsr/...` (provider user and
  password as literal segments), the viewer-credential routes
  (`:username/:password`), the M3U track route (`endpointAntiColision` +
  provider credentials), `/share/:token`, `/download/:token`. Fix any gap;
  add a table test over these route shapes.
- Test: a debug-mode request with `?username=u&password=p` to an
  authenticated route produces log output without `p`; the provider client's
  debug line contains no provider password (capture `log` output).

### A3. Provider URLs built once, with escaped credentials (finding 3)

**Problem.** 15 places format provider stream URLs with
`fmt.Sprintf("%s/…/%s/%s/%s", c.XtreamBaseURL, c.XtreamUser, c.XtreamPassword, …)`
without escaping. A provider password containing `#`, `?`, `/` or `%` gives a
broken URL (everything after `#` is dropped) and every stream fails.
`CredentialString.PathEscape` exists but is unused there.

Call sites (grep `c.XtreamBaseURL, .*c.XtreamUser, c.XtreamPassword`):
`xtream_handlers_stream.go` (102, 115, 142, 195, 398, 422, 437),
`vod_cache.go:791`, `vod_ext.go:55`, `handlers_vod.go` (198, 314, 512),
`xtream_generate.go:139`, `live_shares.go` (521, 522).

**Design.** One helper in `pkg/server` that returns the provider URL for a
stream: base URL, optional kind segment (`live`, `movie`, `series`,
`timeshift`, or none for the bare form), escaped username and password, then
the remaining segments, each escaped with `url.PathEscape`. All 15 sites use
it (sites that need a `*url.URL` parse its result). Name and exact signature
are the builder's call; it must read as what it is for (e.g.
`providerStreamURL`).
- Test: password `a#b/c?d%` and username `u s` → every kind produces a URL
  whose parsed path segments round-trip to the original credentials and id;
  one stream handler test with such credentials reaches the fake provider
  with the right path.

### A4. One credential check (finding 5)

**Problem.** The same rule (LDAP if enabled, else the local user/password) is
implemented three times: `authenticate` (`auth.go:145`, constant-time),
`appAuthenticate` (`auth.go:182`, constant-time), `authWithPathCredentials`
(`server.go:625`, plain `!=`).

**Design.** `func (c *Config) credentialsValid(user, pass string) bool`:
LDAP via `ldapAuthenticateCached` when enabled, otherwise constant-time
comparison of both values (always compare both, no early return). The three
middlewares keep their own input parsing, responses and session handling, and
call it for the decision. `appAuthenticate`'s stray `log.Printf` line becomes
a `utils.DebugLog` without credentials.
- Tests: existing auth tests pass unchanged; add one table test of
  `credentialsValid` (local: right, wrong user, wrong password, empty) and one
  path-credentials request with a wrong password → 401.

### A5. Login check on `/play` and `/hls` (finding 7)

**Problem.** `routes.go`: `/hls/:token/:chunk` and `/play/:token/:type` have
no auth middleware; anyone can make the server contact the provider.

**Design.** Add `requireRecentAuth` to both routes (players reach them right
after an authenticated request from the same IP; a live HLS player keeps
re-fetching its manifest through `/hlsr/...`, which records auth, so a restart
recovers on the next manifest poll). `/hlsr/...` keeps `recordRecentAuth`
(its path carries the provider credentials). Signatures are not used here
because these URLs come from provider redirects, not from links this server
builds. This check is only as good as the client IP, which A9 fixes.
- Test: request without prior auth → 403; after an authenticated request from
  the same IP → passes the middleware (fake provider).

### A6. One panic handler on the internal API (finding 12)

**Problem.** `api.go:50-63`: the global `gin.Recovery`, a second
`gin.Recovery` on the group, and a custom `recover` that answers
`Internal server error: <panic text>`.

**Design.** Keep only the custom handler on the group; it logs the panic and
stack (as today) and answers a fixed `Internal server error` without the
panic text. Remove the group's `gin.Recovery`. The global `gin.Recovery` on
the router stays (it covers every other route).
- Test: a panicking internal-API handler (test-registered) → 500, body
  without the panic text, log contains it.

### A7. Recent-auth IP list is swept (finding 14)

**Problem.** `recentlyAuthenticated` (`auth.go:100`) only drops an entry when
that IP calls a guarded route again; IPs that never return stay forever.

**Design.** A ticker started in `Serve` (same pattern as the LDAP cache
sweeper, stopped on shutdown) removes entries older than `recentAuthTTL`,
every `recentAuthTTL`.
- Test: the sweep function removes an expired entry and keeps a fresh one.

### A8. Only needed client headers go upstream (finding 15)

**Problem.** `mergeHttpHeader(req.Header, ctx.Request.Header)` copies every
client header to the provider: in `buildUpstreamRequest`
(`proxy_handlers.go:144`, all non-VOD streams and `/img`) and in
`xtreamHlsStream` (`hls_handlers.go:62`). Cookies, `Authorization`,
`X-Forwarded-For` (viewer IPs) reach the provider.

**Design.** One allowlist used wherever client headers are forwarded
upstream: `Range`, `If-Range`, `Accept`, `Accept-Language`. `User-Agent`
stays overridden with `utils.GetIPTVUserAgent()` as today; `Accept-Encoding`
and `Connection` keep today's forced values. `xtreamHlsStream` builds its
request through `buildUpstreamRequest` too (controller call), so HLS gets the
IPTV User-Agent like every other upstream request instead of the player's.
VOD keeps `prepareVODHeaders` (check it forwards nothing beyond the
allowlist plus what it sets itself). Response-side `mergeHttpHeader` uses
(provider → client) are unchanged. The owner verifies against the real
provider with a dev image; no override switch.
- Test: a client request with `Cookie`, `Authorization`, `X-Forwarded-For`
  and `Range` → the fake provider sees only `Range` (plus the forced headers).

### A9. Trust `X-Forwarded-For` only behind a reverse proxy (design review)

**Problem.** The router never calls `SetTrustedProxies`, and gin trusts every
proxy by default: `ctx.ClientIP()` returns whatever `X-Forwarded-For` a client
sends. Anyone can claim the IP of a viewer who just logged in and pass
`requireRecentAuth` (`/play`, `/hls`, and any other IP-based check), and IP
aliases and per-IP identities can be spoofed.

**Design.** Tie it to the existing `REVERSE_PROXY_ENABLED` flag (which
already gates `X-Forwarded-Proto`/`-Host` in `requestBaseURL`), no new
setting:
- flag off: `router.SetTrustedProxies(nil)` — `ClientIP()` is the TCP peer.
- flag on: trust only loopback and private ranges (`127.0.0.0/8`,
  `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `::1/128`, `fc00::/7`),
  which covers a reverse proxy on the same host or Docker network (the
  owner's setup: a reverse proxy in front, path-prefixed public URL).
- Document the effect in the README next to `REVERSE_PROXY_ENABLED`.
- Tests: flag off, request with `X-Forwarded-For: 1.2.3.4` from `192.0.2.10`
  → `ClientIP()` is `192.0.2.10`; flag on, same header from `10.0.0.5` →
  `1.2.3.4`; flag on, header from a public peer → the peer.

### A10. Every URL a client receives points at StreamShare (owner, 2026-11-09)

**Owner requirement:** every call a client makes goes through StreamShare.
Players must never receive an address of the provider or any third party.

**Problem (measured on the owner's instance with the PR A build).** EPG
icons and M3U logos already go through `/img`, but player_api does not:
- the catalogue lists served from the catalogue cache (`get_live_streams`,
  `get_vod_streams`, `get_series`) are never rewritten: 121,613
  `stream_icon` (VOD), 29,351 `cover` + 27,472 `backdrop_path` (series) and
  21,419 `stream_icon` (live) point at the provider, image.tmdb.org,
  Amazon and dozens of other hosts; `direct_source` is not stripped;
- the uncached actions are rewritten by `rewriteImageFields`
  (`pkg/server/image_proxy.go:130`) from a fixed field list that misses
  fields such as `info.cover_big`, `info.kinopoisk_url` (`get_vod_info`) and
  `seasons[].cover_big` (`get_series_info`).

**Design.**
- **One rule, no field list.** Every string value anywhere in a player_api
  JSON answer that is, in its entirety once trimmed of surrounding blanks,
  an absolute `http`/`https` URL (the trimmed value parses with `url.Parse`,
  scheme http or https, non-empty host; a space inside the path, as in
  `http://prov/logos/Sky Sports.png`, still counts) becomes
  `base + proxyImagePath(trimmed value)` (a signed `/img` link, A1). Strings
  that merely contain a URL (e.g. a `plot`, `See https://x.com`) are
  unchanged.
  Every `direct_source` key is removed, at any depth. The rewrite never
  mutates its input (catalogue data is shared by concurrent requests); it
  returns copies of the maps and slices it changes.
- **Everywhere.** The uncached player_api path uses the new rule instead of
  `rewriteImageFields` (which is deleted). The catalogue render
  (`renderCatalog` and the disk render) applies it per item, in both storage
  modes, for whole lists and category slices.
- **Per base URL.** The rewritten link contains the client-facing base URL
  (`requestBaseURL`: `PUBLIC_BASE_URL`, else forwarded/Host headers). Each
  catalogue entry keeps its rendered output per base URL, at most 4; adding
  a fifth drops the oldest (disk mode deletes its files). Memory mode and
  disk mode stay byte-identical for the same base URL.
- **README:** recommend setting `PUBLIC_BASE_URL` behind a reverse proxy
  (one render per catalogue, and clients cannot vary the base through
  `Host`/`X-Forwarded-Host`).
- **Cost (accepted):** each rewritten URL grows by ~120 bytes; at the owner's
  size ~25 MB more rendered output per base URL (on disk in disk mode, in
  RAM in memory mode). Signing ~230k URLs per refresh takes well under a
  second.
- Tests: each field from the measurement (`stream_icon`, `cover`,
  `backdrop_path` entries, `cover_big`, `kinopoisk_url`, a nested
  `seasons[].cover_big`) is rewritten; a `plot` containing a URL is not;
  `direct_source` is gone at any depth; the input value is unchanged after
  the rewrite; two different `Host` headers get two renders with their own
  base; a fifth base evicts the oldest (disk files removed); memory and disk
  mode give identical bytes per base; disk-mode RSS does not regress
  (`tools/memprobe`).

## PR B — readability and cleanup

Branch `refactor/readability`, created from PR A's head so the two never
conflict (A3 touches the handlers B4 deletes). Opened against master once PR A
is merged (or as a PR based on PR A's branch until then — builder's call).

### B1. One stream handler per route kind (finding 4)

`xtreamProxyCredentials{Stream,LiveStream,Movie,Series}Handler`
(`xtream_handlers_stream.go:416-452`) duplicate
`xtreamStreamHandler/Live/Movie/Series`. Point the viewer-credential routes
(`addProxyCredentialRoutes`) at the existing handlers and delete the copies.
The extra debug lines they logged are dropped (the request logger already
logs the redacted path in debug mode after A2).

### B2. One VOD extension/path resolver (finding 6)

`resolveVODCacheURL` (`vod_cache.go:781`) and `resolveVODCacheTarget`
(`handlers_vod.go:489`) decide the upstream URL and cache file for a VOD
item with different rules (only the second probes when
`VOD_EXT_PROBE_ENABLED`). Merge into one function with a probe switch; the
request path (fast) keeps not probing, the background download path keeps
probing. Canonical order (controller call): the extension in the request,
then the catalogue (VOD M3U), then probing (background only, when
`VOD_EXT_PROBE_ENABLED`), then the per-type default. Existing cache files
named under the old rules are left alone; they expire through normal cache
maintenance. Both must produce the same file name for the same item and
extension, so a player request and a Discord download share one cache file.
- Test: with probing enabled and a probed extension, both callers resolve the
  same destination.

### B3. Long functions split by job (finding 8)

- `fetchToFile` (`vod_cache.go:189`, 363 lines).
- `NewServer` (`server.go:116`, 251 lines): one function per subsystem it
  sets up (database/sessions, catchup, slate, Discord, health, provider
  info…), `NewServer` reads as the list of steps.
- `Serve` (`server.go:391`, ~150 lines): background workers start/stop vs
  router setup.
- `multiplexedStream` (`server.go:779`, ~150 lines).

Split by job, never by line count; code is moved, not rewritten; no new types
or layers unless a group of values is already passed around together. Error
handling and logging move with the code. Existing tests must pass unchanged.

### B4. `session/manager.go` by job (finding 9)

Move temporary download links (`GenerateTemporaryLink`, `GetTemporaryLink`
and their state) and VOD cache maintenance (`runCacheMaintenance` and
helpers) into their own files in `pkg/session`. No API change.

### B5. `server.go` keeps only startup (finding 10)

Pure moves:
- `addProxyCredentialRoutes`, `authWithPathCredentials`,
  `resolveRequestUsername` → next to the other auth/route code (`auth.go`,
  `routes.go`);
- `handleTemporaryLink` → `handlers_vod.go`;
- `multiplexedStream`, `classifyStreamType` → `xtream_handlers_stream.go`;
- `marshallInto`, `replacePath`, `sanitiseFilename`, `playlistInitialization`
  → an M3U file (existing `m3u_*.go` or a new `m3u_playlist.go`);
- `redactRequestURI`, `quietRequestLogger` → a logging/middleware file.

`server.go` keeps `NewServer`, `Serve` and the background refreshers it
starts. The builder may adjust targets where a better existing file fits;
the rule is "a reader finds a function next to its siblings".

### B6. Startup log shows real values (finding 11)

`server.go:405-408` logs hard-coded `session=1m, stream=1m, tempLink=1h`.
Log the configured values, or delete the line if the session manager does not
expose them without new API (builder's call; no new getters just for a log
line).

### B7. Dead code removed (finding 13)

Delete: `rewriteXMLTVIcons` (test-only wrapper; tests call
`rewriteXMLTVIconsTo`), `connPacer.setSpacing`, `DBManager.Close`,
`utils.Close`, `database.IsUniqueViolation`, `filter.Kind.String`,
`filter.Set.ExcludesAdult` (test-only; adjust tests). Re-run `deadcode`
(golang.org/x/tools/cmd/deadcode v0.28.0 built with Go 1.23) and remove
anything else it reports that has no caller, unless removing it changes
behaviour.

## Verification

- Both PRs: full suite with `-race`, `go vet`, CI (golangci-lint) green.
- PR A: the tests named per section.
- PR B: existing tests unchanged (except tests of deleted dead code), plus
  B2's test; `tools/memprobe/measure.sh` in disk mode shows no regression.

## Calls made without the owner

- Signature key: random, persisted in `CACHE_FOLDER`, in-memory fallback
  with a warning; `/img` no longer requires recent IP auth.
- HLS upstream requests go through `buildUpstreamRequest` (IPTV User-Agent).
- B2 extension order: request, catalogue, probe (background only), default.
- A9 trusted proxies: private and loopback ranges when
  `REVERSE_PROXY_ENABLED`, none otherwise; no new setting.
- Upstream header allowlist: `Range`, `If-Range`, `Accept`,
  `Accept-Language`.
- `/play` and `/hls` get `requireRecentAuth` (IP-based), not signatures.
- PR B branches from PR A's head.

## Owner decisions

- Fix all findings, fork only, two PRs (A then B).
- Deployment: behind a reverse proxy with a path-prefixed public URL.
- One-time 403 on previously cached unsigned image links is accepted.
