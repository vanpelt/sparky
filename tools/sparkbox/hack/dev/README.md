# Local dev environment

Sparkbox's real home is the CoreWeave Kubernetes deployment, so the honest way to
try a change has been to deploy it. This directory is the first half of doing
that locally: run the **production** entrypoints against a working-tree binary,
never a hand-written imitation of them.

Two tiers, in order of how much of the Pod they reproduce and how long they take:

| tier | what runs | what it is for |
| --- | --- | --- |
| **native gateway** — `hack/dev/gateway.sh` | `deploy/kubernetes/gateway-entrypoint.sh`, unmodified, on macOS, no container | everything gateway-side: edge proxy, auth and passkeys, consoles, REST API, launch links, SSH doors, roster, OIDC |
| **five-container pod** — `sparkbox devpod` | the whole node Pod, rendered from `deploy/kubernetes/*.yaml` into docker argv by `internal/devpod` | uids, capabilities, devices, mounts, init ordering — the shape a real sandbox boots in |

The rendering half of the pod tier is a separate piece of work
(`internal/devpod`, `sparkbox devpod`). What this README covers is the gateway
tier plus everything the pod tier needs to exist before it can run: the local
registry, the Apple container machine, and the image that moves between them —
`registry.sh`, `machine.sh`, `image.sh`.

## The gateway loop

```sh
hack/dev/gateway.sh start      # build ./cmd/sparkbox, mint identity on first run, serve
hack/dev/gateway.sh restart    # rebuild + restart: the edit-to-observable loop
hack/dev/gateway.sh status     # pid, ports, and the edge's answer to the console
hack/dev/gateway.sh logs -f    # follow; `logs 200` for the last N lines
hack/dev/gateway.sh stop
```

Measured on an M4 Max: `restart` is **1.7s** when nothing recompiled and **~6s**
after a change that forces a relink of the 42MB binary (4.6s of that is
`go build`). A cold build is ~10s. Nothing is cached between you and what the
server is serving — the binary the entrypoint execs is the one `go build` just
produced.

Then:

```
console  http://my.dev.localhost:8081     # browsers resolve *.localhost with no /etc/hosts
api      http://api.dev.localhost:8081/openapi.json
ssh      ssh -p 2222 ctl@127.0.0.1
```

`curl` does not do the `*.localhost` shortcut, so send the name in a header:
`curl -H 'Host: my.dev.localhost' http://127.0.0.1:8081/`.

## What it actually runs

`gateway.sh` execs `deploy/kubernetes/gateway-entrypoint.sh` — the same file
`/usr/local/sbin/sparkbox-gateway-entrypoint` is built from. That script is
entirely environment-driven, so the only change it needed was
`exec "${SPARKBOX_BIN:-/usr/local/bin/sparkbox}"`; in the Pod nothing sets
`SPARKBOX_BIN` and the path is the image's. Dev-only listen addresses are
appended as `"$@"`, which the entrypoint already forwards and Go's flag package
resolves last-wins, so no production flag is edited to make this work.

State lives in `.dev/` (gitignored) and nothing outside it is touched:

```
.dev/bin/sparkbox                        the working-tree build
.dev/gateway/keys/                       the five fleet private keys, 0600
.dev/gateway/durable/gateway/control/    SPARKBOX_STATE_DIR — sqlite, certs, sandboxes.json
.dev/gateway/users.conf                  operator handle + your ~/.ssh public key
.dev/gateway/gateway.log
```

Throw the whole environment away with `rm -rf .dev/gateway`; the next `start`
rebuilds it.

### bash 4.4

`gateway-entrypoint.sh` expands possibly-empty arrays (`"${tls_args[@]}"`) under
`set -u`. Before bash 4.4 that is an unbound-variable error, so under macOS's
`/bin/bash` (3.2.57, the last GPLv2 release, which is what Apple will ever ship)
the production script aborts at that line — measured, not assumed. `gateway.sh`
re-execs itself under `/opt/homebrew/bin/bash` (or `/usr/local/bin/bash`,
`/opt/local/bin/bash`) and refuses with `brew install bash` if it finds none.

### Identity

The entrypoint passes `--require-keys`, which is the fleet's fail-closed policy:
a missing private key means secret hydration failed, and minting a replacement
would silently fork the fleet identity. On first start `gateway.sh` therefore
does one mint pass — the same command with `--require-keys=false` — so all seven
files are written by the code that will later load them (`internal/nodepki`,
`internal/oidc`, `internal/sshgw`) rather than by an `openssl` line here that
could drift from what they parse. It then derives `gateway_host_key.pub` and
`gateway_upstream_key.pub` with `ssh-keygen -y`, exactly as
`deploy/kubernetes/deploy.sh` derives the `sparkbox-node-trust` Secret, so a
local node has a trust bundle to mount.

