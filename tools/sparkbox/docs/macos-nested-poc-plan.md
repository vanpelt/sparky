# macOS nested gateway PoC plan

Status: ready to implement
Date: 2026-07-24
Related: [macOS host feasibility](macos-host-feasibility.md)

## Outcome

Prove that an unmodified Sparkbox Firecracker runtime can operate reliably
inside a persistent Apple Container machine, and leave behind a repeatable
developer workflow rather than a collection of manual commands.

The PoC is successful when a developer can start from a fresh Apple Container
installation and:

1. build the pinned outer KVM kernel and gateway OCI image;
2. create a persistent `sparkbox-poc` container machine;
3. provision Sparkbox using its existing `setup` command;
4. create and use an L2 Firecracker sandbox from macOS;
5. warm-pause and resume that sandbox;
6. stop/start the outer gateway and recover the sandbox filesystem; and
7. remove every PoC resource with an explicit cleanup command.

The expected effort is three to five engineering days, with useful stop/go
results after the first two milestones.

## Confirmed starting point

The installed executable reports:

```text
container CLI version 1.1.0
build: release
commit: 5973b9c
```

This matters because nested virtualization for `container machine` was added
in Apple Container 1.1.0. The installed plugin exposes:

```text
container machine create \
  --virtualization \
  --kernel /path/to/vmlinux-kvm \
  --cpus 8 \
  --memory 24G \
  --home-mount none \
  --name sparkbox-poc \
  <image>
```

