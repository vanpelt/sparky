# Deploying sparkbox on Scaleway Elastic Metal

Scaleway Elastic Metal is real bare metal with **hourly billing and no
commitment fee** (the one-month commitment fee applies to monthly billing
only), provisioning in minutes with a proper API — ideal for validating the
firecracker driver without renting a box long-term.

**Recommended offer:** `EM-A610R-NVMe` — AMD Ryzen PRO 3600 (6c/12t), 32 GB
ECC, 2× 1 TB NVMe, **€0.11/h** (€39.99/mo monthly), zone `fr-par-1`, 500 Mbps.
Roughly €2.60/day while you experiment. The €0.077/h `EM-A116X-SSD` exists but
its 4c/4t 2011-era Xeon isn't worth the savings. (Prices July 2026, excl.
VAT: [pricing](https://www.scaleway.com/en/pricing/elastic-metal/).)

> **Validated on `EM-B220E-NVMe`** (the full lifecycle below + the zero-touch
> pipeline in §6). A610R is frequently out of stock and per-offer quota starts
> at 0 on fresh accounts — see [Gotchas](#gotchas). Any Ryzen/EPYC-class NVMe
> offer works; keep the whole fleet on **one** CPU family so snapshots stay
> portable.

## 1. Account + CLI

1. Sign up at [console.scaleway.com](https://console.scaleway.com), complete
   identity/payment verification (first-time accounts may need a short review
   before bare metal is orderable).
2. Add your SSH public key: console → *Project → SSH keys* (or
   `scw iam ssh-key create`). This is the key the OS installer bakes into the
   box's `ubuntu` user.
3. Install and init the CLI:

```sh
brew install scw   # or: curl -fsSL https://raw.githubusercontent.com/scaleway/scaleway-cli/master/scripts/get.sh | sh
scw init           # paste an API key from console → IAM → API keys
```

## 2. Order + install the server

```sh
# What's in stock (hourly-billed offers):
scw baremetal offer list zone=fr-par-1 subscription-period=hourly

# Create (this reserves the hardware; billing starts):
scw baremetal server create name=sparkbox-1 type=EM-A610R-NVMe zone=fr-par-1

# Pick an OS (grab the Ubuntu 24.04 ID):
scw baremetal os list zone=fr-par-1

# Install — SERVER_ID from create; your SSH keys go to the 'ubuntu' user:
scw baremetal server install <SERVER_ID> zone=fr-par-1 \
  os-id=<UBUNTU_24_04_ID> all-ssh-keys=true

# Watch until status becomes 'ready' (install takes ~10 min):
scw baremetal server get <SERVER_ID> zone=fr-par-1
```

The whole thing is also one console flow: *Elastic Metal → Create server →
pick offer/zone/hourly → pick Ubuntu 24.04 → select SSH keys*. Docs:
[CLI guide](https://www.scaleway.com/en/docs/elastic-metal/api-cli/elastic-metal-with-cli/),
[quickstart](https://www.scaleway.com/en/docs/elastic-metal/quickstart/).

## 3. Set up the host

```sh
ssh ubuntu@<server-ip>
sudo -i

# users.conf line = "<username> <your laptop public key>"
curl -fsSL https://raw.githubusercontent.com/vanpelt/sparky/<branch>/tools/sparkbox/hack/setup-host.sh -o setup-host.sh
chmod +x setup-host.sh
./setup-host.sh "vanpelt $(cat /home/ubuntu/.ssh/authorized_keys | head -1)"
```

`hack/setup-host.sh` handles everything: verifies `/dev/kvm`, installs
firecracker + a Firecracker-CI guest kernel, creates an **XFS loopback
volume** for reflink CoW rootfs copies (Scaleway's default install is ext4,
which can't reflink), builds sparkbox, generates gateway keys, builds an
`ubuntu.ext4` rootfs template from `ubuntu:24.04` with the gateway key baked
in, and sets up NAT for sandbox egress. It prints the `sparkbox serve`
command when done.

## 4. Validate

```sh
# on the host:
sparkbox serve --driver firecracker --state-dir /srv/sparkbox/data/state \
  --kernel /srv/sparkbox/vmlinux --image-dir /srv/sparkbox/data/images \
  --users /srv/sparkbox/users.conf --ssh-addr :2222 --api-addr 127.0.0.1:8080

# from your laptop:
ssh -p 2222 new@<server-ip>        # creates a sandbox
ssh -p 2222 <name>@<server-ip>     # real microVM shell

# numbers worth capturing while you're here:
time curl -s -XPOST localhost:8080/v1/sandboxes -d '{"name":"t1","owner":"vanpelt"}'  # cold create
curl -s -XPOST localhost:8080/v1/sandboxes/t1/pause
time ssh -p 2222 t1@<server-ip> true                                                  # snapshot resume
```

## 5. Zero-touch fleet provisioning (recommended)

§3–4 build sparkbox from source **on** the host — great for a first box, slow
for a fleet. For repeatable deploys, build once and let new hosts self-provision
by fetching prebuilt artifacts over cloud-init. No SSH, no compiler on the box.

**Build + publish a release** (once, from any host with the repo, docker, go,
firecracker, a guest `vmlinux`, and `rclone` pointed at a Scaleway Object
Storage bucket). The rootfs bakes only the gateway *public* key; the *private*
keys are the fleet secret and are never uploaded.

```sh
# on a build host (e.g. an existing sparkbox box):
RELEASE=v1 tools/sparkbox/hack/build-artifacts.sh
# -> uploads vmlinux, firecracker, sparkbox, ubuntu.ext4.gz, manifest.env
#    to  <bucket>/releases/v1/  (public-read) and points latest.env at it.
```

**…or publish from CI.** `.github/workflows/build-artifacts.yml` runs the exact
same script on a GitHub runner — pick *Run workflow* under Actions (inputs: an
optional `release` tag, `promote_latest`, and the rootfs base image). It needs
three one-time settings, copied from your local `scw` + fleet key:

```sh
printf %s "$(scw config get access-key)" | gh secret set SCW_ACCESS_KEY
printf %s "$(scw config get secret-key)" | gh secret set SCW_SECRET_KEY
# the fleet gateway PUBLIC key baked into the rootfs (derive from the private key):
gh variable set GATEWAY_UPSTREAM_PUBKEY \
  --body "$(ssh-keygen -y -f secrets/gateway_upstream_key.pem)"
```

The Scaleway API key doubles as S3 credentials. `workflow_dispatch` only appears
once the workflow file is on the **default branch**, so merge it to `main` first.

**Launch a self-provisioning host.** `launch-host.sh` renders your secrets (your
laptop pubkey + the fleet gateway *private* keys) into cloud-init user-data,
creates + installs the server, and walks away. On first boot cloud-init fetches
+ sha256-verifies the release, builds the XFS reflink volume + egress NAT, and
starts `sparkbox.service`.

```sh
GATEWAY_HOST_KEY=secrets/gateway_host_key.pem \
GATEWAY_UPSTREAM_KEY=secrets/gateway_upstream_key.pem \
RELEASE=v1 \
tools/sparkbox/deploy/launch-host.sh
# then, ~1-2 min after the box reports 'ready':
ssh -p 2222 new@<server-ip>        # a real microVM, cold
```

The gateway public/private keypair is **fleet-wide**: its public half is baked
into every release's rootfs so every sandbox trusts any fleet gateway, and the
private half is injected per-host as a secret. Generate it once (setup-host.sh
does on the first box) and reuse it across every release + host.

**Stable DNS via flexible IPs (optional but recommended).** Each new box gets a
fresh ephemeral IP, which means re-pointing DNS on every rebuild. Reserve
Scaleway *flexible* IPs once and have `launch-host.sh` move them onto each new
box instead. Reserve them (`scw fip ip create`, add `is-ipv6=true` for a v6
/64), point Cloudflare `A`/`AAAA` at them **once** (grey / DNS-only), then pass
their IDs + host addresses on every launch:

```sh
FLEXIBLE_FIP_IDS=<v4-fip-id>,<v6-fip-id> \
FLEXIBLE_ADDRS="62.210.142.210/32 2001:bc8:702:1c7::1/64" \
GATEWAY_HOST_KEY=... GATEWAY_UPSTREAM_KEY=... \
tools/sparkbox/deploy/launch-host.sh
```

`launch-host.sh` attaches the flexible IPs to the new server (they route to its
NIC — no virtual MAC needed) and cloud-init pins `FLEXIBLE_ADDRS` on the primary
interface via a netplan drop-in that keeps DHCP for the default route. For a /64
use its `::1` host address, not the bare `/64`. DNS never changes again.

Timings observed on `EM-B220E-NVMe`: OS install ~10 min, then cloud-init ~4–5
min to fetch the ~155 MB release and serve the first microVM — fully hands-off.

## 6. Tear down (stop the meter)

```sh
# Detaches reserved flexible IPs (keeps them for the next box), then deletes:
tools/sparkbox/deploy/teardown-host.sh                 # auto-detects a single box
# or: SERVER_ID=<id> tools/sparkbox/deploy/teardown-host.sh
```

`teardown-host.sh` detaches any flexible IPs *before* deleting the server —
otherwise they stay "attached" to the dead server id and orphan (still billed,
awkward to reclaim). It keeps the IPs by default (they carry your DNS); pass
`DELETE_FLEXIBLE_IPS=1` to actually release them. Raw equivalent:

```sh
scw fip ip detach fips-ids.0=<fip-id> zone=fr-par-1   # per attached IP, first
scw baremetal server delete <SERVER_ID> zone=fr-par-1
```

## Gotchas

- **Hourly ≠ suspendable:** billing runs while the server exists, powered on
  or off. Delete it when done; the disk contents are gone with it, so push
  anything you care about.
- **Zone stock varies.** If `fr-par-1` is out of A610R, check other offers
  with `scw baremetal offer list` — anything Ryzen/EPYC with NVMe works.
- **Per-offer quota starts at 0.** Fresh/unverified accounts get `0/0` on most
  Elastic Metal offers, and `create` fails with a quota error. Complete
  **identity verification** in the console (IAM → verify) — it lifted every
  offer's quota at once for us. Until then, probe offers individually; one tier
  (for us `EM-B220E-NVMe`) sometimes has quota while others read 0.
- **Confirm deletes.** `scw baremetal server delete` has silently no-op'd for us
  once (server kept billing). Always **re-list** afterward — `scw baremetal
  server list` should no longer show it — and don't trust the delete until it's
  gone.
- **Kernel:** the host kernel is stock Ubuntu (fine); the *guest* vmlinux
  comes from Firecracker's CI bucket via the setup script. Keep the fleet on
  one CPU family — snapshots aren't portable across CPU models, and Scaleway
  offers mix Intel/AMD across ranges.
- Remote console (KVM-over-IP) availability varies by offer; if an install
  wedges, use `scw baremetal server reboot ... boot-type=rescue`.