The server is always started with `--require-keys` — point it at an empty key
dir and it refuses with `host key: ... no such file`, minting nothing. The
wrapper is what is permissive: it re-runs the mint pass whenever the local set
is incomplete, because a half-deleted *local* identity is a scratch directory to
rebuild, not a fleet to lock out. Delete a key on CKS and you get the refusal.

## GitHub repo attachments

Out of the box `repo add` answers *"no GitHub App is configured on this host"*,
and it is right to: minting a repo credential needs an App's RSA private key,
and unlike every other key this dev box uses, that one cannot be generated here.
GitHub holds the public half.

The account-linking flow is unaffected and needs nothing — `--github-client-id`
defaults to a shipped public client id and the device flow has no callback URL,
so `github link` works on `dev.localhost` as-is. But a linking token cannot
clone: `internal/users/githubdevice.go` requests **no scope at all**, on purpose,
and a GitHub App's user token reaches only repositories that App is installed on.
Linking proves who you are; it is not a credential.

So this needs its own App — a **dev** App, not the cluster's. The two differ
only in which repositories they are installed on, and a dev box that can mint
against production installations is the thing worth avoiding. It needs no public
URL: minting is outbound-only, gateway to api.github.com. No callback, no
webhook, no tunnel.

One is already registered and wired up, so on this machine there is nothing to
do — `gateway.sh` defaults to client id `Iv23liZ9eVp3hIxJnALL` and reads its key
from `op://Hivemind-Dev/github-app-key/password` in the `coreweave.1password.com`
account. Both are checked in because neither is a secret: a client id is a public
identifier that travels in the request minting a device code (`cmd/sparkbox`
ships one for the same reason), and a 1Password reference is an address, useless
without a session on that account. The key itself is never in the tree.

To point at your own instead, or at none:

```sh
export SPARKBOX_GITHUB_APP_CLIENT_ID=Iv23li...              # your App
export SPARKBOX_DEV_OP_ITEM='op://YourVault/your-item/password'
export SPARKBOX_DEV_OP_ACCOUNT=your.1password.com           # or "" to let op choose
export SPARKBOX_DEV_OP_ITEM=                                # or: skip 1Password entirely
export SPARKBOX_GITHUB_APP_KEY_FILE=/path/to/app.pem        # a PEM already on disk
```

Registering one takes about five minutes: create the App with repository
permissions **Contents**, **Pull requests** and **Issues** (the set
`internal/metadata/repos.go` asks for, narrowed to what the installation actually
granted), generate a private key, put the PEM in a vault, and install the App on
the repositories you want to attach.

`start` reads the reference into `.dev/gateway/keys/github_app_key.pem` at 0600
and `stop` removes it, so the vault holds the only durable copy — which is the
point, since `.dev/` is disposable and this key cannot be regenerated from here.

**A 1Password problem is never fatal.** A missing `op`, a broken session, an
empty field: each warns, says attachments will be unavailable, and lets the
gateway start. The reference is a checked-in default, so a checkout with no
access to that vault has to run the control plane fine, and the desktop-app
integration is flaky enough that making it load-bearing would be a bad trade.
Contradictory *local* configuration still stops the run — an unreadable
`SPARKBOX_GITHUB_APP_KEY_FILE`, or both sources set at once — because that is a
typo, not a dependency having a bad day.

Then, on the dev box: `github link` (an `assertion` link is refused by
`attachIdentity`), `github install` for the URL that installs *this* App,
`repo add owner/name`, and `repo check` — which exists because every other
surface reports success whether or not the App was ever installed.

**`sparkbox fetch-secrets --provider op` is the wrong tool here**, despite being
the production path for this exact file. It walks the whole secret manifest and
writes every hit into `--key-dir`, so aiming it at a vault holding a real fleet
identity would overwrite this box's minted gateway, upstream and OIDC keys with
that fleet's. A vault holding only the App key does not work either: three
manifest entries are `required: true` and a missing one is fatal. Hence the
single-reference read.

If `op` fails with `RequestDelegatedSession: cannot setup session`, that is the
desktop-app integration, not your reference — Settings → Developer → Integrate
with 1Password CLI, then restart the app fully, or use `op signin` to skip the
desktop app. `op whoami` answers from local config even while the session is
broken; `op vault list` is the test.

