# Sparkbox sandbox hardening

This note records the CKS POC's security boundary, what the jailer change buys
us, and the path from the split public gateway/private VM-node deployment to a
smaller least-privilege node runtime.

## Boundary and threat model

The Firecracker microVM is the primary guest/host boundary. Firecracker's
built-in seccomp filters, deliberately small device model, and KVM are valuable.
On a conventional host the production posture also expects the matching
`jailer`: it constructs a private mount namespace and chroot, gives the VMM only
KVM/TUN device nodes, drops to an unprivileged uid/gid, and then execs
Firecracker. See the upstream
[jailer design](https://github.com/firecracker-microvm/firecracker/blob/main/docs/jailer.md)
and [production host guidance](https://github.com/firecracker-microvm/firecracker/blob/main/docs/prod-host-setup.md).

The jailer is defence in depth after a Firecracker process compromise. It does
not fix a KVM or host-kernel vulnerability, a malicious host-side control
service, unsafe parsing of a guest disk by the host, or unrestricted guest
networking.

The stock jailer necessarily calls `unshare(CLONE_NEWNS)`, `mount`, and
`pivot_root`, which requires `CAP_SYS_ADMIN`. CKS instead uses Sparkbox's
`--chroot-jailer`: the container runtime has already created the Pod mount
namespace, and Sparkbox constructs a per-VM chroot in that namespace with only
the Firecracker executable, KVM/TUN nodes, and explicitly linked VM resources.
The child is execed as `100000 + network slot`, with that as its only
supplementary group and an empty environment. It receives no Linux capabilities.

The CKS integration now:

- downloads and checksum-verifies the pinned Firecracker v1.16.1 binary;
- creates one chroot per VM and exposes only hard links to that VM's kernel,
  rootfs, and snapshot files;
- assigns each concurrently live VM a different unprivileged uid/gid
  (`100000 + network slot`);
- keeps Firecracker's own seccomp filter enabled;
- keeps the VMM in the Pod's existing Kubernetes cgroup and mount namespace.

The external jailer remains available with `--jailer` for conventional hosts,
and the direct launcher remains available when neither jail mode is selected.
The tradeoff is explicit: the CKS launcher gives up the stock jailer's nested,
per-VMM mount namespace to remove `CAP_SYS_ADMIN`; it retains the chroot,
distinct UID, device minimization, empty environment, and zero-capability VMM
inside the container runtime's mount boundary.

## Gateway/node split

The existing fleet-node mode is already the right control-plane seam:

```text
Internet
   |
unprivileged gateway Deployment
  SSH / HTTPS / auth / accounts / placement / OIDC
   |
authenticated fleet control + guest data path
   |
dedicated CKS node runtime
  KVM / TAP / chroot launcher / VM disks / local metadata relay
   |
one jailed Firecracker process per sandbox
```

The CKS manifests now enforce this shape. The gateway runs as UID/GID 65532,
with `RuntimeDefault` seccomp, every capability dropped, privilege escalation
disabled, and a read-only container root. It has no `/dev/kvm`, `/dev/net/tun`,
or hostPath. The node has no public Service and mounts neither the account
database nor `sparkbox-identity`; it receives only a public gateway host-key
pin and the public upstream login key used to prepare the trusted base template.
A default-deny ingress policy admits the gateway's public/fleet ports and admits
no node ingress.

The one-time cutover copies gateway databases and edge certificate caches to
the RWX volume while the combined process is stopped. The node init container
then removes those databases, TLS cache, and fleet private keys from the local
hostPath before the VM runtime starts. VM inventory and disks never
leave the Node-local XFS filesystem.

Sparkbox's `--gateway` branch exits into node mode before opening users, routes,
schedules, OIDC, consoles, or the HTTP/SSH edge. The gateway uses
`--gateway-only --driver mock`; a remote-only placer prevents local creates and
ordinary creates automatically select an online fleet node. A 1 MiB local
admission budget is a second fail-closed guard if a future control path reaches
the otherwise-unused mock manager.

## KVM/TUN device plugin

There is strong prior art. Kubernetes' stable
[Device Plugin API](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/)
lets a node plugin advertise an extended resource and return the exact device
node and permissions during `Allocate`. KubeVirt uses this pattern for
`/dev/kvm`, `/dev/net/tun`, and `/dev/vhost-net`: its
[controller creates generic plugins for those paths](https://github.com/kubevirt/kubevirt/blob/main/pkg/virt-handler/device-manager/device_controller.go),
and its [Allocate implementation returns a `DeviceSpec`](https://github.com/kubevirt/kubevirt/blob/main/pkg/virt-handler/device-manager/generic_device.go).
Because KVM and TUN are shared character devices rather than consumable PCI
functions, KubeVirt advertises a configurable number of synthetic device IDs
that all map back to the same host path.

Sparkbox now runs a small, in-tree plugin that advertises one synthetic
`sparkbox.dev/kvm` and `sparkbox.dev/tun` allocation per eligible physical
Node. The VM runtime requests:

```yaml
resources:
  limits:
    sparkbox.dev/kvm: "1"
    sparkbox.dev/tun: "1"
```

The plugin validates that the host paths remain device nodes, registers again
when the kubelet socket changes, and returns `rwm` `DeviceSpec` entries during
allocation. The plugin Pod is root only so it can create sockets in the
kubelet-owned directory; it is not privileged, has no Linux capabilities, has
a read-only root, mounts each health device read-only, and has no service
account token.

This improves scheduling and health reporting, applies device-cgroup scoping,
and removes the KVM/TUN hostPaths from the VM node. The long-lived node is now
`privileged: false` and has no `CAP_SYS_ADMIN`:

- TAP creation and route/filter setup still need `CAP_NET_ADMIN`;
- chroot construction needs `MKNOD`, `SYS_CHROOT`, and uid/gid transition
  privileges in the controller before it execs each zero-capability VMM;
- trusted base-template patching still needs `CAP_SYS_ADMIN` and loop devices,
  but now runs in a one-shot init container that exits before the controller
  accepts fleet or guest work.

The current plugin therefore also advertises one temporary
`sparkbox.dev/loop` bundle containing `/dev/loop-control` and `/dev/loop0`
through `/dev/loop7`. That is deliberately one atomic allocation, not eight
independent claims. Only the preparation init container receives it; the
long-lived node requests KVM and TUN only. The allocation remains held for the
Pod lifetime, but there is no running process in that container after startup.

The VM-node container drops every default capability, adds only `CHOWN`,
`DAC_OVERRIDE`, `FOWNER`, `KILL`, `MKNOD`, `NET_ADMIN`, `SETGID`, `SETUID`, and
`SYS_CHROOT`, has a read-only root filesystem, and receives a bounded `/tmp`.
Host PID/network/IPC namespaces and service-account credentials remain absent.
Seccomp and AppArmor are still explicitly unconfined while the chroot launcher
is validated on CKS; selecting a tested outer profile is the next independent
hardening step.

Remaining useful work is:

1. Remove the preparation init container's loop bundle after trusted-template
   tooling moves into the image build or a disposable helper microVM.
2. Re-enable template snapshots with guest-side first-boot sanitization or a
   mountless/disposable sanitizer; CKS currently refuses that operation rather
   than mounting an attacker-controlled disk.
3. Install a tailored outer seccomp/AppArmor profile around the controller and
   runtime. Kubernetes documents the available [kernel-level constraints](https://kubernetes.io/docs/concepts/security/linux-kernel-security-constraints/).

The architectural alternative is a node-level runtime shim or daemon rather
than a device-owning application Pod. Kata Containers puts the hypervisor in a
separate cgroup/network/SELinux environment, and
[firecracker-containerd](https://github.com/firecracker-microvm/firecracker-containerd/blob/main/docs/architecture.md)
uses an out-of-process runtime shim between containerd and each VMM. Sparkbox
does not need their OCI guest-agent machinery, but the ownership boundary is a
good model: a narrow node runtime owns virtualization; the public service calls
it over a constrained protocol.

## Guest disk handling

The guest controls the bytes and directory entries in its rootfs. Mounting that
filesystem in the capability-bearing management Pod enlarges the trusted
computing base from Firecracker/KVM to the host ext4 driver and every root
process that walks the mount.

This change closes three immediate issues in CKS:

- snapshot sanitization now uses `os.Root` beneath-root operations, so a guest
  symlink cannot redirect `/etc/hostname`, SSH-key removal, or secret cleanup
  into the management container;
- the agent-tool refresher no longer loop-mounts `snap-*.ext4` user-derived
  templates. It patches only release/operator base images in the init container;
- `Create` no longer mounts each VM disk to install the gateway key. The deploy
  script derives the gateway's public upstream key into the public-only node
  trust Secret, and the init container bakes it into the trusted base template;
- template snapshot creation is explicitly refused under
  `--disable-host-rootfs-mounts` instead of silently recovering the old mount.

The remaining parsing is explicit technical debt:

- the startup refresher loop-mounts trusted release/operator base images in the
  completed init container;
- `e2fsck`, `resize2fs`, `zerofree`, and `debugfs` parse filesystem metadata in
  host userspace even when they do not mount it.

The end state should put first-boot/fork identity work inside the microVM. Pass a
public gateway key and a "regenerate identity" marker over a bounded boot or
metadata channel; an early guest service installs the key, clears machine/SSH
identity on a fork, and starts SSH only afterwards. Snapshot creation then
becomes a reflink of opaque bytes. Compaction and repair can either be skipped,
or run in a disposable helper microVM that receives only the target block image
and returns a replacement. At that point no attacker-controlled filesystem is
mounted or parsed by the long-lived VM runtime.

## Acceptance gates

Before calling the CKS sandbox production-strength:

- verify chroot launch, pause/resume, destroy, and stale-jail cleanup on the
  actual CKS kernel and container runtime;
- prove every VMM runs under its assigned non-root uid with a distinct chroot;
- deny node-runtime ingress except the authenticated fleet/control path;
- default-deny guest east-west, metadata, node, and control-plane traffic, then
  explicitly allow required egress;
- remove host parsing of user-derived disks;
- run Firecracker/KVM advisories and kernel updates as a release gate;
- fuzz/authorize the node control API and cap VM count, memory, disk, file
  descriptors, CPU, and network independently of guest cooperation.
