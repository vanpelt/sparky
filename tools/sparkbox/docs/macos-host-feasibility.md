# macOS host feasibility

Status: research spike
Date: 2026-07-24

Environment update: the development Mac now has Apple Container CLI 1.1.0.
The local proof below was performed before that upgrade with CLI 0.3.0; the
follow-up [nested gateway PoC plan](macos-nested-poc-plan.md) uses the installed
1.1.0 `container machine` workflow.

## Conclusion

Running the existing Linux/Firecracker Sparkbox stack on an Apple Silicon Mac is
feasible on M3-or-newer hardware.

The lowest-risk first implementation is a long-lived Linux gateway VM created
with Apple Container's `container machine`. The gateway uses a custom
KVM-enabled kernel and runs the existing ARM64 Sparkbox binary, Firecracker,
TAP devices, iptables, and ext4 tooling. Firecracker then launches the actual
Sparkbox guests using nested virtualization.

This is more than a paper design. On an M4 Max running macOS 26.5.2, the
following stack booted successfully:

```text
macOS
  Apple Virtualization.framework VM (Linux 6.14.9, KVM enabled)
    Sparkbox Firecracker v1.16.1
      Sparkbox Linux 6.1.155 ARM64 guest on ext4
```

The L2 guest reached `/sbin/init`, printed a success marker, and powered off
cleanly. TAP creation with Sparkbox's `sbtapN` naming also succeeded in the
outer Linux VM.

The main unresolved questions are therefore operational rather than
fundamental: host port forwarding, memory reclamation at useful density,
snapshot reliability under nested KVM, graceful gateway shutdown, and
long-running stability.

## What Apple supports now