## NO SILENT CAPS

Three deliberate deviations from the Pod, all announced in `gateway.sh`'s header:

- **listeners are on 127.0.0.1**, not `0.0.0.0` — `users.conf` here holds your
  real public key.
- **TLS is off** (`SPARKBOX_PROXY_TLS=false`). There is no public DNS name for
  this host and no ACME challenge that could be answered for one, so the OIDC
  issuer's own startup warning ("advertises https but --proxy-tls is off") is
  expected here and is a real failure anywhere else.
- **the fleet identity is local**. This gateway is its own fleet, not CKS's.

And a green run here is evidence about the gateway *program*. It is not evidence
about:

- **`linux/amd64`.** This is a darwin/arm64 build. Anything platform-specific —
  `_linux.go` files, the firecracker driver, cgroups, netfilter — is compiled
  out or stubbed, and the release the cluster runs is a different binary.
- **the scheduler and resource limits.** No requests/limits, no QoS class, no
  OOM kill, no `cpu: "48"` that has to fit on a node. Nothing here would notice
  a Pod that cannot be scheduled.
- **the device plugin.** `/dev/kvm` and `/dev/net/tun` are not requested,
  granted, or used: `--gateway-only` runs the mock driver, in the cluster too.
- **NetworkPolicy / Cilium.** All traffic is loopback. A policy that blocks the
  gateway from a node, or the edge from the internet, is invisible here.
- **the LoadBalancer and public edge.** No external IP, no ingress path, no
  Cloudflare DNS, no certificate. Ports are dialed directly.
- **the VAST durable tier.** `.dev/gateway/durable` is a local directory on APFS.
  RWX semantics, the migration Job, sqlite over a network filesystem, and
  anything about capacity or permissions on the real volume are untested.
- **sandboxes.** With the mock driver nothing boots. Lifecycle *bookkeeping* is
  exercised; guests, networking, snapshots, and agents are not.
- **the fleet's GitHub App.** With a dev App attached (above) the minting path
  is real, but the installations, repositories and permission grants are yours
  and not the cluster's. `fetch-secrets`, which is how the App key actually
  reaches a Pod, is deliberately not the path used here.

The five-container tier (`sparkbox devpod`) covers some of the middle of that
list and records what it still cannot reproduce in `Plan.Divergences`. Nothing
local covers the last three.

## The pod loop: registry, machine, image

A Mac cannot host the node Pod. It needs `/dev/kvm`, `/dev/net/tun`, cgroup v2
and a reflink-capable filesystem, and macOS has none of them. All four live one
level down, inside the Linux VM Apple's `container machine` boots. So the pod
tier is three moving parts, and until this session all three were hand-typed
state that nobody owned:

```sh
hack/dev/registry.sh up        # the local registry — idempotent, everything calls it
hack/dev/machine.sh ensure     # make the Apple machine ready; repairs what it can
hack/dev/image.sh all          # registry up -> build -> push -> machine pulls
```

Then `sparkbox devpod up` has an image to run.

### Why the image goes Mac → registry → machine, and never the other way

```
Mac / OrbStack  --push-->  127.0.0.1:5001  <--pull--  Apple machine
                           (registry.sh, on the Mac; the machine
                            reaches the same container at 192.168.64.1:5001)
```

The obvious arrangement is the broken one. Running the registry *inside* the
machine and pushing to `192.168.64.2:5000` from the Mac **times out** — measured.
`docker` on a Mac is a thin client for a daemon inside OrbStack's own Linux VM,
and that VM has no route to Apple's `192.168.64.0/24` vmnet. macOS
`curl http://192.168.64.2:5000/v2/` succeeds, because that is the *Mac's* network
stack, which is exactly what makes the failure look like a registry or TLS
problem instead of a routing one. Pushing to `127.0.0.1` keeps the traffic inside
OrbStack's port forward; the machine pulling outward reaches the vmnet gateway
address fine.

Two consequences: `127.0.0.1:5001` needs no `insecure-registries` entry (Docker
trusts `127.0.0.0/8` over plain HTTP by default) but `192.168.64.1:5001` does
inside the machine — `machine.sh ensure` writes it; and one image carries two
references, `127.0.0.1:5001/sparkbox-cks:dev` for the push and
`192.168.64.1:5001/sparkbox-cks:dev` for the pull. Same blobs, two addresses,
because the two sides do not share one.

Measured on an M4 Max: build **57s** cold, push **3s**, pull **1.9s**.

