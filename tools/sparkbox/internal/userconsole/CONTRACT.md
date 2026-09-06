# The console's own contract

`console.go` and `index.html` are two languages sharing one API surface with
no compiler checking that they agree. This file exists because that surface
has already drifted once silently: the visibility-toggle mutation built its
in-flight key as `vis:<sub>:<port>` in one function and `vis:<sub>` in
another, so `inflight.has()` could never match and a route-visibility button
rebuilt mid-request by the 4s poll came back clickable instead of staying
disabled. Nothing failed loudly — `go build`, `go vet` and every existing test
stayed green — because the mismatch is a fact about two string templates
agreeing, and nothing but a human reading both sides checks that.

This is not the public REST API (`internal/restapi`'s OpenAPI-documented
`/v1/...` surface, meant for third parties and the CLI). It is
console-internal: shaped for exactly the one SPA in this package, free to
change in lockstep with it, and undocumented anywhere else.

## Routes

One row per route `console.go`'s `Handler()` registers, its handler, and
anything about its request/response shape that is not obvious from the field
name. This intentionally does not retype every JSON key — the Go struct is
the source of truth for that and a second copy here would just be a second
place to forget to update — only the parts a reader would otherwise have to
find by reading the handler.

| Route | Handler | Notes |
|---|---|---|
| `GET /api/me` | `me` | terminal/launch subdomains, operator flag |
| `POST /api/logout` | `logout` | |
| `GET /api/machines` | `machines` | returns `[]sandboxView`; see **Missing vs. zero** below for `mem_used_mb`/`cpu_seconds` |
| `GET /api/usage` | `usage` | pooled footprint (disk reflink baseline, memory pool/burst) |
| `POST /api/machines/{name}/pause` \| `/resume` \| `/archive` \| `/pin` \| `/unpin` \| `/reboot` | `pause`, `resume`, `archive`, `pin`, `unpin`, `reboot` | no body |
| `DELETE /api/machines/{name}` | `destroy` | |
| `POST /api/machines/{name}/snapshot` | `snapshot` | body: `{snapshot_name}` |
| `POST /api/machines/{name}/rename` | `rename` | body: `{new_name}` |
| `POST /api/machines/{name}/turbo` | `turbo` | body: `{on: bool}` |
| `POST /api/machines/{name}/port` | `setPort` | body: `{port}` |
| `PUT /api/machines/{name}/tags` | `setTags` | body: `{tags: []string}`, full replace not a delta |
| `POST /api/routes/{subdomain}/visibility` | `setVisibility` | body: `visibilityReq{visibility, port}`. **`port: 0` means the route's own (portless) default port, not "unspecified" — a route can list several ports, each independently public/private, each with its own button.** See `visibilityReq`'s own comment. |
| `DELETE /api/routes/{subdomain}/ports/{port}` | `forgetPort` | per-port, same as visibility above — removes the row entirely (unlike setting it private, which keeps the row) |
| `GET /api/snapshots` | `listSnapshots` | |
| `POST /api/snapshots/{snapshot}/fork` | `fork` | body: `{name}` — owner is always the session handle, never request data |
| `POST /api/snapshots/{snapshot}/delete` | `deleteSnapshot` | no body — same reason |
| `GET /api/secrets` | `listSecrets` | **501** when the encryption key is unavailable — the tab stays visible with an explanation rather than disappearing |
| `PUT` \| `DELETE /api/secrets/{env_name}` | `putSecret`, `deleteSecret` | |
| `GET /api/network-rules` | `listNetRules` | **501** when network rules aren't enabled on this host |
| `PUT` \| `DELETE /api/network-rules/{name}` | `putNetRule`, `deleteNetRule` | |
| `GET /api/environments` | `listEnvironments` | **501** when there's no environment store on this host |
| `PUT` \| `DELETE /api/environments/{name}` | `putEnvironment`, `deleteEnvironment` | |
| `GET` \| `PUT /api/environments/{name}/script` | `getEnvScript`, `putEnvScript` | the setup script editor |
| `POST /api/environments/{name}/script/from-repo` | `adoptRepoScript` | |
| `POST /api/environments/{name}/build` \| `/capture` | `buildEnvironment`, `captureEnvironment` | |
| `GET /api/repos` | `listRepos` | **501** when repo attachments aren't enabled on this host |
| `POST /api/repos/{slug}/authorize` | `authorizeRepo` | |
| `PUT` \| `DELETE /api/repos/{slug}` | `putRepo`, `deleteRepo` | |
| `GET /api/machines/{name}/bandwidth` | `bandwidth` | per-domain egress for one machine, fetched only for panels the visitor has opened |
| `GET /api/favicon` | `favicon` | proxied favicon fetch, for the egress panel's domain list |