Apple's [Container CLI](https://github.com/apple/container) runs each Linux
container in a lightweight VM backed by
[Virtualization.framework](https://developer.apple.com/documentation/virtualization).
Its lower-level
[Containerization package](https://github.com/apple/containerization) supplies
the Linux kernel, init system, vsock services, networking, and OCI image
integration.

Nested virtualization is officially available on M3 and newer Apple Silicon.
The Virtualization framework exposes
[`isNestedVirtualizationSupported`](https://developer.apple.com/documentation/virtualization/vzgenericplatformconfiguration/isnestedvirtualizationsupported),
and Apple Container exposes it through `--virtualization`. Apple's
[`container machine` documentation](https://github.com/apple/container/blob/main/docs/container-machine.md)
now describes persistent Linux machines with nested virtualization.

There are three important qualifications:

1. The outer Linux VM needs a kernel built with `CONFIG_KVM=y`. Apple
   Container's default kernel did **not** expose `/dev/kvm` in the local test.
2. Sparkbox must use ARM64 Firecracker and ARM64 guests. Firecracker does not
   provide cross-architecture emulation.
3. The current Apple Container project requires Apple Silicon and macOS 26.
   M3+ is the practical Sparkbox baseline because older chips cannot expose
   nested KVM.

Apple's published
[ARM64 kernel configuration](https://github.com/apple/containerization/blob/0.5.0/kernel/config-arm64)
already includes the important pieces for Sparkbox: KVM, TUN/TAP, network
namespaces, bridges, netfilter, iptables NAT, masquerading, and redirect.

## Local proof

The test host was:

```text
Mac:       M4 Max, 64 GB, arm64
macOS:     26.5.2
CLI:       Apple Container 0.3.0
Outer VM:  Ubuntu 24.04, custom Linux 6.14.9
Inner VMM: Sparkbox release Firecracker 1.16.1 ARM64
L2 guest:  Sparkbox Linux 6.1.155 ARM64, ext4 root
```

The custom outer kernel reported:

```text
/dev/kvm
/dev/net/tun
kvm [1]: Hyp nVHE mode initialized successfully
```

The nested Firecracker invocation reported:

```text
Running Firecracker v1.16.1
Successfully started microvm
SPARKBOX_NESTED_FIRECRACKER_BOOT_OK
Firecracker exiting successfully. exit_code=0
```

This exercised the most important dependency chain:

- Virtualization.framework exposed nested KVM.
- Linux exposed `/dev/kvm` and `/dev/net/tun`.
- Sparkbox's exact released Firecracker executable ran at L1.
- Firecracker booted Sparkbox's exact released ARM64 kernel at L2.
- The inner guest mounted an ext4 root filesystem and ran init.
- The outer VM allowed creation and addressing of an `sbtapN` TAP device.

It did not yet exercise guest networking, SSH/HTTPS forwarding, Firecracker
memory snapshots, ballooning, archive/restore, or sustained multi-VM load.

## Option A: nested Sparkbox gateway

This should be the first proof of concept.

```text
macOS host
  host front door / port forwarding
    persistent Apple container machine
      current sparkbox serve
        Firecracker microVMs
```

### Shape

- Require M3+ Apple Silicon and macOS 26.
- Upgrade to Apple Container 1.1 or newer. `container machine` arrived in 1.0,
  and the 1.1 release notes add nested virtualization support.
- Build a gateway OCI image containing systemd, Sparkbox, Firecracker, ext4
  utilities, iptables, `iproute2`, zstd, and the existing host dependencies.
- Build or pin an outer kernel with KVM, TUN/TAP, bridge, and netfilter support.
- Create the persistent machine with `--virtualization --kernel ...`.
- Keep Sparkbox state on a persistent machine disk or mounted durable volume.
- Forward the public SSH/HTTPS entry points from macOS to the gateway.

### Why it is attractive

Most of Sparkbox remains unchanged. Its `vmm.Driver` surface is already narrow,
and the complete Firecracker driver continues to run in Linux. That retains:

- warm pause/resume through Firecracker memory snapshots;
- reflinked ext4 root disks;
- snapshot/archive tooling;
- disk growth;
- balloon controls;
- CPU, network, and disk metering;
- the existing TAP, NAT, metadata, DNS, and proxy design.

The existing manager can also cold-create a VM from its preserved root disk
when process state is lost, which is useful after a gateway restart.

### Risks to validate

**Host networking.** Apple Container VMs get dedicated addresses that are
reachable from the Mac, but the virtual network is not directly reachable from
external machines. Apple Container supports
[`--publish`](https://github.com/apple/container/blob/main/docs/how-to.md) and
newer releases support port ranges. The PoC should verify that host port
forwarding preserves the destination port into the gateway. Sparkbox's current
Linux edge relies on iptables `REDIRECT` plus `SO_ORIGINAL_DST` for arbitrary
HTTPS ports. If `container machine` cannot express the required range, a small
macOS forwarding helper using
[`vmnet` port-forwarding rules](https://developer.apple.com/documentation/vmnet/vmnet_network_configuration_add_port_forwarding_rule(_:_:_:_:_:_:))
is likely required.

**Memory reclamation.** Apple Container's
[technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md)
states that freed guest pages are not currently relinquished to macOS.
Firecracker ballooning or pausing an L2 guest may therefore return pages to the
gateway Linux VM without reducing the gateway VM's macOS resident high-water
mark. Periodic gateway restarts may be necessary, and density must be measured
rather than inferred from the Linux deployment.

**Nested-KVM confidence.** Firecracker requires `/dev/kvm`, but the
[firecracker-containerd documentation](https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/getting-started.md)
notes that nested virtualization is not well tested upstream. A successful
boot establishes feasibility, not production isolation or long-term
reliability.

**Failure domain.** Stopping, crashing, or updating the gateway stops every
inner microVM. Sparkbox can reconstruct guests from root disks, but an
ungraceful outer shutdown can still leave dirty ext4 filesystems or incomplete
memory snapshots.

**Security model.** A Firecracker escape would land inside a Linux VM rather
than directly on macOS, so the outer Virtualization.framework boundary remains
useful. The nested combination still needs its own threat model and soak/fuzz
testing before it is treated as equivalent to the current bare-metal KVM
deployment.

## Option B: a host-native Apple Container driver

A Darwin build of the gateway could treat one Apple Container VM as one
Sparkbox sandbox. The Go driver would invoke the `container` CLI or a dedicated
host helper:

| Sparkbox operation | Initial Apple Container mapping |
| --- | --- |
| Create | create/run a Linux VM from a pinned OCI image |
| Pause | stop the VM; resume would be a cold boot |
| Resume | start the existing VM |
| Destroy | delete the VM and its storage |
| Inspect | obtain the assigned IP and VM state |

This removes nested KVM and gives every sandbox a direct
Virtualization.framework VM. OCI images, quick starts, and host networking are
natural fits.

It is not the shortest route to feature parity. The initial driver would not
have Firecracker's warm memory snapshots, ext4 snapshot/archive flow, disk
resize behavior, balloon semantics, or existing metrics. The CLI also needs a
version-pinned machine-readable contract. This is a good fallback if nested
memory or networking proves unacceptable, but it should initially advertise
fewer optional `vmm.Driver` capabilities rather than imitating Firecracker
semantics.

## Option C: a native Virtualization.framework helper

For the strongest long-term macOS backend, build a small signed Swift daemon
around Containerization or raw Virtualization.framework and let the Go
gateway communicate with it over a Unix socket.

Raw Virtualization.framework offers the building blocks for Linux VMs,
networking, memory ballooning, and
[saving/restoring VM state](https://developer.apple.com/documentation/virtualization/vzvirtualmachine/restoremachinestatefrom%28url%3Acompletionhandler%3A%29).
It would also be the path to optional
[macOS guests](https://developer.apple.com/documentation/virtualization/running-macos-in-a-virtual-machine-on-apple-silicon).
Apple Container and Containerization are Linux-oriented and do not turn an OCI
image into a macOS VM.

This option gives the most control and the cleanest eventual macOS-guest story,
but it creates a second VMM implementation plus a Swift/Go RPC boundary,
signing and distribution work, OS-version compatibility work, and new storage
and lifecycle semantics. It is justified only after the smaller experiments
show that a native backend is needed.

## Recommended PoC

### Phase 1: nested gateway

1. Upgrade a test Mac from Apple Container 0.3.0 to 1.1 or newer.
2. Reproduce the nested boot with `container machine`, not a disposable
   `container run`.
3. Package a reproducible gateway OCI image and pinned outer KVM kernel.
4. Install the current ARM64 Sparkbox release inside the gateway.
5. Put `/var/lib/sparkbox` and related state on persistent storage.
6. Create a sandbox through the real API and verify SSH, metadata, DNS, and
   outbound networking.
7. Verify HTTPS on 443 and at least one arbitrary forwarded port.
8. Exercise warm pause/resume, snapshot/archive/restore, disk growth, and
   balloon controls.
9. Stop/start and force-kill the gateway, then verify guest recovery and disk
   integrity.
10. Run 1, 10, and 25 simultaneous guests while measuring cold boot, warm
    resume, CPU overhead, disk latency, and macOS resident memory high-water.
11. Exercise host sleep/wake, reboot, service auto-start, and clean shutdown.

### Decision gate

- If nested Firecracker is stable and the memory high-water is manageable,
  ship an experimental macOS-host packaging mode around the gateway machine.
- If gateway memory or port forwarding is the blocker, prototype the reduced
  host-native Apple Container driver.
- If full snapshot parity, deeper networking control, or macOS guests becomes
  a requirement, invest in the Swift Virtualization.framework helper.

## Expected repository impact

The nested gateway should require little or no change to `internal/vmm`.
Most work belongs in packaging and host lifecycle:

- a gateway OCI image;
- an outer KVM kernel config/build;
- a macOS installer or launcher;
- persistent-volume wiring;
- host port forwarding;
- service management and graceful shutdown;
- a macOS-specific doctor command.

A host-native backend would instead need:

- `cmd/sparkbox/driver_darwin.go`;
- an `applecontainer` implementation of the core `vmm.Driver`;
- Darwin replacements for Linux-only host checks and original-destination
  proxy behavior;
- explicit capability reporting for unsupported archive, snapshot, balloon,
  resize, and metrics operations;
- macOS release artifacts and CI coverage.

## References

- [Apple Container](https://github.com/apple/container)
- [Apple Container releases](https://github.com/apple/container/releases)
- [Container machines](https://github.com/apple/container/blob/main/docs/container-machine.md)
- [Apple Container how-to and networking](https://github.com/apple/container/blob/main/docs/how-to.md)
- [Apple Container technical overview](https://github.com/apple/container/blob/main/docs/technical-overview.md)
- [Apple Containerization](https://github.com/apple/containerization)
- [Apple Containerization ARM64 kernel config](https://github.com/apple/containerization/blob/0.5.0/kernel/config-arm64)
- [Virtualization.framework](https://developer.apple.com/documentation/virtualization)
- [Virtualization nested-support API](https://developer.apple.com/documentation/virtualization/vzgenericplatformconfiguration/isnestedvirtualizationsupported)
- [Firecracker getting started](https://github.com/firecracker-microvm/firecracker/blob/main/docs/getting-started.md)
