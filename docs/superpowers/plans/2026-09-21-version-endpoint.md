# Runtime Version Endpoint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let a running stream-share instance report its own version at `GET /api/internal/version`, and stamp that version into every build path (GoReleaser release, manual dev build, root `Dockerfile`).

**Architecture:** A one-variable package `pkg/version` (`Version = "dev"`, overwritten at link time with `-X`). A small handler `getVersion` returns it in the house `types.APIResponse` envelope, registered inside the existing `/api/internal` group so `apiKeyAuth` already applies. Three build paths get an `-ldflags "-X .../pkg/version.Version=..."`.

**Tech Stack:** Go (module `github.com/lucasduport/stream-share`, vendored deps, gin), GoReleaser, GitHub Actions, Dockerfile.

**Design spec:** `docs/superpowers/specs/2026-09-21-version-endpoint-design.md`

## Global Constraints

- Work only in the worktree `/Users/jorislankhorst/Source/stream-share-wt-version-endpoint`, branch `claude/version-endpoint` (off `origin/master`). Do not touch `/Users/jorislankhorst/Source/stream-share` (the main checkout). Do not push the branch or any tag; the controller does that.
- The worktree's git identity is already the GitHub noreply address (`20641331+x3n0n10@users.noreply.github.com`, name `x3n0n10`); do not change it. GitHub rejects pushes carrying the personal address.
- Commit messages end with `Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>`. Plain `git commit` works in this repo. If a hook ever blocks it with `simplify-guard: ...`, commit with plumbing (`git write-tree`, `git commit-tree -p HEAD -m ...`, `git update-ref refs/heads/claude/version-endpoint <sha>`), verifying `git diff --cached --stat` is non-empty first.
- Go tooling: always `-mod vendor` (dependencies are vendored; do not run `go mod tidy`/`go get`, do not touch `vendor/` or `go.mod`). Baseline before this plan: `go vet -mod vendor ./...` clean and `go test -mod vendor ./...` all green.
- The module path is `github.com/lucasduport/stream-share` (the fork keeps the upstream path). The ldflags variable is exactly `github.com/lucasduport/stream-share/pkg/version.Version`.
- New Go files carry this exact licence header (files the fork authored name only the maintainer):
  ```go
  /*
   * stream-share is a project to efficiently share the use of an IPTV service.
   * Copyright (C) 2026  x3n0n10
   *
   * This program is free software: you can redistribute it and/or modify
   * it under the terms of the GNU General Public License as published by
   * the Free Software Foundation, either version 3 of the License, or
   * (at your option) any later version.
   *
   * This program is distributed in the hope that it will be useful,
   * but WITHOUT ANY WARRANTY; without even the implied warranty of
   * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   * GNU General Public License for more details.
   *
   * You should have received a copy of the GNU General Public License
   * along with this program.  If not, see <https://www.gnu.org/licenses/>.
   */
  ```
  `pkg/server/api.go` already carries both copyright lines; leave its header alone.
- Match existing style: gofmt'd code; comments explain *why*; handler doc comments start with the method and path, like `getInstanceInfo`.
- Response shape (exact): `{"success": true, "data": {"version": "<string>"}}`.
- Ruby is available for YAML assertions (`ruby -ryaml`); `docker`, `actionlint` and `goreleaser` are not installed. Write assertion scripts to a scratch file outside the repo (`/private/tmp/claude-501/-Users-jorislankhorst-Claude/83ef7f57-0172-4320-ba0f-424f637c7b19/scratchpad/`), not into the worktree.

## File Structure

- Create: `pkg/version/version.go` — the `Version` variable.
- Create: `pkg/version/version_test.go` — default is `dev`.
- Create: `pkg/server/handlers_version.go` — `getVersion` handler.
- Create: `pkg/server/handlers_version_test.go` — handler test.
- Modify: `pkg/server/api.go` — register the route (Task 1).
- Modify: `.goreleaser.yaml`, `.github/workflows/dev-build.yml`, `Dockerfile` — stamping (Task 2).

---

## Task 1: `pkg/version` and the `/api/internal/version` endpoint

