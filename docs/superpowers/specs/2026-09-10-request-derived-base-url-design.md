# Derive client-facing URLs from the request instead of static hostname config

## Problem

Every URL this proxy hands back to a client — stream/M3U track URLs
(`replaceURL`), image/manifest proxy URLs (`proxyImageURL`, shipped in
[2026-09-09-proxy-image-urls-design.md](2026-09-09-proxy-image-urls-design.md)),
and the Xtream login response's `server_info` block — is built from
`c.HostConfig.Hostname` and `c.AdvertisedPort`, static config populated from
the `--hostname`/`--advertised-port` flags via `viper.GetString("hostname")`/
`viper.GetInt("advertised-port")`.

`viper.AutomaticEnv()` binds `hostname` to the env var `HOSTNAME` with no way
to tell "the operator explicitly passed `--hostname`" apart from "nothing was
passed and viper fell through to an ambient env var." Docker always sets
`HOSTNAME` inside a container to the container ID, regardless of what the
operator intends — so any deployment that doesn't explicitly pass
`--hostname` gets the container ID baked into every client-facing URL. This
was found via a blank-icon report during initial rollout of the image-proxy
work above: `stream_icon` values came back as
`http://eb0e9ae2c5aa:8082/img?url=...`, unreachable from any client outside
the container's own network namespace.

`PublicBaseURL`/`ReverseProxyEnabled` already exist in `ProxyConfig` as an
explicit-override mechanism, but today only `handlers_vod.go`'s temporary
download-link builder uses them (`handlers_vod.go:369-399`) — every other
URL-generation call site bypasses them and goes straight to the
collision-prone `HostConfig.Hostname`/`AdvertisedPort` pair.

## Scope

Every place that builds an absolute client-facing URL:

- `replaceURL` (`server.go`) — M3U track URLs, both Xtream and
  M3U-provider modes.
- `proxyImageURL` (`image_proxy.go`) — image/manifest proxy URLs (the
  `/img?url=` route from the prior spec).
- The Xtream login response's `server_info.url`/`port`/`https_port`/
  `rtmp_port` fields (`xtream_handlers_api.go:137-142`).
- The two places that serve a cached M3U file to a client (`getM3U`,
  `xtreamGet`) — these need a different treatment than the three above,
  since the file they serve is generated once and reused across many
  future requests from potentially different hostnames (see Design §3).

Not in scope: `handlers_vod.go`'s existing download-link builder (already
has a working `PublicBaseURL`/`ReverseProxyEnabled` mechanism; unifying it
onto the new shared helper is a nice-to-have cleanup, not required to fix
the bug — see Out of scope).

## Design

### 1. `requestBaseURL`: one shared helper

```go
// requestBaseURL returns the "scheme://host[:port]" a client should use to
// reach this server, with no trailing slash. Priority:
//  1. PublicBaseURL, if the operator set one explicitly.
//  2. X-Forwarded-Proto/X-Forwarded-Host, but only when ReverseProxyEnabled
//     is on — an unset flag means "don't trust client-supplied headers,"
//     since any direct client could otherwise spoof the host used in URLs
//     handed back to itself.
//  3. The live request's own Host header (includes a non-default port
//     already, e.g. under Docker port-mapping the client's Host header
//     already reflects whatever external port they connected through).
// Protocol in cases 2-3 falls back to c.HTTPS when no forwarded-proto
// header is present — this app doesn't reliably know whether it's
// terminating its own TLS, so this one static flag stays.
func (c *Config) requestBaseURL(ctx *gin.Context) string {
    if base := strings.TrimRight(strings.TrimSpace(c.PublicBaseURL), "/"); base != "" {
        return base
    }

    protocol := "http"
    if c.HTTPS {
        protocol = "https"
    }
    host := ctx.Request.Host

    if c.ReverseProxyEnabled {
        if fh := ctx.Request.Header.Get("X-Forwarded-Host"); fh != "" {
            host = fh
        }
        if fp := ctx.Request.Header.Get("X-Forwarded-Proto"); fp != "" {
            protocol = fp
        }
    }

    return fmt.Sprintf("%s://%s", protocol, host)
}
```

Lives in `pkg/server/image_proxy.go`, next to `proxyImageURL` (its first
caller); no new file needed for one function.

### 2. `proxyImageURL` splits into path-only and request-aware forms