Apple documents the same M3+, macOS 15+, and `CONFIG_KVM=y` requirements in
the [1.1.0 container-machine documentation](https://github.com/apple/container/blob/1.1.0/docs/container-machine.md).
The default kernel does not support nested KVM.

No container machines currently exist on the development Mac.

## Scope

The first PoC includes:

- one persistent Linux gateway VM;
- the current Linux/ARM64 Sparkbox gateway and Firecracker driver;
- a pinned, checksum-verified outer KVM kernel;
- a systemd-based Ubuntu 24.04 gateway image;
- persistent Sparkbox state inside the machine root filesystem;
- direct access from macOS over the machine's vmnet address;
- SSH, guest egress, HTTP routing, metadata, pause/resume, and restart tests;
- basic memory and latency measurements; and
- an idempotent host-side lifecycle script.

It deliberately excludes:

- macOS guests;
- a host-native Virtualization.framework driver;
- public/LAN ingress and production TLS;
- launchd installation and unattended host boot;
- fleet enrollment or cloud secret managers;
- production hardening or an isolation claim; and
- parity-scale density testing.

Those are follow-ons only if the nested gateway passes its decision gate.

## Architecture

```text
macOS
  Apple Container CLI 1.1.0
    container machine: sparkbox-poc
      custom Linux kernel with KVM, TUN, XFS, loop, and netfilter
      systemd
        sparkbox-net.service
        sparkbox.service
          Firecracker ARM64
            Sparkbox Linux sandbox
```

For the first PoC, the Mac connects directly to the gateway's vmnet IP:

```text
ssh -p 2222 new@<gateway-ip>
curl -H 'Host: <sandbox>.sparkbox.test' http://<gateway-ip>:8081/
```

`container machine create` has no publish or network option. A macOS port
forwarder is therefore not part of the critical path. If the nested runtime
works, host port forwarding becomes the next implementation slice.

## Design choices

### Reuse `sparkbox setup`

The gateway is a real Linux systemd environment, so the existing provisioner
should own:

- release manifest resolution and checksum verification;
- Firecracker, guest-kernel, and rootfs downloads;
- the reflink-enabled XFS data volume;
- `users.conf`;
- sysctls and iptables rules;
- systemd units; and
- service verification.

This keeps the experiment close to the deployed Linux product. The PoC adds
only the outer VM, its kernel/image, and host lifecycle.

One prerequisite code fix is required. `checkVirt` currently accepts only x86
`vmx`/`svm` flags and fails every ARM64 Linux host even when `/dev/kvm` is
usable. On ARM64, the existing `/dev/kvm` presence/writability check should be
the gate and the CPU-flag check should report an architecture-appropriate
result.

### Use a small data volume

The production provisioner defaults to a 300 GiB sparse XFS image and 16 GiB
of real swap. The PoC should begin with:

```text
--data-volume-gb 40
--swap-gb 0
```

Forty GiB is enough for the released sparse rootfs and a small number of
sandboxes without assuming how much storage Apple allocates to a container
machine. Before provisioning, the lifecycle script must record `df -h`,
`df -T`, and `lsblk` and fail clearly if 40 GiB cannot be allocated.

The first smoke test may use a smaller Ubuntu rootfs release if the universal
template does not fit. Storage density is not the first milestone's purpose.

### Do not mount the macOS home directory

Container machines default to mounting the user's entire Mac home read/write.
The gateway must use `--home-mount none`. The operator's public key is passed as
literal text to `sparkbox setup`; no private keys or repository tree are shared
with the gateway.

### Pin every moving input

The following become constants in the PoC scripts:

- Apple Container minimum version: 1.1.0;
- outer Linux version and source SHA-256;
- Apple base kernel-config tag and SHA-256;
- gateway base-image digest;
- Sparkbox release tag used for the sparkbox binary, Firecracker, guest kernel,
  and rootfs; and
- the local gateway image tag.

> **Superseded by B3.** The plan originally also pinned a Go toolchain version,
> because the image compiled sparkbox from source. It no longer does: one release
> tag now covers the binary and the artifacts together (see below).

The lifecycle script prints the resolved values into a result bundle so a
successful run can be reproduced later.

## Repository changes

The implementation should produce this shape:

```text
tools/sparkbox/
  docs/
    macos-nested-poc-plan.md
    macos-nested-poc-results.md       # created when the run is complete
  macos/
    .gitignore
    Containerfile.gateway
    sparkbox-bootstrap.sh             # fetch + verify the released binary (B3)
    test-bootstrap.sh                 # exercises it without a machine
    poc.sh
    smoke.sh
    kernel/
      build.sh
      sparkbox-arm64.fragment
  internal/hostsetup/
    checks.go
    checks_test.go
```

### `macos/kernel/build.sh`

- Downloads a pinned upstream Linux release and Apple ARM64 base config.
- Verifies both inputs before use.
- Applies a small Sparkbox fragment.
- Builds in a disposable ARM64 build container.
- Writes `macos/out/vmlinux-kvm` plus a manifest containing versions, hashes,
  and the final `.config` hash.
- Verifies at minimum:

```text
CONFIG_KVM=y
CONFIG_VIRTUALIZATION=y
CONFIG_TUN=y
CONFIG_BLK_DEV_LOOP=y
CONFIG_XFS_FS=y
CONFIG_EXT4_FS=y
CONFIG_NETFILTER=y
CONFIG_IP_NF_IPTABLES=y
CONFIG_IP_NF_NAT=y
CONFIG_NETFILTER_XT_TARGET_REDIRECT=y
```

Build output belongs under an ignored `macos/out/` directory and is never
committed.

### `macos/Containerfile.gateway`

Single-stage ARM64 build:

1. Create an Ubuntu 24.04 systemd machine image following Apple's documented
   container-machine pattern.
2. Install only the Linux host dependencies needed by `sparkbox setup` and
   runtime:

```text
ca-certificates curl zstd
xfsprogs e2fsprogs zerofree util-linux
iptables iproute2
systemd dbus
```

3. Install `macos/sparkbox-bootstrap.sh` at `/usr/local/sbin/sparkbox-bootstrap`.
4. Set the machine's default target to `multi-user.target`.
5. Do not include credentials, operator keys, rootfs images, or mutable state —
   **and no sparkbox binary**.

> **Superseded by B3.** The first implementation had a Go build stage that
> compiled the working tree and baked the result at `/usr/local/bin/sparkbox`.
> That required the git repo, a Go toolchain and `go mod download` on every
> onboarding, and it decoupled the machine's control plane from the release it
> provisioned — a machine built at `v0.3.0-5-g18bfe3b` provisioned
> `--release v0.4.0` and drove v0.4.0 artifacts with a v0.3.0-era sparkbox, with
> both numbers recorded in the same evidence bundle and nothing comparing them.
>
> Since A1, `sparkbox setup` installs the binary it is running from. So
> `provision` now runs `/usr/local/sbin/sparkbox-bootstrap` in the machine, which
> downloads the released `sparkbox-linux-arm64` for the pinned tag and verifies
> it against `SHA256_SPARKBOX` in that release's `manifest-arm64.env`, then runs
> `setup` from the staged copy; setup installs it at `/usr/local/bin/sparkbox`.
> `provision` fails if the installed binary reports any other version.
>
> Developing against a working-tree build is still supported, but only opt-in via
> `SPARKBOX_SOURCE_BUILD=1` or `SPARKBOX_LOCAL_BINARY`, and such a machine is
> marked with `/etc/sparkbox-poc-source-build` and reports the fact in `status`.
> `macos/test-bootstrap.sh` exercises the bootstrap's decisions against a fake
> release without needing a machine.

### `macos/poc.sh`

Provide deliberately small subcommands:

```text
./macos/poc.sh doctor
./macos/poc.sh build
./macos/poc.sh create
./macos/poc.sh provision
./macos/poc.sh status
./macos/poc.sh stop
./macos/poc.sh start
./macos/poc.sh smoke
./macos/poc.sh destroy [--machine] [--image] [--kernel] [--all] --yes
```

Behavior:

- `doctor` verifies Apple Silicon, macOS, CLI >= 1.1.0, required host tools,
  available disk, and that the fixed machine name is not owned by something
  unexpected.
- `build` creates the outer kernel and gateway OCI image.
- `create` is idempotent and creates `sparkbox-poc` with explicit CPU, memory,
  kernel, nested virtualization, and `home-mount=none`.
- `create` immediately verifies `/dev/kvm`, `/dev/net/tun`, the kernel version,
  KVM initialization in dmesg, loop support, an XFS loop mount, and outbound
  HTTPS. It does not proceed on a partial pass.
- `provision` passes the operator's public key as literal text and runs:

```text
sparkbox setup \
  --release <pinned-tag> \
  --operator-handle operator \
  --operator-key '<public-key>' \
  --proxy-domain sparkbox.test \
  --data-volume-gb 40 \
  --swap-gb 0
```

- `status` reports machine state, vmnet IP, filesystems, `/dev/kvm`,
  Sparkbox service state, and current L2 sandbox count.
- `start` boots the machine through `container machine run` and waits for
  `sparkbox.service` readiness.
- `destroy` is the only destructive operation and requires `--yes`. It never
  prunes unrelated Apple Container data, and it tears down **by cost tier**
  rather than all at once: `--machine` (the default — seconds to recreate),
  `--image` (a multi-minute container build), `--kernel` (an 8-CPU
  in-container Linux compile, reproducing bytes that are already pinned and
  checksummed), or `--all` for every tier plus `macos/out`'s inputs and
  `results/` evidence. A bare `destroy --yes` deletes only the machine.
- `provision` switches a machine between standalone gateway and fleet node
  **in place** when the machine carries no sandboxes — the change is one line in
  `sparkbox.env`, which `setup` will not rewrite on its own, so `poc.sh` backs
  the file up and lets `setup` render it fresh. It still refuses when sandboxes
  are present, or when it cannot prove there are none, because a gateway's users
  DB, routes and fleet secrets are meaningless on a node.

Defaults for this 64 GiB M4 Max test host are 8 vCPUs and 24 GiB of outer
memory. Both remain environment-variable overrides.

### ARM64 preflight fix

Update `internal/hostsetup/checks.go` so:

- AMD64 retains the `vmx`/`svm` test.
- ARM64 passes the hardware-virtualization check when `/dev/kvm` is present,
  with detail that CPU flags are not the ARM64 signal.
- Other architectures keep the existing failure/warning behavior.

Add table tests for ARM64 with and without `/dev/kvm`, and preserve the AMD64
coverage.

## Milestones

### M0 — ARM64 provisioning preflight

Work:

- implement the architecture-aware virtualization check;
- add unit tests; and
- build a Linux/ARM64 Sparkbox binary.

Acceptance:

- `go test ./internal/hostsetup ./cmd/sparkbox` passes;
- an ARM64 probe with `/dev/kvm` passes preflight; and
- AMD64 behavior does not change.

Estimate: half a day.

### M1 — reproducible L1 machine

Work:

- add the kernel builder and config fragment;
- add the systemd gateway image;
- implement `doctor`, `build`, and `create`; and
- create `sparkbox-poc`.

Acceptance:

- the machine uses the pinned custom kernel;
- `/dev/kvm` and `/dev/net/tun` exist;
- dmesg reports successful KVM initialization;
- a temporary `sbtap99` can be created and removed;
- a temporary XFS loop image can be mounted and unmounted;
- the gateway can reach the Sparkbox release endpoint; and
- the Mac home directory is not mounted.

Stop here if KVM, TUN, XFS/loop, storage capacity, or outbound networking does
not work reliably.

Estimate: one day.

### M2 — provision the real gateway

Work:

- implement `provision`, `status`, `start`, and `stop`;
- run `sparkbox setup` with 40 GiB data and no swap;
- resolve the gateway vmnet IP; and
- preserve logs and the provision manifest.

Acceptance:

- `sparkbox doctor` has no failures;
- `sparkbox-net.service` and `sparkbox.service` are active;
- the exact released Firecracker and guest kernel are installed;
- stopping and starting the container machine remounts the XFS volume;
- Sparkbox's sqlite state and fleet keys survive the restart; and
- the host script discovers the current IP rather than assuming it is stable.

Estimate: half to one day.

### M3 — one real sandbox end to end

Work:

- implement the basic smoke test;
- create a named sandbox through the SSH gateway;
- execute commands in it; and
- exercise networking and proxy paths.

Acceptance:

- `ssh -p 2222 new+macpoc@<gateway-ip>` creates an L2 sandbox;
- `uname -m` in the sandbox reports `aarch64`;
- a file written to the guest rootfs survives reconnect;
- guest DNS and outbound HTTPS work;
- guest metadata/token access works and cross-sandbox spoofing remains blocked;
- a guest HTTP server is reachable through the gateway at `:8081` with the
  expected Host header; and
- one arbitrary destination port reaches the correct guest port through the
  outer Linux REDIRECT/original-destination path.

Estimate: one day.

### M4 — lifecycle parity and recovery

Work:

- test Firecracker pause/resume;
- test snapshot/archive if object storage can be configured locally;
- stop/start the outer gateway; and
- measure resource behavior.

Acceptance:

- warm pause/resume preserves the guest boot ID and an in-memory sentinel;
- the Firecracker memory and state snapshots are created and consumed;
- a gateway stop/start cold-boots the sandbox with its rootfs data intact;
- dirty shutdown recovery is characterized with `e2fsck`;
- archive/restore or snapshot/fork succeeds, or its exact blocker is recorded;
- cold boot and warm resume latency are recorded for 1 and 10 guests; and
- macOS and outer-Linux resident memory are recorded before load, after load,
  after inner pause/balloon, and after outer restart.

Estimate: one to two days.

## Smoke-test evidence

`macos/smoke.sh` should write timestamped output beneath
`macos/out/results/<timestamp>/`:

```text
host.txt                 # hardware, macOS, container version
inputs.txt               # image/kernel/Sparkbox pins and hashes
machine-inspect.json
gateway-kernel.txt
gateway-storage.txt
gateway-network.txt
sparkbox-doctor.txt
sparkbox-journal.txt
firecracker-journal.txt
l2-boot.txt
lifecycle.txt
memory.csv
latency.csv
summary.txt
```

Generated evidence is ignored by Git. A short, curated result is copied into
`docs/macos-nested-poc-results.md`.

## Decision gate

Proceed to an experimental macOS packaging mode only if all of these are true:

- nested Firecracker boots repeatedly without KVM or VMM errors;
- SSH, guest egress, metadata, and HTTP routing work;
- warm pause/resume is reliable;
- gateway restart preserves durable state and has a clean recovery story;
- ten small guests fit without pathological CPU or disk overhead; and
- outer-memory high-water behavior is understood and operationally tolerable.

If the runtime passes but ingress is missing, the next slice is a small macOS
host forwarder plus launchd lifecycle.

If runtime memory cannot be reclaimed or nested snapshots are unreliable,
stop investing in the gateway and prototype the reduced host-native Apple
Container driver described in the feasibility document.

If the gateway cannot safely persist the XFS loop volume, evaluate a dedicated
Virtualization.framework data disk before proceeding; do not silently store
production state in the Mac home mount.

## Implementation order

The first implementation PR should stop at M1:

1. fix ARM64 host preflight and tests;
2. add the pinned outer-kernel builder;
3. add the gateway Containerfile;
4. add `doctor`, `build`, `create`, and explicit `destroy`; and
5. capture evidence that the persistent machine exposes KVM/TUN and supports
   XFS loop storage.

The second PR should implement M2 and M3. M4 should remain a separate
measurement/reliability change so functional bring-up is easy to review.