**Files:**
- Create: `pkg/version/version.go`, `pkg/version/version_test.go`
- Create: `pkg/server/handlers_version.go`, `pkg/server/handlers_version_test.go`
- Modify: `pkg/server/api.go` (one route + comment, after the instance/stats routes)

**Interfaces:**
- Consumes: `types.APIResponse` (`pkg/types`), `Config` (`pkg/server`), `config.ProxyConfig` (`pkg/config`).
- Produces: package `version` with `var Version string` (default `"dev"`); `(*Config).getVersion(*gin.Context)`; the route `GET /api/internal/version`. Task 2 stamps `version.Version` at link time.

- [ ] **Step 1: Write the failing tests**

Create `pkg/version/version_test.go`:

```go
/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package version

import "testing"

func TestDefaultIsDev(t *testing.T) {
	if Version != "dev" {
		t.Errorf("Version = %q, want %q: a build made without -ldflags must report dev", Version, "dev")
	}
}
```

Create `pkg/server/handlers_version_test.go`:

```go
/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/version"
)

func TestGetVersionReportsTheBuildVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previous := version.Version
	version.Version = "1.2.3-test"
	t.Cleanup(func() { version.Version = previous })

	c := &Config{ProxyConfig: &config.ProxyConfig{}}
	w := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(w)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/internal/version", nil)

	c.getVersion(ctx)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	var body struct {
		Success bool `json:"success"`
		Data    struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v\n%s", err, w.Body.String())
	}
	if !body.Success {
		t.Errorf("success = false, want true")
	}
	if body.Data.Version != "1.2.3-test" {
		t.Errorf("data.version = %q, want %q", body.Data.Version, "1.2.3-test")
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run (in the worktree): `go test -mod vendor ./pkg/version/ ./pkg/server/`
Expected: build failures: `pkg/version` has no non-test Go files / `Version` undefined, and `pkg/server` reports `c.getVersion undefined` and a missing `pkg/version` import.

- [ ] **Step 3: Create `pkg/version/version.go`**

```go
/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

// Package version holds the release this binary was built from.
package version

// Version is set at link time (see .goreleaser.yaml, the Dockerfile and the
// dev-build workflow). A build made without that reports "dev".
var Version = "dev"
```

- [ ] **Step 4: Create `pkg/server/handlers_version.go`**

```go
/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2026  x3n0n10
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package server

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/version"
)

// getVersion GET /api/internal/version — the release this instance is running,
// so a dashboard managing several instances can show it without having to
// inspect the container image. A build made without a stamped version reports
// "dev".
func (c *Config) getVersion(ctx *gin.Context) {
	ctx.JSON(http.StatusOK, types.APIResponse{
		Success: true,
		Data:    map[string]interface{}{"version": version.Version},
	})
}
```

- [ ] **Step 5: Register the route in `pkg/server/api.go`**

Replace these existing lines:

```go
	// Dashboard endpoints: instance identity (for multi-instance aggregation)
	// and aggregate/leaderboard stats
	api.GET("/instance", c.getInstanceInfo)
	api.GET("/stats", c.getDashboardStats)
```

with:

```go
	// Dashboard endpoints: instance identity (for multi-instance aggregation)
	// and aggregate/leaderboard stats
	api.GET("/instance", c.getInstanceInfo)
	api.GET("/stats", c.getDashboardStats)

	// The release this instance was built from, for dashboards that show what
	// each managed instance is running
	api.GET("/version", c.getVersion)