```go
// proxyImagePath returns the relative /img?url= path for raw, with no
// scheme or host — for callers with no live request (see §3).
func (c *Config) proxyImagePath(raw string) string {
    if _, err := url.ParseRequestURI(raw); err != nil {
        return raw
    }
    customEnd := strings.Trim(c.CustomEndpoint, "/")
    if customEnd != "" {
        customEnd = "/" + customEnd
    }
    return fmt.Sprintf("%s/img?url=%s", customEnd, url.QueryEscape(raw))
}

// proxyImageURL rewrites an absolute image/asset URL to point at this
// proxy's generic asset route instead of the upstream provider.
func (c *Config) proxyImageURL(ctx *gin.Context, raw string) string {
    path := c.proxyImagePath(raw)
    if path == raw {
        return raw // proxyImagePath left it untouched (empty/malformed/relative)
    }
    return c.requestBaseURL(ctx) + path
}
```

`ctx` threads through `proxyImageURL`'s three existing callers
(`rewriteImageFields`, `rewriteXMLTVIcons`, `rewriteM3U8`) — all of them
already have a live `ctx` in every call site (`xtreamPlayerAPI`,
`xtreamXMLTV`, `assetProxy`, `xtreamHlsStream`, `hlsXtreamStream`,
`m3u8ReverseProxy`), so this is a mechanical signature change, not a design
question.

### 3. M3U track URLs and `tvg-logo`: relative at generation, absolute at serve

`replaceURL` and `marshallInto` currently build absolute URLs at
*generation* time — but `marshallInto` runs from two different contexts:
inside a live request (`cacheXtreamM3u`, called from `xtreamGet`) and at
server startup with no request at all (`playlistInitialization()` →
`Serve()`, before any client has connected). A cached file, once written,
also gets served to many future clients that may reach the server via
different hostnames — baking in whichever hostname happened to trigger
generation is wrong for everyone else.

Fix: stop building absolute URLs at generation time entirely.

- `replaceURL` drops its protocol/host-building block and returns a
  relative path only (`/endpointAntiCollision/user/pass/0/track.ts`
  style, unchanged from today's path-building logic — only the
  `fmt.Sprintf("%s://%s%s:%d%s%s", protocol, ...)` prefix goes away).
- `marshallInto`'s `tvg-logo` rewrite calls `c.proxyImagePath(...)`
  (§2) instead of `c.proxyImageURL(...)` — also relative, also no `ctx`
  needed.

This also solves the "no `ctx` at `playlistInitialization()`" problem by
removing the need for one, rather than working around it.

**New: `rewritePlaylistHosts`** (same file as `rewriteM3U8`, same shape —
reuses the existing `rewriteQuotedURI` helper from the prior spec):

```go
// rewritePlaylistHosts prepends the current request's base URL to every
// relative track URI and tvg-logo value in an M3U body generated by
// marshallInto. marshallInto never emits an absolute URL, so any value
// starting with "/" is always ours to prefix — no resolution needed, unlike
// rewriteM3U8's cross-origin HLS case.
func (c *Config) rewritePlaylistHosts(ctx *gin.Context, body []byte) []byte {
    base := c.requestBaseURL(ctx)
    lines := strings.Split(string(body), "\n")
    for i, line := range lines {
        trimmed := strings.TrimRight(line, "\r")
        switch {
        case strings.HasPrefix(trimmed, "#EXTINF") && strings.Contains(trimmed, "tvg-logo="):
            lines[i] = rewriteQuotedURI(trimmed, "tvg-logo=", func(raw string) string {
                if strings.HasPrefix(raw, "/") {
                    return base + raw
                }
                return raw
            })
        case !strings.HasPrefix(trimmed, "#") && strings.HasPrefix(trimmed, "/"):
            lines[i] = base + trimmed
        }
    }
    return []byte(strings.Join(lines, "\n"))
}
```

**Serve-time wiring** — both call sites change from `ctx.File(path)` to
read-rewrite-serve:

- `getM3U` (`proxy_handlers.go`): read `c.proxyfiedM3UPath`, run
  `rewritePlaylistHosts`, `ctx.Data(200, "application/octet-stream",
  rewritten)` — keeping the existing `Content-Disposition`/`Content-Type`
  headers already set before this call.
- `xtreamGet` (`xtream_handlers_api.go:104`): same treatment on `path`
  (the cached file path, whether just-regenerated or a cache hit — both
  branches already converge on the same `ctx.File(path)` line today).

### 4. Xtream login response

`xtream_handlers_api.go:137-142`'s hand-built `server_info` map gets the
same treatment:

```go
base := c.requestBaseURL(ctx)
_, hostPort, _ := strings.Cut(base, "://")
host, portStr, err := net.SplitHostPort(hostPort)
if err != nil {
    // hostPort had no ":port" (client connected on the scheme's default
    // port, which Go's Host header omits) — fall back to the real listen
    // port, the only value guaranteed to exist.
    host = hostPort
    portStr = strconv.Itoa(c.HostConfig.Port)
}
```

