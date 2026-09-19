# Track the client IP as viewer identity when custom credentials are used

## Problem

When LDAP is disabled, the client's IP address is the de-facto per-viewer
identity: sessions, `/status`, VOD views, `stream_history` rows, IP aliases
([ip_aliases.go](../../../pkg/server/ip_aliases.go)) and the Discord history
commands all key on it. That identity comes from `resolveRequestUsername`
([server.go](../../../pkg/server/server.go)), which falls back to
`ctx.ClientIP()` when nothing earlier put a username in the request context.

The direct Xtream stream routes in `xtreamRoutes` ([routes.go](../../../pkg/server/routes.go))
never set a username. They are matched against the literal provider
credentials in the path, so viewers on those routes get the IP identity.

The proxy-credential routes in `addProxyCredentialRoutes` (`/:username/:password/:id`,
`/live/…`, `/movie/…`, `/series/…`, `/timeshift/…`) are different. Their
middleware, `authWithPathCredentials`, validates the custom `--auth-user` /
`--auth-password` pair, then calls `RegisterUser(username, ip, userAgent)` and
`ctx.Set("username", username)` with the **custom login name**. Every device
using the custom login therefore shares one identity:

- One session per login. `RegisterUser` overwrites `IPAddress` on each call,
  so the session shows whichever device connected last.
- `stream_history.username` holds the shared login; `ip_address` holds only
  the last-seen IP at the moment a stream opened.
- `displayNameFor` only resolves aliases for IP-shaped identifiers, so custom
  logins can never be aliased per device.

With a shared identity, `RequestStream` ([manager.go](../../../pkg/session/manager.go))
treats a second device using the same login as the same client, so it closes
the first device's client channel and stops its stream when it switches
channel. Two devices on the custom login could not watch simultaneously;
per-IP identity fixes that.

`authenticate` and `appAuthenticate` (player_api/get.php/xmltv) do not
register sessions, so they are unaffected.

## Scope

In:

- `authWithPathCredentials` uses the client IP as viewer identity when LDAP is
  disabled, matching the provider-credentials routes.

Out:

- LDAP-enabled behavior. The LDAP username is a real per-user identity and
  stays as is.
- Any change to how the custom login is validated.
- Migrating existing `stream_history` rows. Old rows keep the login name in
  `username`.
- New API fields, new columns, or suite (`stream-share-suite`) changes. The
  suite already treats IP-shaped viewer IDs as aliasable.

## Design

In `authWithPathCredentials`, after credentials are validated, choose the
tracking identity once:

```go
identity := username
if !c.LDAPEnabled {
    identity = ip
}
```

Use `identity` in both places that previously used `username`:

- `c.sessionManager.RegisterUser(identity, ip, userAgent)`
- `ctx.Set("username", identity)`

`resolveRequestUsername` then returns the IP for these routes without
modification, because it reads `ctx.GetString("username")` first. Downstream
handlers (`multiplexedStream`, the VOD/cache handlers) need no change.

The login name is still logged by the existing `DebugLog` lines and is not
otherwise recorded.

## Behavior after the change

| Mode | Route | Viewer identity |
| --- | --- | --- |
| LDAP off, provider creds | direct Xtream routes | client IP (unchanged) |
| LDAP off, custom creds | proxy-credential routes | client IP (**was** custom login) |
| LDAP on | proxy-credential routes | LDAP username (unchanged) |

## Trade-offs

- The custom login name no longer appears in sessions, history or `/status`.
- Devices behind one NAT share an identity, same as the provider-credentials
  path today.
- Existing history rows keep the old login-name `username` next to the
  new IP-keyed rows. Anything grouping history by `username` sees both.
- With no LDAP, `RegisterUser` looks up `GetDiscordByLDAPUser(identity)`, so a
  Discord mapping made against the shared login no longer attaches to the
  session; `DiscordID`/`DiscordName` stay empty (as on the provider-credentials
  path).
- `/disconnect` and `/timeout` (`handlers_users.go`) act on the identity
  verbatim, so admins must now target the client IP; `/disconnect <custom-login>`
  becomes a no-op.
- `createVODDownload`'s "user is watching live" guard (`handlers_vod.go`) looks
  up a session by the Discord-linked name and stops matching for LDAP-off
  viewers, as it already does on the provider-credentials path.

## Testing

Add a Go test in `pkg/server` (alongside `auth_test.go`) exercising
`authWithPathCredentials` through a gin test router:

1. `LDAPEnabled=false`, valid custom creds, request from IP `A` then IP `B`:
   two sessions exist, keyed `A` and `B`, each with its own `IPAddress`; the
   custom login is not a session key.
2. `LDAPEnabled=false`, invalid creds: 401, no session registered.
3. `LDAPEnabled=true` with `LDAPAuthCacheMinutes > 0`, and a successful
   result pre-seeded in `ldapAuthCache` via `ldapCacheKey` (the technique
   `TestLDAPCacheHitAvoidsDirectory` in `auth_cache_test.go` uses, so no
   directory is contacted): session is keyed by the LDAP username, not the IP.

No suite (`node --test`) or frontend changes.