```

The route is inside the `api := r.Group("/api/internal")` group, which already has `api.Use(c.apiKeyAuth())`; do not add auth separately.

- [ ] **Step 6: Run the new tests, then vet and the full suite**

Run: `go test -mod vendor ./pkg/version/ ./pkg/server/`
Expected: both `ok`, including `TestDefaultIsDev` and `TestGetVersionReportsTheBuildVersion` (`go test -mod vendor -run 'Version' -v ./pkg/version/ ./pkg/server/` shows them by name).
Run: `gofmt -l pkg/` — expected: no output.
Run: `go vet -mod vendor ./...` — expected: no output.
Run: `go test -mod vendor ./...` — expected: every package `ok` or `no test files`.

- [ ] **Step 7: Commit**

Stage `pkg/version/version.go`, `pkg/version/version_test.go`, `pkg/server/handlers_version.go`, `pkg/server/handlers_version_test.go`, `pkg/server/api.go`. Subject: `Add GET /api/internal/version reporting the build version`. Body: new `pkg/version` package (default `dev`, overwritten at link time), authenticated route in the `/api/internal` group returning the house envelope.

---

## Task 2: Stamp the version into every build path

**Files:**
- Modify: `.goreleaser.yaml` (the `builds:` entry)
- Modify: `.github/workflows/dev-build.yml` (the two `go build` lines)
- Modify: `Dockerfile` (builder stage)

**Interfaces:**
- Consumes: `version.Version` from Task 1 at `github.com/lucasduport/stream-share/pkg/version.Version`.
- Produces: release binaries report the tag without its leading `v` (GoReleaser `{{.Version}}`, the same value as the existing `org.opencontainers.image.version` label); dev builds report `dev-<shortsha>`; a plain `docker build` reports `dev`, or `--build-arg VERSION=...`.

- [ ] **Step 1: Write the failing assertion**

Save as `check-stamping.rb` in the scratch dir; run it from the worktree root:

```ruby
require "yaml"

VAR = "github.com/lucasduport/stream-share/pkg/version.Version"

gr = YAML.load_file(".goreleaser.yaml")
ldflags = gr["builds"][0]["ldflags"]
abort "goreleaser ldflags missing" unless ldflags.is_a?(Array) && !ldflags.empty?
expected = "-s -w -X #{VAR}={{.Version}}"
abort "goreleaser ldflags must be exactly: #{expected} (got #{ldflags.inspect})" unless ldflags.include?(expected)

dev = File.read(".github/workflows/dev-build.yml")
builds = dev.lines.select { |l| l.include?("go build") }
abort "expected two go build lines in dev-build.yml, found #{builds.size}" unless builds.size == 2
builds.each do |l|
  abort "dev build line lacks the stamp: #{l}" unless l.include?(%(-ldflags "-X #{VAR}=dev-${SHORT_SHA}"))
end
abort "no ${{ inside a go build line" if builds.any? { |l| l.include?("${{") }

docker = File.read("Dockerfile")
abort "Dockerfile ARG VERSION=dev missing" unless docker.include?("ARG VERSION=dev")
abort "Dockerfile stamp missing" unless docker.include?(%(-ldflags "-X #{VAR}=${VERSION}"))
build_stage = docker.split(/^FROM /)[1]
abort "ARG must be declared in the build stage" unless build_stage.include?("ARG VERSION=dev")
abort "ARG must come before the go build line" unless build_stage.index("ARG VERSION=dev") < build_stage.index("go build")

puts "stamping ok"
```

- [ ] **Step 2: Run it and confirm it fails**

Run: `ruby <scratch>/check-stamping.rb`
Expected: aborts with `goreleaser ldflags missing` (the entry has no `ldflags` yet; `nil` is not an Array).

- [ ] **Step 3: Edit `.goreleaser.yaml`**

In the single `builds:` entry, add `ldflags` directly after the `flags:` block so it reads:

```yaml
builds:
  - binary: stream-share
    env:
      - CGO_ENABLED=0
    flags:
      - -mod=vendor
    ldflags:
      - -s -w -X github.com/lucasduport/stream-share/pkg/version.Version={{.Version}}
    goos:
```

(Setting `ldflags` explicitly replaces GoReleaser's defaults, which is why `-s -w` is restated.) Change nothing else in the file.

- [ ] **Step 4: Edit `.github/workflows/dev-build.yml`**

Replace the amd64 build line:

```yaml
          GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=vendor -o stream-share .
```

with:

```yaml
          GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -mod=vendor -ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=dev-${SHORT_SHA}" -o stream-share .
```

and the arm64 build line:

```yaml
          GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=vendor -o stream-share .
```

with:

```yaml
          GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -mod=vendor -ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=dev-${SHORT_SHA}" -o stream-share .
