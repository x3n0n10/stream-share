# Runtime version endpoint

## Context

A running stream-share instance cannot tell anyone which version it is. The
version exists only as an image label (`org.opencontainers.image.version`, set by
GoReleaser), so a dashboard that manages instances can read it off the image but
not from the instance itself. `stream-share-suite` (the operations layer that
deploys and monitors instances) shows the running version of each instance in
its sidebar and wants the instance's own answer, falling back to the image label
for releases that predate this change.

This spec adds one authenticated endpoint that reports the version, and stamps
that version into every build path.

## Scope

**In scope:** `GET /api/internal/version`, a `pkg/version` package holding the
value, and link-time stamping in GoReleaser, the manual dev build and the root
`Dockerfile`.

**Out of scope:** commit or build-date fields, a startup banner line, any
endpoint outside `/api/internal`, and the consumer in `stream-share-suite`
(a separate change in that repository, see "Consumer").

## Endpoint

`GET /api/internal/version`, registered in `setupInternalAPI`
(`pkg/server/api.go`) alongside the other endpoints, inside the existing
`/api/internal` group. That group already applies `c.apiKeyAuth()`, so the route
is authenticated with no extra wiring, exactly like its siblings.

The response uses the envelope every internal route uses (`types.APIResponse`):

```json
{ "success": true, "data": { "version": "1.4.2" } }
```

The handler is `getVersion` in a new file `pkg/server/handlers_version.go`,
following the shape of `getInstanceInfo`: a method on `*Config` that writes
`types.APIResponse{Success: true, Data: map[string]interface{}{"version": version.Version}}`
with status 200. It cannot fail, so there is no error path.

## Version source

A new package `pkg/version` with a single exported variable:

```go
// Version is the release this binary was built from. It is set at link time
// (see .goreleaser.yaml); a build made without that reports "dev".
var Version = "dev"
```

A separate package rather than a variable in `package main` keeps the value
importable from `pkg/server` without threading it through `cmd.Execute` and the
server config.

## Stamping

All three paths set `-X github.com/lucasduport/stream-share/pkg/version.Version=<value>`.
The module path is the upstream one; the fork keeps it.

- **`.goreleaser.yaml`**: add `ldflags` to the build:
  `-s -w -X github.com/lucasduport/stream-share/pkg/version.Version={{.Version}}`.
  Setting `ldflags` explicitly replaces GoReleaser's defaults, so `-s -w` is
  restated. `{{.Version}}` is the tag with its leading `v` removed, the same
  value the existing image label `org.opencontainers.image.version={{.Version}}`
  already carries, so the endpoint and the label agree for a release.
- **`.github/workflows/dev-build.yml`**: both `go build` lines (amd64 and arm64)
  gain `-ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=dev-${SHORT_SHA}"`.
  The workflow already exports `SHORT_SHA` to the environment for its image tags.
- **`Dockerfile`** (root): `ARG VERSION=dev` in the build stage and
  `-ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=${VERSION}"`
  on the existing `go build -o stream-share .`. A plain `docker build` reports
  `dev`; `--build-arg VERSION=...` overrides it.

## Testing

Go tests in the existing style (call the handler with a gin test context, as
`handlers_health_test.go` does):

- `pkg/version`: the default value is `"dev"`.
- `pkg/server`: `getVersion` answers 200 with `success: true` and
  `data.version` equal to `version.Version`, with `version.Version` overridden
  to a known value for the test and restored afterwards.
- Route placement is checked by reading the registration in `setupInternalAPI`
  during review (it sits inside the group that applies `apiKeyAuth`). No test
  in the repository currently exercises the internal-API auth boundary, so this
  placement is review-verified rather than test-covered; a router-level test
  asserting 401 without a key would protect every internal route and is a
  worthwhile separate change, not part of this one.

Verification also includes `go vet ./...`, `go test -mod vendor ./...` and
`go build -mod vendor` (what CI runs). The stamping itself is checked locally by
building with `-ldflags "-X ...Version=x.y.z"` and confirming the value is
present in the binary (`go version -m` does not show `-X`; use `strings` on the
binary, or a one-off test binary that prints it). GoReleaser is not run locally;
its config change is verified by the next tagged release.

## Consumer (not part of this change)

`stream-share-suite` shows each instance's version in its sidebar. Its spec
assumed a bare `{"version": ...}` body; the real shape is the envelope above, so
its `fetchVersion` needs a small change to read `data.version`. That is a
separate pull request in the Suite after this ships. Until an instance runs a
release containing this endpoint, the Suite gets a 404 and falls back to the
image label, so the two changes can land in either order.

## Rollout

1. Merge this change.
2. The maintainer tags the next release; GoReleaser stamps the version.
3. The Suite's follow-up change is merged; its sidebar then shows the true
   running version for instances on that release.