### `registry.sh`

`registry:2` in a container on the Mac, named `sparkbox-reg-mac`, storage in the
named volume `sparkbox-reg-data`.

```sh
hack/dev/registry.sh up              # create, or start, or say it is already up
hack/dev/registry.sh status          # state, restart policy, contents, volume size
hack/dev/registry.sh gc [--dry-run]  # actually reclaim space
hack/dev/registry.sh down [--purge]  # remove the container (--purge: the volume too)
```

It exists because the registry used to be a `docker run` somebody typed once,
with no restart policy — and it **died on its own** (exit 2, about seven minutes
in), taking the image loop with it and leaving a `docker push` that timed out for
no visible reason. `up` is idempotent and is what `image.sh` calls before every
push, so the state is asserted rather than assumed. `status` on an existing
container also checks its *shape* — restart policy, published port, backing
volume — because a container that is running can still be the wrong container.

`gc` is a separate subcommand because `REGISTRY_STORAGE_DELETE_ENABLED=true` only
**permits** deletion; it reclaims nothing by itself. Push `:dev` a second time and
the first manifest stays, untagged, still referencing every layer, so the volume
only grows. `--delete-untagged` is what unlinks those, and without it a
repeatedly-rebuilt tag reclaims almost nothing. Upstream advises running the
collector with the registry read-only, since a blob being uploaded concurrently
can be collected out from under the push; this registry has exactly one writer,
so the rule here is simply *do not gc during a push*. Do not copy that shortcut
anywhere shared.

`down` never removes the volume unless you pass `--purge`. Removing the container
is a one-second mistake to undo; removing the volume costs a rebuild and re-push
of every tag.

### `machine.sh`

```sh
hack/dev/machine.sh ensure   # check + repair, idempotent; non-zero on anything it cannot fix
hack/dev/machine.sh status   # the same checks, read-only, plus disk
hack/dev/machine.sh shell    # root shell in the machine
```

It **never creates, starts, stops or deletes a machine.** The machine is a ~27GB
one-way ratchet — Apple's disk image never shrinks — that also carries the outer
KVM kernel and a provisioned sparkbox install, so losing one costs a full
`sparkbox setup` and a kernel download. This script adopts a machine that already
exists; if there is none it prints the `sparkbox setup --machine-name …` command
and exits non-zero.

`ensure` checks, in the machine: `/dev/kvm` and `/dev/net/tun`, cgroup v2,
`/srv/sparkbox/data` mounted, `cp --reflink=always` **actually attempted** rather
than assumed, docker installed and running, its data-root under that volume, and
the Mac's registry reachable from inside. Reflink is tested because the two
filesystems in this machine disagree: `/` is ext4 and `--reflink=always` fails
there, `/srv/sparkbox/data` is XFS and it works. Get that wrong and rootfs clones
silently degrade to full copies — a slow boot, not an error.

`/etc/docker/daemon.json` is **merged**, never rewritten: a `python3` pass adds
`data-root` and appends `192.168.64.1:5001` to `insecure-registries`, keeps every
other key, refuses outright if the file is not valid JSON, and writes through a
temp file. Docker is restarted **only when the file actually changed** — a
restart kills every running container, the dev pod included, so an unconditional
one would make `ensure` unsafe to run mid-session, which is the one thing an
idempotent command must not be.

### Getting commands into the machine

There is no shared filesystem (`--home-mount none`) and no `container machine cp`.
The only transport is a script on stdin:

```sh
container machine run -i --root --name sparkbox -- bash -s < script.sh
```

**`-i` is mandatory.** Without it stdin is discarded silently, bash reads EOF, and
the command exits **0 having run nothing** — a green run that did nothing, which
is worse than a red one. Payloads at or above ~192 KiB deadlock, so the guest
scripts stay small. `machine.sh` does not trust the transport's exit status
either: the guest prints a trailing `EXIT n` line and that is what decides, so a
transport that swallows a status cannot turn a red run green.

### `image.sh`

```sh
hack/dev/image.sh all      # registry up -> build -> push -> pull, with per-stage timings
hack/dev/image.sh build    # build on the Mac only
hack/dev/image.sh push     # push an already-built image
hack/dev/image.sh pull     # machine.sh ensure, then have the machine pull
```

The build context is `tools/` — `deploy/kubernetes/Containerfile` copies both
`sparkbox/` and `sluice/` — and it is the **live working tree**. Uncommitted edits
are included by design; that is the entire point, and also why a build started
while someone else is mid-edit bakes in a half-written file.