Three list endpoints (`secrets`, `network-rules`, `environments`, `repos`)
answer **501, not an error toast**, when the subsystem is disabled on this
host — `index.html`'s `refresh()` checks `.status === 501` before falling
through to the generic failure branch for each of them. A new disableable
subsystem should follow the same convention, on both sides.

## The mutate-key invariant

`index.html`'s `mutate(key, btn, fn)` disables `btn`, runs `fn`, and records
`key` in the `inflight` `Set` until `fn` settles. The 4s poll can rebuild the
whole card mid-request; `reapplyInflight()` walks the rebuilt buttons and
re-disables any whose `inflightKey(btn)` is in `inflight` — otherwise a
rebuilt button comes back clickable while its own request is still in
flight, and a second click double-submits.

**The invariant this depends on:** the key string built at the `mutate()`
call site and the key string built by the matching branch of `inflightKey()`
must be constructed byte-for-byte identically. If a mutation's identity
includes a field beyond its main dataset name — a port, a tag, an env
name — that field belongs in the key on both sides, built by one shared
function, not typed out twice. Two call sites typing out "the same" string is
exactly how `vis:` drifted (and `forget:` drifted with it — it was never
wired into `inflightKey()` at all, the same class of gap, fixed alongside
`vis:`).

Current key templates, by dataset attribute:

| Attribute | Key | Built by |
|---|---|---|
| `data-act` | `act:<name>:<act>` | inline at both sites |
| `data-turbo` | `turbo:<name>` | inline |
| `data-snap` | `snap:<name>` | inline |
| `data-vis` | `vis:<sub>:<port>` | `visKey(sub, port)` |
| `data-forget` | `forget:<sub>:<port>` | `forgetKey(sub, port)` |
| `data-untag` | `tags:<name>` | inline |
| `data-fork` | `fork:<snapshot>` | inline |
| `data-del` | `delsnap:<snapshot>` | inline |
| `data-delSecret` | `delsecret:<name>` | inline |
| `data-delRepo` | `delrepo:<slug>` | inline |
| `data-envbuild` / `-envcapture` / `-envfromrepo` / `-envdel` | `env*:<name>` | inline |

Only `vis` and `forget` carry a second identifying field beyond their main
name, which is why they're the two with a shared builder function — a new
mutation kind should ask the same question (does this key need more than one
field to be unique?) before deciding whether it needs one too.

## Missing vs. zero

A handful of stats can legitimately be "not sampled yet" rather than zero,
and the wire format has to be able to say which:

- `sandboxView.CPUSeconds` (`cpu_seconds`) and `MemUsedMB` (`mem_used_mb`) are
  `*float64`/`*int64` with `json:",omitempty"` — **the key is omitted
  entirely** when there's no reading yet (a machine that isn't `running`),
  not sent as `0`. `index.html`'s `cpuPct()` checks
  `typeof b.cpu_seconds !== "number"` for exactly this reason; a rendering
  path that instead did `b.cpu_seconds || 0` would silently paint "0%" for
  "no data" and a real idle 0% identically.
- The CPU percentage itself is never sent by the server at all — it is
  computed client-side from the delta between two polls' `cpu_seconds`
  (`cpuPct()` in `index.html`), so "no baseline yet" (first poll, or first
  poll after a machine starts) is a third state on top of the two above,
  rendered as "–" rather than a percentage. Only `renderMachines()`'s own
  poll-driven call may advance that baseline — see its `sample` parameter —
  because anything else that repaints from stale data would recompute a
  delta against an unchanged sample and read out wrong until the next real
  poll self-heals it.

A new numeric stat should decide up front which of these three states it can
be in, and use `omitempty` on a pointer type for "not sampled" rather than
overloading zero — the same choice `sandboxView` already made.
