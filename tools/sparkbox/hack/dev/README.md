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

And one script that is not a tier but an escape hatch: `guest.sh`, for when a
guest boots far enough to exist and not far enough to be reachable. See
[When the guest will not answer](#when-the-guest-will-not-answer).

The rendering half of the pod tier is a separate piece of work
(`internal/devpod`, `sparkbox devpod`). What this README covers is the gateway
tier plus everything the pod tier needs to exist before it can run: the local
registry, the Apple container machine, and the image that moves between them —
`registry.sh`, `machine.sh`, `image.sh`.

## Start here

```sh
sparkbox setup --machine-name sparkbox   # once, if you have no container machine
hack/dev/up.sh                           # everything else, idempotent
ssh -p 2222 new+mybox@127.0.0.1          # a real aarch64 Firecracker guest
```

`up.sh` is the front door. It runs `machine.sh ensure`, `image.sh all`,
`gateway.sh start` and `sparkbox devpod up` in the order they need, and makes
the three joins between them that nothing else does — the wider SSH bind, the
host key copied into the machine, and the node approval. Each of those fails far
from its cause if you skip it, which is why they are worth a script rather than
a paragraph: a loopback listener looks like a gateway with no nodes, a missing
host key looks like a node that never comes online, and overlapping guest
subnets are refused at `node approve` with no hint that the *gateway* is the
half to move.

It is safe to re-run. A converged environment is left alone rather than
rebuilt — in particular it will not stop a node that is carrying sandboxes, so
re-running it does not cost somebody their build. `up.sh down` stops the gateway
and the node Pod but keeps the machine, the image and the node's data volume;
`up.sh status` reports all three tiers read-only.

Two things it cannot finish, and says so: the 1Password session that unlocks the
GitHub App key, and `github link`, which is an interactive device flow. Both are
optional — sandboxes boot without either; only repo attachments need them.

`sparkbox setup` is deliberately outside all of this. The container machine is a
~27GB one-way ratchet that carries the custom KVM-capable outer kernel, so
`machine.sh` adopts one and never creates, starts or deletes one.

## Start with this one: transparent huge pages

Guest RAM is anonymous memory Firecracker `mmap`s. Every page the guest touches
for the *first* time faults through two stage-2 layers — ours, and Apple's
underneath it. Measured in this machine, that costs **~300 us per fault**:
about 3,300 faults/sec, or **~13 MB/s** of first-touch memory. The very same
allocation made directly in the machine, no VM involved, runs at 4,600 MB/s.

A **350x** penalty, and nothing about it looks like a memory problem.

What it looks like instead is that *boot* is slow, because boot is what touches
most of guest RAM (the kernel initialises a `struct page` for all of it). So the
cost scales with `mem_size_mib` while everything else looks fine — userspace
compute in a booted guest runs at full speed. Same kernel, same rootfs, time to
`/init`:

| guest RAM | time to init |
|---|---|
| 512 MiB  | 1.43 s |
| 1024 MiB | 1.42 s |
| 2048 MiB | **25.74 s** |
| 4026 MiB | **34.08 s** |

A cliff, not a slope. THP collapses 512 of those faults into one:

| `transparent_hugepage/enabled` | 4026 MiB guest, time to init |
|---|---|
| `madvise` (Apple's default) | 29.82 s |
| `always` | **0.23 s** |

`machine.sh ensure` now sets `always` and reports it, so this is handled. The
trap is that `madvise` *looks* enabled — but Firecracker does not `madvise()`
its guest memory, so `madvise` means no THP here at all.

We do **not** use Firecracker's own `huge_pages: "2M"`, which gets the same
result (0.21 s): that is `MAP_HUGETLB`, it needs pages reserved up front, and it
is incompatible with the balloon device. THP needs no code change, keeps the
balloon, and keeps snapshots.

Setting it is a live sysctl. It is deliberately NOT wired to `machine.sh`'s
`changed` flag, because that flag restarts docker — which kills the node Pod and
every sandbox on it.

With `always`, a `new+<name>` sandbox reaches `running` in **4 s**, the whole
boot costs 6.5 CPU-seconds, `containerd` starts in **1.05 s**, and
`systemctl --failed` is empty.

### What is NOT the cause

Kept so nobody re-runs them. Every one of these was measured and cleared: host
storage (L1 cold-reads 128 MB in 382 ms), the backing file (209 extents, 25-50
ms for 128 MB), reflink copy-on-write (2.8x, and the penalty is on reads), the
guest clock (ratio 0.934 against host wall time), the serial console (2000
bytes in 10 ms), memory compaction (zero stalls across a 4 GB touch), and CPU
or virtualization traps — guest compute is *faster* than the VM hosting it
(562 ms vs 1154 ms on the same loop).

Small-file guest I/O also looked catastrophic on the way here: reading 8 MiB
took 5845 ms in 2048 4 KiB requests against 177 ms in 128 64 KiB ones, which
reads as a per-request cost. Most of that was this same memory bug — with THP
on, the identical 4 KiB run takes **20 ms**, and firecracker's `Async`
(io_uring) block engine measures indistinguishable from the default `Sync`
(530 ms vs 502 ms total boot). There was a `--block-io-engine` flag here for a
while; it was removed once that was measured.

## The other one: the idle reaper's balloon

The guest that pegged four vCPUs for an hour was **ballooned down to its 256 MB
reserve two minutes into a boot that had not finished**. Firecracker's own log
is where this is visible, and it is one line:

```
The API server received a Patch request on "/balloon" with body "{\"amount_mib\":3770}"
```

3770 of 4026 MB, taken from a guest that was starting docker. containerd then
failed to start eleven times, docker took fourteen minutes, and the in-guest
agent never came up at all. All four vCPUs sat at ~100% **system** time — page
reclaim, not guest work — and `guest=0` in `/proc/<vmm>/task/*/stat` the whole
time.

It could not recover on its own, and that is the important part. The balloon is
deflated by evidence of activity, and the activity floors are
`--activity-cpu-pct` (**0 by default** — CPU is not a signal unless you opt in)
and `--activity-net-kb`. A guest starved before its agent started moves no
traffic, so it reads as perfectly idle forever. Squeezing it is what makes it
look idle, which is what keeps it squeezed.

Two changes now stand between you and that:

- **No path** squeezes a guest below what the balloon device says it is
  actually using — the idle reaper leaves 1.5x it, the memory-pressure
  controller reclaims down to it but never past it. A booting guest has a large
  working set and is left alone; a genuinely idle one still gives its RAM back.
  Only the idle path was guarded at first, which left the same trap open on the
  path that fires under real pressure.
- `--idle-balloon` now defaults to **20 minutes**, not 2.

Genuine memory overage is still reclaimed promptly, by the memory-pressure
controller, which reclaims exactly the excess from the coldest guests.

To see it live on a running guest:

```sh
container machine run -i --root --name sparkbox -- bash -s <<'EOF'
S=/srv/sparkbox/data/devpod/hot/jailer/firecracker/sparkbox-0/root/fc.sock
curl -s --unix-socket $S http://localhost/balloon/statistics
EOF
```

Driving `sparkbox devpod up` by hand instead of through `up.sh` means passing
all three yourself. `devpod plan` marks the omissions `[BLOCKING]`, including
the specific one — *"one sandbox is 12288 MB on a 12079 MB machine"* — and that
line is the only warning there is.

## Sandboxes are sized for the machine, not for CKS

`up.sh` reads the container machine's RAM and core count and passes
`-host-mem-mb`, `-default-mem-mb` and `-default-vcpus` derived from them — a
third of RAM and half the cores per sandbox, overridable with
`SPARKBOX_DEV_SANDBOX_MEM_DIVISOR` and `SPARKBOX_DEV_SANDBOX_CPU_DIVISOR`.

It does that because the defaults underneath are a cluster's. A sandbox nobody
sizes gets **4 vCPU / 12288 MB**, and `deployment.yaml` declares
`SPARKBOX_HOST_MEM_MB=480000` — both right for a CKS node, both catastrophic in
a container machine with ~12 GB and 8 cores, where a single guest is larger
than the entire machine.

Nothing refuses that. Admission control asks whether a sandbox fits the host's
RAM, and one VM at the ceiling technically does; the manifest's 480000 means it
does not even ask honestly. So a guest is promised memory that does not exist,
and what happens when it reaches for it is the host's decision rather than the
guest's — and four vCPUs out of eight leaves the node supervising the guest
competing with it for cores.

**Do not read a wedged guest as proof of this.** The sizing is worth fixing
because a VM larger than its host is indefensible on its own terms. It is not
what wedged the guest that prompted the fix, and a correctly sized 4026 MB
replacement wedged in exactly the same way.

## When the guest will not answer

Every supported route into a sandbox goes through the in-guest agent on `:8000`:
`ssh <name>@127.0.0.1`, the browser terminal, the REST API. A guest whose boot
never finished has no agent, so all three say *"could not reach the sandbox's
shell; it may still be starting"*, the node logs `connection refused` on `:8000`
every couple of seconds, and there is nowhere to go next.

```sh
hack/dev/guest.sh console <name>        # the serial console: the real boot log
hack/dev/guest.sh console <name> -f     # follow it
```

**`console` asks nothing of the guest.** firecracker writes ttyS0 to the
vmm-helper container's stdout, so it works with no network, no sshd and no
agent. Raw it is unreadable — the VMM's own API log shares that stdout, and
systemd's progress bar rewrites one line several times a second, so a stuck boot
buries its own evidence under thousands of redraws. `console` strips both and
ends with a **"still waiting"** summary naming each unit systemd is stuck on and
for how long. That summary is usually the whole answer.

There is deliberately no `shell` mode. One existed and was deleted: it stood up
a bind-then-`setns` forwarder to reach the guest's own sshd, and it never
worked — it dialled with the operator's ssh identities while a guest's
`authorized_keys` holds only the gateway's key, and its reused listener port
could answer for the wrong guest, which is indistinguishable from the bug you
would be debugging. It cost real time during the investigation above.

If you do need a shell past the agent, the node container is already in the
guest's network namespace and already has an ssh client, so it is one composed
command and nothing is left running afterwards:

```sh
container machine run -i --root --name sparkbox -- \
  docker exec -i sparkbox-dev-sparkbox-node ssh sparky@<guest-ip>
```

### …but the guest booted fine and still will not answer

Then suspect the **gateway key in the template**, which fails in a way that
looks nothing like a key problem: the node links, the sandbox creates and boots
and reports `running`, `systemctl --failed` is empty — and every route in still
says *"could not reach the sandbox's shell"*, because the gateway cannot SSH
into a guest that does not hold its public key.

The node runs with `--disable-host-rootfs-mounts` (uid 65532, no
`CAP_SYS_ADMIN`, and deliberately never `mount(2)` on a guest-authored ext4), so
per-create key injection is skipped by design. Whatever `prepare-vm-assets`
baked into the template is the only key in the guest, and the template cannot
repair itself later.

Check it — these three must all match:

```sh
# 1. what the gateway will authenticate with
cat .dev/gateway/keys/gateway_upstream_key.pub

# 2. what prepare-vm-assets will bake, and 3. what the template actually carries
container machine run -i --root --name sparkbox -- bash -s <<'SH'
cat /srv/sparkbox/data/devpod-trust/gateway_upstream_key.pub
mkdir -p /mnt/kc && mount -o ro,loop \
  /srv/sparkbox/data/devpod/images/universal.ext4 /mnt/kc
cat /mnt/kc/home/sparky/.ssh/authorized_keys
umount /mnt/kc
SH
```

`up.sh` re-copies both public keys into the trust dir on every run and
`prepare-vm-assets` re-bakes the template, so `hack/dev/up.sh node` is the fix.
Until that was true the upstream `.pub` was written once and never refreshed,
so re-minting the gateway identity — which is exactly what starting a gateway in
a fresh checkout does — silently stranded every sandbox created afterwards.

## Keeping the identity: `secrets.sh`

The line above — *"re-minting the gateway identity is exactly what starting a
gateway in a fresh checkout does"* — is the whole reason this exists. `.dev/` is
disposable and the keys inside it are not:

- the **node** copies the gateway's host key into its control dir on its first
  successful link and trusts *that* from then on, so a re-mint means it refuses
  every link afterwards;
- the **rootfs template** carries the gateway's upstream public key as the login
  user's `authorized_keys`, so a re-mint means the gateway can no longer ssh
  into any sandbox created before it;
- the **OIDC signing key** derives the KEK for every user secret in the
  database, so a re-mint silently orphans all of them.

```sh
hack/dev/secrets.sh push     # .dev/gateway/keys -> 1Password
hack/dev/secrets.sh pull     # 1Password -> .dev/gateway/keys, then derive .pub
hack/dev/secrets.sh status   # what exists where, and whether they agree
```

It is a thin wrapper around `deploy/sync-fleet-secrets.sh`, which already does
this for a real fleet and has the parts that are easy to get wrong: values
travel in a template rather than argv, and **every write is proved by reading it
back** — `op item create` can accept a write and store an empty field, and a
backup you cannot restore is worse than none.

Three things it deliberately does not do:

- **It refuses the fleet vault.** The fleet default is `Sparkbox` and this one
  is `Sparkbox-Dev`; they differ by four characters, and one is reached by
  forgetting to set a variable. A `push` there would overwrite the fleet's
  gateway, upstream and OIDC keys with a laptop's.
- **It does not touch `github-app-key`**, which is already escrowed in a
  *different* vault and account (`op://Hivemind-Dev/…`, see `gateway.sh`). Two
  sources for one secret, silently preferring one, is how you end up debugging
  an App that is not the App you edited. Nor does it mint a webhook secret,
  Cloudflare token or console password — this box uses none of them, and an
  escrowed value nobody consumes is indistinguishable from one that matters.
- **It does not store the `.pub` halves.** `ssh-keygen -y` regenerates them from
  the `.pem` byte for byte, so escrowing them would be a second copy of the same
  fact that can disagree with the first. `pull` derives them, and `gateway.sh`
  now derives them on every start — a *restored* identity is complete, so
  `mint_identity` never runs and nothing else would.

After a `pull`, run `hack/dev/up.sh node`: the node still has the old host key
pinned, and `up.sh` clears that pin when it disagrees.

`hack/dev/test-secrets.sh` covers all of the above against a stub `op` on PATH,
including the two refusals, and runs in CI. A real vault cannot: `op` needs an
authorized desktop app or a service account token.

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
- **sandboxes, from the gateway alone.** `--gateway-only` runs the mock driver,
  so this process never boots a guest itself no matter how it is configured.
  Link the node Pod to it (below) and real microVMs boot; without one, creating
  a sandbox fails in `internal/fleet` with *"this gateway has no VM nodes"*.
- **the fleet's GitHub App.** With a dev App attached (above) the minting path
  is real, but the installations, repositories and permission grants are yours
  and not the cluster's. `fetch-secrets`, which is how the App key actually
  reaches a Pod, is deliberately not the path used here.

The five-container tier (`sparkbox devpod`) covers some of the middle of that
list and records what it still cannot reproduce in `Plan.Divergences`. Nothing
local covers the last three.

## Booting real sandboxes: linking the node

`up.sh` does all of this. It is written out because when the link breaks, the
symptom is never where the cause is, and because a fleet node on real hardware
needs the same three things lined up by hand.

The gateway can only bookkeep sandboxes. Booting them needs a VM node, which is
what `sparkbox devpod` runs. Three things have to line up, and each fails with
its own sentence if it does not.

**1. The gateway has to be reachable.** Its SSH listener is the single flow a
node uses — control *and* guest data both ride the node's own outbound
connection to it, which is why the cluster's Cilium egress policy allows exactly
`gateway:2222` and nothing else toward it. That listener is on loopback by
default and the container machine cannot reach loopback on the Mac:

```sh
SPARKBOX_DEV_SSH_BIND=0.0.0.0 hack/dev/gateway.sh restart
```

Only the SSH listener moves. The edge and API stay on 127.0.0.1, because only a
browser on this Mac uses them. `gateway.sh status` says which mode it is in and
prints the next command either way.

**2. The two guest prefixes must not overlap.** `node approve` reserves the
address space a machine may hand its guests and refuses a collision. The node
Pod's is `172.30.0.0/20`, from `deployment.yaml`; the gateway's own would
otherwise default to `guestnet.DefaultPrefix` — `172.30.0.0/16` — which contains
it. `gateway.sh` therefore passes `--guest-subnet 10.201.0.0/20`
(`SPARKBOX_DEV_GUEST_SUBNET`). Moving the *gateway* is the correct half to move:
the node's value is production config this environment reproduces rather than
edits. Nothing boots into the gateway's prefix — it is reserved, not used.

**3. The node needs the gateway's host key**, since it verifies it on every
link. `gateway.sh` derives `gateway_host_key.pub` into `.dev/gateway/keys/`;
copy that one file — not the directory, which holds private keys — into a
trust dir inside the machine, then link:

```sh
sparkbox devpod up -image <ref> -data /srv/sparkbox/data/devpod \
  -trust-dir /srv/sparkbox/data/devpod-trust \
  -gateway 192.168.64.1:2222 -driver firecracker -node-name macdev
```

Then approve it from the Mac, by fingerprint, with the subnet it reports:

```sh
ssh -p 2222 ctl@127.0.0.1 node ls
ssh -p 2222 ctl@127.0.0.1 node approve SHA256:... --guest-subnet 172.30.0.0/20
```

A fingerprint and not a name, because a node picks its own name and the gateway
has nothing to check it against — `ctlops.ApproveNode` argues that out. `up.sh`
automates the ceremony rather than skipping it: it reads the fingerprint from
the node's own console inside the machine, checks the roster is offering that
same one, and approves only on a match. Approving whatever the roster happens to
hold would be the shortcut that turns the ceremony into theatre.

The node retries with growing backoff while pending, so it can take up to a
minute to flip to `online` after approval — it is not stuck. Then
`ssh new+<name>@127.0.0.1` boots a real aarch64 Firecracker guest.

Re-minting the gateway identity invalidates the copied `gateway_host_key.pub`,
and the node will refuse the link until the trust dir is refreshed. Nothing
detects that for you.

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