`"url"` becomes `protocol + "://" + host` (protocol recovered the same way,
`strings.Cut(base, "://")`'s first return value), `"port"`/`"https_port"`/
`"rtmp_port"` all become `portStr` — matching today's behavior of using one
port value for all three fields.

### 5. `--hostname`/`--advertised-port`: deprecated, not removed

Both flags stay defined (`cmd/root.go` keeps registering them; `--help`
keeps showing them; passing them is not an error) but are no longer read by
any URL-generation code path after this change. `PublicBaseURL` is the one
remaining explicit override — it has no equivalent env-collision risk (no
platform reserves `PUBLIC_BASE_URL`) and already existed for exactly this
purpose. `HostConfig.Port` keeps its real job (the actual TCP listen bind,
`server.go:482-483`) — only `HostConfig.Hostname` and `AdvertisedPort`
become dead for URL generation.

Anyone currently relying on `--hostname` as a workaround (as this
deployment is, today, for the prior spec's rollout) keeps working until
this change ships; the durable fix going forward is
`--public-base-url=http://<host>:<port>`.

## Error handling

- `PublicBaseURL` is used as a raw string prefix, not parsed — matches
  today's existing behavior in `handlers_vod.go`; a malformed value is the
  operator's problem to notice (same as today), not something this design
  needs to validate.
- `X-Forwarded-Host`/`X-Forwarded-Proto` are trusted as-is when
  `ReverseProxyEnabled` is on — no additional sanitization, consistent
  with what the flag already communicates ("there's a trusted proxy in
  front of me").
- If `ctx.Request.Host` is somehow empty (malformed request), `base`
  degrades to `"http://"` — the emitted path then still starts with `/`
  and resolves correctly if the client treats it as a relative URL, or
  fails visibly (not silently) if it doesn't. No crash either way.
- `getM3U`/`xtreamGet`'s file read replaces `ctx.File`'s automatic 404 on a
  missing path with an explicit `os.ReadFile` error check →
  `ctx.AbortWithError(500, ...)`, consistent with every other handler's
  error style in this codebase.

## Testing

- `requestBaseURL` table test: `PublicBaseURL` set wins over everything;
  `ReverseProxyEnabled` + both forwarded headers present wins over raw
  `Host`; `ReverseProxyEnabled` true but headers absent falls through to
  raw `Host`; `ReverseProxyEnabled` **false** with forwarded headers
  present ignores them (the security-relevant case — must use raw `Host`,
  not the spoofable header).
- `proxyImagePath`/`proxyImageURL(ctx, ...)`: existing table test adapted,
  plus one case confirming `proxyImageURL(ctx, x) == requestBaseURL(ctx) +
  proxyImagePath(x)`.
- `rewritePlaylistHosts` table test: relative track URI gets prefixed;
  relative `tvg-logo` gets prefixed; a non-`#` line not starting with `/`
  (shouldn't occur given the generation-time invariant, but defensive) is
  left alone; a `#EXTINF` line with no `tvg-logo` attribute is left alone
  apart from its (already-relative) track line.
- Integration test for `getM3U`/`xtreamGet`: generate a cached file once
  (real `marshallInto` call, relative URIs), then call the handler twice
  with two *different* `Host` headers, assert the same cached file
  produces two different, each-correct, absolute URLs — this is the
  regression test directly proving the cross-client bug is fixed.
- Login response test: table test over `Host`/`ReverseProxyEnabled`/
  forwarded-header combinations, asserting `server_info.url`/`port` match.

## Out of scope

- Unifying `handlers_vod.go`'s download-link builder onto
  `requestBaseURL` — it already works (has its own
  `PublicBaseURL`/`ReverseProxyEnabled`/`DiscordAPIURL` heuristic) and
  isn't broken; revisit only if it needs to change for an unrelated reason.
- Approach C from brainstorming (memoizing `rewritePlaylistHosts`'s output
  per distinct `Host` header) — YAGNI until per-request rewrite cost is a
  measured problem; M3U playlists are small text, same order of cost as
  the already-shipped per-request HLS manifest rewriting.
- Detecting TLS termination via `ctx.Request.TLS` instead of the static
  `--https` flag — this app doesn't reliably terminate its own TLS in
  typical deployments (usually plain behind a reverse proxy, or directly
  exposed over plain HTTP), so the operator-set flag stays authoritative
  outside the `X-Forwarded-Proto` case.
- Removing the `--hostname`/`--advertised-port` flags outright — kept
  defined for backward compatibility (see Design §5); actual removal is a
  separate, later cleanup decision once nothing depends on them.
