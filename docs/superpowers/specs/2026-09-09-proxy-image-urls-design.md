# Proxy EPG icons, channel icons, and VOD/series covers

## Problem

The video stream itself is already proxied through stream-share, but
associated image URLs are not:

- `xmltv.php` streams the upstream EPG XML byte-for-byte, including
  `<icon src="...">` attributes that point at the upstream provider.
- `player_api.php` actions (`get_live_streams`, `get_vod_streams`,
  `get_series`, `get_series_info`, `get_vod_info`) return the upstream
  JSON untouched by `xproc.ProcessResponse`, so `stream_icon`, `cover`,
  `movie_image`, and `backdrop_path` all point at upstream.
- Generated M3U playlists (`xtreamGenerateM3u` for Xtream mode, and the
  bootstrap playlist for M3U-provider mode) set `tvg-logo` to the raw
  upstream `stream_icon`/source value.

Viewing clients resolve all of the above directly against the upstream
provider instead of through stream-share, bypassing its proxying (and
VPN egress).

## Scope

All image/icon URLs surfaced to clients: EPG icons, live channel icons,
and VOD/series covers and backdrops. Not just EPG + channel icons.

## Design

### 1. New route: generic image proxy

`GET /:endpoint/img?url=<url-encoded target>`, registered once in
`routes()` (`pkg/server/routes.go`) ahead of the xtream/m3u branch, so
it applies regardless of provider mode. Guarded by the existing
`c.authenticate` middleware, same as every other proxy route.

Handler (`assetProxy`, new file `pkg/server/image_proxy.go`):

1. Read `url` query param.
2. `url.Parse` it; reject (400) if it fails to parse or its scheme is
   not `http`/`https` (blocks `file://` and similar — not a host
   allowlist, just basic sanity).
3. Delegate to the existing `c.stream(ctx, parsed)` — the same
   body-streaming code already used for video — so no new proxying
   logic is introduced.

No host allowlist: per explicit decision, this proxy will fetch any
http(s) URL a client passes it, not just the configured provider's
host. This is a deliberate, known trade-off — the route sits behind
`c.authenticate`, so only already-authenticated viewers can use it,
but a viewer could use it to make the server fetch arbitrary
http(s) URLs (SSRF-ish, e.g. to probe hosts reachable from the
container/VPN egress network).

### 2. Helper: `proxyImageURL`

`func (c *Config) proxyImageURL(raw string) string` (same file), built
the same way `replaceURL` (`server.go`) already builds proxied stream
URLs:

```go
func (c *Config) proxyImageURL(raw string) string {
    if _, err := url.ParseRequestURI(raw); err != nil {
        return raw // leave empty/malformed/relative values untouched
    }
    protocol := "http"
    if c.HTTPS {
        protocol = "https"
    }
    customEnd := strings.Trim(c.CustomEndpoint, "/")
    if customEnd != "" {
        customEnd = "/" + customEnd
    }
    return fmt.Sprintf("%s://%s:%d%s/img?url=%s",
        protocol, c.HostConfig.Hostname, c.AdvertisedPort, customEnd,
        url.QueryEscape(raw))
}
```

### 3. Three rewrite call sites

Each is already a single choke point, so both provider modes are
fixed by one change:

- **`marshallInto`** (`pkg/server/server.go`): when writing a track's
  tags, if `tag.Name == "tvg-logo"`, rewrite `tag.Value` through
  `c.proxyImageURL` before formatting it into the `#EXTINF` line. This
  function is shared by `xtreamGenerateM3u`'s cache write
  (`cacheXtreamM3u` → `marshallInto(f, true)`) and the M3U-provider
  bootstrap write (`playlistInitialization` → `marshallInto(f, false)`),
  so one edit covers both.

- **`xtreamPlayerAPI`** (`pkg/server/xtream_handlers_api.go`): after
  `processedResp := xproc.ProcessResponse(resp)`, add
  `processedResp = c.rewriteImageFields(processedResp)` before
  `ctx.JSON`. `rewriteImageFields` (new, `pkg/server/image_proxy.go`)
  recursively walks `map[string]interface{}` / `[]interface{}` and,
  for the keys `stream_icon`, `cover`, `movie_image`, rewrites a
  string value via `proxyImageURL`; for `backdrop_path` (an array of
  strings), rewrites each element. Unknown keys/types pass through
  unchanged — this only ever touches known image fields.

- **`xtreamXMLTV`** (`pkg/server/xtream_handlers_stream.go`): after
  `resp, err := client.GetXMLTV()`, pass `resp` through a new
  `rewriteXMLTVIcons(resp []byte) []byte` (new file) before
  `ctx.Data`. Implemented with `encoding/xml`'s `Decoder`/`Encoder`
  token copy (not regex): copy every token verbatim, except when a
  `StartElement` is named `icon`, in which case its `src` attribute
  value is rewritten via `proxyImageURL`. Using the token stream
  (rather than string/regex matching) means entity-escaped characters
  in the URL (e.g. `&amp;` in a query string) round-trip correctly,
  and the rest of the document — encoding, whitespace, other
  elements — passes through byte-identical.

## Error handling

- Malformed or relative URLs in any of the three call sites are left
  as-is rather than dropped, so one bad string doesn't blank out a
  channel's icon.
- The `/img` route's fetch errors flow through the existing
  `c.stream` error handling — no new error path needed.
- `rewriteXMLTVIcons` falls back to returning the original bytes
  unchanged if the XML fails to decode (e.g. a provider returning a
  non-XML error body) rather than erroring the whole EPG response.

## Testing

One self-check is enough for this size of change — no framework, no
per-function suite:

- `pkg/server/image_proxy_test.go`: table test for `proxyImageURL`
  (empty input, relative/malformed input passes through, absolute
  URL gets wrapped and query-escaped) and for `rewriteXMLTVIcons`
  (an `icon src="..."` gets replaced, an entity-escaped URL survives,
  and a document with no `icon` elements comes back byte-identical).

## Out of scope

- Restricting the image proxy to the configured provider's own host
  (considered, explicitly declined — see "No host allowlist" above).
- Rewriting anything inside `<programme>` EPG entries beyond `icon`
  (xmltv programme-level artwork isn't used by this provider's EPG
  today; add if a provider is found that sends it).