```

`SHORT_SHA` is already exported to the job environment by the existing "Set short SHA" step, so `${SHORT_SHA}` is an ordinary shell variable inside these `run:` scripts. Do not use a `${{ }}` expression here.

- [ ] **Step 5: Edit the root `Dockerfile`**

In the builder stage, replace:

```dockerfile
# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -o stream-share .
```

with:

```dockerfile
# Stamped into the binary so GET /api/internal/version can report it; a plain
# build reports "dev".
ARG VERSION=dev

# Build static binary
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=${VERSION}" -o stream-share .
```

- [ ] **Step 6: Run the assertion**

Run: `ruby <scratch>/check-stamping.rb`
Expected: prints `stamping ok`.

- [ ] **Step 7: Prove the link-time stamp works**

`docker` and `goreleaser` are not installed, so exercise the same `-X` mechanism with the local Go toolchain. From the worktree root:

```bash
go build -mod vendor \
  -ldflags "-X github.com/lucasduport/stream-share/pkg/version.Version=9.9.9-stamp-check" \
  -o /private/tmp/claude-501/-Users-jorislankhorst-Claude/83ef7f57-0172-4320-ba0f-424f637c7b19/scratchpad/stream-share-stamped .
strings /private/tmp/claude-501/-Users-jorislankhorst-Claude/83ef7f57-0172-4320-ba0f-424f637c7b19/scratchpad/stream-share-stamped | grep -c "9.9.9-stamp-check"
go build -mod vendor -o /private/tmp/claude-501/-Users-jorislankhorst-Claude/83ef7f57-0172-4320-ba0f-424f637c7b19/scratchpad/stream-share-plain .
strings /private/tmp/claude-501/-Users-jorislankhorst-Claude/83ef7f57-0172-4320-ba0f-424f637c7b19/scratchpad/stream-share-plain | grep -c "9.9.9-stamp-check"
```

Expected: the first `grep -c` prints a number greater than 0 (the stamped value is in the binary); the second prints `0`. Delete both binaries afterwards. Do not leave a `stream-share` binary in the worktree (`git status` must show only the three modified files).

- [ ] **Step 8: Final verification**

Run: `go vet -mod vendor ./...` (expected: no output) and `go test -mod vendor ./...` (expected: all `ok`/`no test files`). Run `git status --short`: expected exactly `.goreleaser.yaml`, `.github/workflows/dev-build.yml`, `Dockerfile` modified.

- [ ] **Step 9: Commit**

Stage `.goreleaser.yaml`, `.github/workflows/dev-build.yml`, `Dockerfile`. Subject: `Stamp the build version into releases, dev builds and the Dockerfile`. Body: GoReleaser passes `{{.Version}}` (same value as the image label), the manual dev build passes `dev-<shortsha>`, the root Dockerfile takes `ARG VERSION` (default `dev`); all via `-X .../pkg/version.Version`.

---

## Self-Review

**Spec coverage.**
- Endpoint in `setupInternalAPI`, inside the authenticated group, house envelope, handler in `handlers_version.go`: Task 1.
- `pkg/version` package with `Version = "dev"`: Task 1.
- GoReleaser ldflags with `-s -w` restated and `{{.Version}}`; both dev-build `go build` lines; root Dockerfile `ARG VERSION=dev`: Task 2.
- Tests: default is `dev`, handler 200 + envelope + overridden version (Task 1); route placement inside the authenticated group is by construction in Task 1 Step 5 and checked in review; link-time stamp proven locally in Task 2 Step 7 (replaces the spec's `strings` suggestion with an explicit stamped-vs-plain comparison).
- Verification `go vet`, `go test -mod vendor ./...`, `go build`: Tasks 1 and 2.
- Consumer (Suite) and rollout: out of this plan; the spec covers them.

**Placeholders.** None.

**Consistency.** The ldflags variable path, the envelope shape and the `dev` default are identical across the tests, handler, and the three stamping edits.

## Rollout (after merge; not plan tasks)

1. Maintainer tags the next stream-share release; GoReleaser stamps `{{.Version}}`. The first release image's `GET /api/internal/version` should answer the same string as its `org.opencontainers.image.version` label.
2. Then the Suite follow-up PR (`fetchVersion` reads `data.version` from the envelope).
