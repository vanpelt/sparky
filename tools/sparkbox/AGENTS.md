# Sparkbox Agent Guide

## Product and invariants

- Sparkbox is an agentic sandbox service implemented as a Go binary. It exposes the same sandbox lifecycle through the smart SSH gateway, the authenticated REST API, browser terminals, and web proxying.
- Preserve standalone operation. `sparkbox serve --driver mock` is the portable development and integration path; `--driver firecracker` is the real Linux/KVM path.
- User identity and ownership checks are security boundaries. Do not leak whether another user's sandbox, route, snapshot, schedule, or account resource exists.
- Resume-on-connect, sandbox naming, reserved names, route ownership, and typed control errors are cross-surface behavior. Changes must remain consistent across SSH, REST, xterm, proxy, and consoles.

## Architecture

- `cmd/sparkbox`: command wiring for `serve`, `setup`, `doctor`, secrets, TLS, and platform-specific driver construction. Keep orchestration here; put domain behavior in `internal` packages.
- `internal/ctlops`: transport-independent control-plane operations and types. SSH and REST should delegate here instead of reimplementing validation, authorization, timeouts, or lifecycle logic.
- `internal/sshgw`, `internal/restapi`, `internal/xterm`: transport adapters. Keep their protocol concerns local and their domain behavior aligned through `ctlops`.
- `internal/host`: single-node sandbox records, lifecycle, capacity, snapshots/archives, and idle reaping. It coordinates the driver and optional driver capabilities.
- `internal/vmm`: driver contract. `vmm/mock` must remain usable without KVM; `vmm/firecracker` owns real microVM, tap, rootfs, and snapshot behavior.
- `internal/proxy`, `internal/frontdoor`, `internal/dnsedge`, `internal/edgeauth`: public HTTP/DNS edge, routing, TLS-adjacent plumbing, and browser/session authentication.
- `internal/users`, `internal/secrets`, `internal/routes`, `internal/schedule`, `internal/netrules`: durable control-plane stores. Preserve ownership boundaries, migrations, and restart behavior.
- `internal/api` is a legacy loopback-only CRUD API. Do not mount it on the public edge or extend it as the primary public contract.
- `deploy`, `hack`, and `images` build and provision host/guest artifacts. Changes here can affect both `linux/amd64` and `linux/arm64` releases.

## Public contracts

- A control operation should have one implementation in `internal/ctlops`, with thin SSH and REST representations around it. Add focused `ctlops` tests plus transport tests when wire behavior changes.
- `internal/restapi/openapi.json` is the canonical, hand-authored OpenAPI document embedded by `openapi.go`. Update it with public REST routes, schemas, examples, errors, or status codes; keep `openapi_test.go` and route/schema tests passing.
- Preserve stable error `code` values once shipped. Clients should be able to match codes without parsing messages.
- When adding or renaming a public hostname/subdomain, update reserved-name rules and their tests as well as proxy, DNS, certificate, terminal, and documentation assumptions.
- When flags, environment variables, defaults, release assets, systemd units, or host prerequisites change, update the relevant files in `docs/`, `deploy/`, `.github/workflows/`, and the README together.

## Embedded web assets

- The consoles and terminal UI are hand-authored HTML/CSS/JS embedded into the Go binary. There is no separate frontend build step.
- Shared console assets live in `internal/webui`; page templates embed its marker blocks and are minified at package initialization. Preserve the marker and `go:embed` relationships.
- `internal/xterm/assets` contains vendored xterm.js assets. Do not casually edit minified vendor files; record intentional upgrades and exercise terminal asset/bridge tests.
- UI changes should stay usable at narrow and wide viewports and must preserve authentication, origin, WebSocket, and reconnect behavior. Use `hack/preview-console.py` only as a preview aid; Go tests remain the contract.

## Implementation practices

- Run `gofmt` on every touched Go file. Follow existing package boundaries and use narrow interfaces where `ctlops` or driver capabilities already define them.
- Prefer table-driven or focused package tests. Use `t.TempDir`, mock drivers, in-process SSH/HTTP servers, and fake clocks/hooks where existing tests do; avoid tests that depend on external services.
- Keep platform-specific implementations behind `_linux.go` and provide portable stubs or skips where the package must compile elsewhere.
- Do not weaken filesystem permissions, key handling, cookie/auth checks, SSH public-key verification, or guest network isolation for test convenience.
- Treat persisted JSON/SQLite shapes and on-disk VM state as compatibility surfaces. Add migration/backward-compatibility coverage when changing them.

## Verification

Run from `tools/sparkbox`:

```sh
# During iteration: choose the packages affected by the change.
go test ./internal/ctlops ./internal/restapi

# Before completion when practical.
go test ./...
go vet ./...
go build ./...
```

- Replace the example targeted packages with the packages actually touched.
- `go test ./...` exercises the mock-driver end-to-end stack without KVM. Firecracker, real networking, systemd, release-image, and deployment changes still need explicit Linux/KVM or host validation; state clearly when that was not available.
- For embedded web changes, run the owning package tests and inspect the served page in a browser when layout or interaction changed.
