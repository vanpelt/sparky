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

## 5. Tear down (stop the meter)

```sh
scw baremetal server delete <SERVER_ID> zone=fr-par-1
# flexible IPs are billed separately (€0.005/h) until released, if you added one:
scw fip ip list  # then: scw fip ip delete <ID>
```

## Gotchas

- **Hourly ≠ suspendable:** billing runs while the server exists, powered on
  or off. Delete it when done; the disk contents are gone with it, so push
  anything you care about.
- **Zone stock varies.** If `fr-par-1` is out of A610R, check other offers
  with `scw baremetal offer list` — anything Ryzen/EPYC with NVMe works.
- **Kernel:** the host kernel is stock Ubuntu (fine); the *guest* vmlinux
  comes from Firecracker's CI bucket via the setup script. Keep the fleet on
  one CPU family — snapshots aren't portable across CPU models, and Scaleway
  offers mix Intel/AMD across ranges.
- Remote console (KVM-over-IP) availability varies by offer; if an install
  wedges, use `scw baremetal server reboot ... boot-type=rescue`.
