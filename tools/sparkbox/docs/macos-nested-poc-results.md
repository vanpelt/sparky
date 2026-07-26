# macOS nested gateway PoC results

Run date: 2026-07-24

Result: M0 through M3 passed on Apple Silicon. A Sparkbox gateway running in
an Apple Container machine booted a real ARM64 Firecracker sandbox, and the
SSH, egress, metadata, token, and HTTP ingress paths worked end to end.

## Test environment

| Component | Version or configuration |
| --- | --- |
| macOS | 26.5.2 (25F84), arm64 |
| Apple Container CLI | 1.1.0 |
| Outer gateway | 8 vCPU, 24 GiB RAM, no macOS home mount |
| Outer kernel | Linux 6.14.9, custom KVM/TUN/XFS configuration |
| Sparkbox release | v0.3.0 |
| Firecracker | v1.16.1, arm64 |
| Data volume | 40 GiB XFS loop volume, no swap |
| L2 guest | Linux 6.1.155, aarch64 |

The kernel input, Apple base configuration, Go image, and Ubuntu image were
pinned by version and digest. The resulting outer kernel SHA-256 was
`7bb865dfc2dfb6578d41a9fb2d044299c626377ff69c540b15108afb75dd080c`.

That is a record of what this run produced, and it has been quoted since as
though it were an identity anyone could re-derive. It is not: the builder's
toolchain came from an unpinned `apt-get install build-essential`, and gcc's and
binutils' version strings are compiled into the kernel banner. See B2 in
[`forward-plan.md`](forward-plan.md) for what CI does gate on instead.

## Milestone results

### M0 — ARM64 provisioning preflight

- ARM64 accepts `/dev/kvm` as the hardware-virtualization signal instead of
  looking for the x86-only `vmx` or `svm` CPU flags.
- AMD64 behavior remains covered by unit tests.
- A static Linux/ARM64 Sparkbox binary builds successfully.

### M1 — reproducible L1 machine

- `sparkbox-poc` booted the pinned `6.14.9-sparkbox-poc` kernel.
- KVM initialization succeeded and `/dev/kvm` was usable.
- TUN/TAP creation, loop devices, XFS mount/unmount, and outbound HTTPS passed.
- The machine was created with `home-mount=none`.

### M2 — provision the real gateway

- `sparkbox setup` installed the v0.3.0 ARM64 artifacts with 40 GiB of XFS data
  storage and zero swap.
- `sparkbox doctor` reported 14 passes, one warning, and zero failures.
- `sparkbox-net.service` and `sparkbox.service` were enabled and active.
- An idempotent second provisioning run made no destructive changes.
- SQLite state, `users.conf`, `sparkbox.env`, and the fleet keys retained the
  same hashes across a gateway restart.
- The gateway address changed during testing and was rediscovered dynamically.

The one doctor warning is expected for the PoC sizing: the released universal
rootfs is 25 GiB, leaving about 14 GiB free on the 40 GiB data volume.

### M3 — one real sandbox end to end

- `new+macpoc` created an ARM64 Firecracker sandbox through the SSH gateway.
- `uname -m` returned `aarch64`.
- A rootfs sentinel survived SSH reconnect and a full outer-gateway restart.
- Guest DNS and outbound HTTPS succeeded.
- The metadata identity endpoint and injected JWT token worked.
- A request from a deliberately spoofed cross-slot address returned HTTP 403.
- The default `:8081` proxy reached guest port 8000 with the expected Host
  header.
- The arbitrary-port REDIRECT/original-destination path reached guest port
  8123.

The full outer restart changed both the outer and nested guest boot IDs, then
cold-booted the persisted sandbox with its rootfs sentinel intact. XFS and both
Sparkbox services recovered automatically.

## Implementation finding

The released rootfs contains a baked fleet SSH key, while standalone
`sparkbox setup` creates a new local upstream key. That mismatch prevented the
gateway from authenticating to a freshly cloned guest even though the guest
SSHD was healthy.

The Firecracker driver now installs the active gateway upstream public key into
each cloned rootfs before boot. The login identity is resolved from the
rootfs's `/etc/passwd`, and the resulting `authorized_keys` ownership and mode
are set inside the mounted image. This keeps fleet and standalone gateways
aligned with the key they actually loaded.

## Storage-accounting finding

The first macOS run showed `macpoc` as using 25 GiB of its 25 GiB disk even
though the guest's `df` reported only about 2.4 GiB used. Two representation
details combined to produce that result:

- setup's streaming Go decompressor wrote every decoded zero byte, materializing
  the sparse 25 GiB rootfs template on XFS; and
- Firecracker's disk reporter used host-side `du`, which counts materialized
  and shared reflink blocks rather than guest filesystem usage.

Compressed artifacts now preserve all-zero ranges as sparse holes while they
are installed. The Firecracker reporter reads the ext4 superblock's total,
free, and metadata-overhead counters, matching the guest `df` semantics without
running filesystem-mutating tools against a live image. The existing PoC
template remains physically materialized until it is recreated or compacted,
but its user-console meter no longer reports that host representation as guest
usage.

## Verification

- `go test ./...`
- `go vet ./...`
- Linux/ARM64 Firecracker and Sparkbox command tests in the pinned Go build
  container
- static `GOOS=linux GOARCH=arm64 CGO_ENABLED=0` build
- Bash syntax checks for every macOS helper
- two complete M3 smoke runs, including the delegated `poc.sh smoke` path
- gateway stop/start followed by L2 reconnect and sentinel verification

Raw timestamped evidence is generated beneath `macos/out/results/` and is
intentionally ignored by Git.

## Fleet integration

After the multi-node gateway/node work landed on `main`, the macOS branch was
rebased onto that architecture and revalidated on 2026-07-24.

- Firecracker now installs `vmm.Config.GatewayPublicKey` per guest creation
  instead of capturing a standalone gateway key when the driver is built. This
  is required for a fleet node, which learns and caches the gateway key only
  after its enrollment handshake.
- `sparkbox setup --gateway <host:port> [--node-name <name>]` provisions a
  Linux host directly as a fleet node, while `sparkbox doctor --gateway
  <host:port>` diagnoses the node layout without requiring gateway-only fleet
  keys or `users.conf`.
- A fresh macOS PoC machine can select that role with
  `SPARKBOX_FLEET_GATEWAY=<host:port>` and an optional
  `SPARKBOX_NODE_NAME=<name>` when running `./macos/poc.sh provision`.
- A temporary real Firecracker-capable node process inside the Apple Container
  machine enrolled at the live gateway, waited for fingerprint approval,
  received the gateway upstream key, and reported online as `arm64`. Its test
  enrollment and transient state were removed afterward.
- The complete Go suite, the pinned Linux/ARM64 tests, the gateway OCI build,
  and the full nested M3 smoke suite passed against the merged fleet-aware
  source.

The merged multi-node change is its M0/M1 slice: enrollment, trust, heartbeat,
and capacity reporting work. Scheduling and serving remote sandboxes still
depend on the later multi-node lifecycle/data-plane milestones; that is an
upstream fleet limitation rather than a macOS virtualization blocker.

M4 pause/resume, snapshot/archive, dirty-shutdown recovery, ten-guest latency,
and memory measurements remain separate reliability work as planned.
