# Deploying sparkbox on a real KVM host (Hetzner)

The mock driver runs anywhere; the firecracker driver needs `/dev/kvm`, which
means bare metal (or one of the few nested-virt clouds). Verified as of July
2026: **Hetzner Cloud does NOT support nested virtualization on any tier**
([official FAQ](https://docs.hetzner.com/cloud/servers/faq/)), and Hetzner has
no US dedicated servers — Robot/dedicated is Falkenstein/Nuremberg/Helsinki
only. See `docs/baremetal-hosting.md` at the repo root docs for the full
provider comparison.

## Recommended dev/staging box

A **Hetzner Server Auction** machine: €51–65/mo gets 64 GB RAM + NVMe
(i7-class, non-ECC), no setup fee, provisions in minutes, unlimited 1 Gbit
traffic. Order via [the auction](https://www.hetzner.com/sb/); it arrives
booted into the rescue system.

## Bring-up

```sh
# 1. In the rescue system: install Ubuntu 24.04 with XFS (reflink CoW copies
#    of rootfs templates depend on it; ext4 falls back to full copies).
installimage -n sparkbox-1 -r no -i images/Ubuntu-2404-noble-amd64-base.tar.gz \
  -p /boot:ext4:1G,/:xfs:all

# 2. After reboot: verify KVM.
ls -l /dev/kvm

# 3. Install firecracker (static binary).
ARCH=$(uname -m)
REL=$(curl -s https://api.github.com/repos/firecracker-microvm/firecracker/releases/latest | grep -o '"tag_name": "[^"]*' | cut -d'"' -f4)
curl -L "https://github.com/firecracker-microvm/firecracker/releases/download/${REL}/firecracker-${REL}-${ARCH}.tgz" | tar xz
install release-*/firecracker-*[!.debug] /usr/local/bin/firecracker
install release-*/jailer-*[!.debug] /usr/local/bin/jailer

# 4. Get a guest kernel: use the CI vmlinux Firecracker's quickstart points
#    at, or build one with their recommended microVM config
#    (resources/guest_configs in the firecracker repo).

# 5. Build a rootfs template (bakes in the gateway's public key). The fleet
#    default is codex-universal (~30GB of toolchains; the ext4 is a thin
#    ceiling — reflink clones only pay for written blocks). For a slim box use
#    ubuntu:24.04 with size 4096 and pass a matching --default-image below.
./hack/build-rootfs.sh ghcr.io/openai/codex-universal:latest /srv/sparkbox/images/universal.ext4 gateway_upstream_key.pub 65536

# 6. Kernel networking: forwarding so sandboxes reach the internet, and strict
#    reverse-path filtering so a guest can't source-spoof a neighbour (the
#    metadata service identifies its caller by source address).
#    /etc/sysctl.d, not /proc, so it survives a reboot.
cat > /etc/sysctl.d/99-sparkbox.conf <<'EOF'
net.ipv4.ip_forward=1
net.ipv4.conf.all.rp_filter=1
net.ipv4.conf.default.rp_filter=1
EOF
sysctl -q --system

# 7. NAT for sandbox egress + lock the metadata port to taps (172.30.0.0/16).
#    iptables rules are not persistent state, so a boot unit owns them —
#    applying them by hand means losing sandbox egress at the next reboot.
install -m 0755 deploy/sparkbox-net.sh /usr/local/sbin/sparkbox-net.sh
systemctl enable --now sparkbox-net.service   # unit body: see hack/setup-host.sh

# 8. Run it.
sparkbox serve --driver firecracker \
  --state-dir /srv/sparkbox/state \
  --kernel /srv/sparkbox/vmlinux \
  --image-dir /srv/sparkbox/images \
  --jailer /usr/local/bin/jailer \
  --users /srv/sparkbox/users.conf \
  --ssh-addr :22 --api-addr 127.0.0.1:8080
```

(To run the gateway on port 22, move the host's sshd to another port first —
or keep the gateway on 2222 while iterating.)

## Production gaps to close before real multi-tenant use

- **Jailer cgroups.** The launcher now supports the jailer's chroot and dropped
  per-VM uid/gid. Standalone production hosts should additionally place each
  VMM in a resource-bounded cgroup; Kubernetes deployments inherit the Pod
  cgroup as their outer resource boundary.
- **Warm snapshots.** Boot each template once to sshd-ready, snapshot, and
  make Create() restore a CoW clone of that snapshot instead of cold-booting
  (the sub-second acquisition path from the design doc).
- **Resource limits.** Balloon device management and I/O rate limiting via the
  SDK; disk quotas on the per-VM ext4.
- **Snapshot hygiene.** Reseed guest entropy and re-sync the guest clock after
  restore; keep the fleet CPU-homogeneous (snapshots aren't portable across
  CPU models).
- **Networking.** Per-VM iptables rules to block inter-sandbox traffic on the
  bridge (taps currently share one /16 with no isolation between VMs).
