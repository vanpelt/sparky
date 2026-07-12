# Bare-Metal / KVM Hosting for a Firecracker Sandbox Fleet

*Research snapshot: July 2026. Prices from provider pages/feeds where possible
(**[P]** primary); secondary trackers flagged **[S]**/approximate. EUR excl.
VAT. Companion to `agentic-sandbox-design.md` — the fleet needs `/dev/kvm`,
which rules out most cheap VPSes.*

## Four things that changed recently

1. **AWS added nested virtualization to regular EC2 (Feb 2026)** on Intel
   C8i/M8i/R8i/C7i/M7i/R7i families via
   `--cpu-options "NestedVirtualization=enabled"`, no extra cost. Firecracker
   no longer requires `.metal` on AWS — still ~10× the $/thread of dedicated,
   but fine for elastic burst. [P: AWS docs]
2. **Hetzner raised prices three times in 2026** (Apr 1 +3–21%, June 15
   restructure into `-1/-2/-3` SKUs with higher list prices plus cheaper
   supply-limited `-LTD` tiers). Existing rentals keep old pricing; the
   **server auction remains the value floor**. [P: Hetzner price-adjustment doc]
3. **Equinix Metal sunsets June 30, 2026** — don't build on it. Latitude.sh /
   Hivelocity / OVH are the migration paths. [S: DCD]
4. **netcup no longer enables nested virt on root servers** (official forum,
   Apr 2026) and **Hetzner Cloud has never supported it** ("No, this is not
   possible on cloud server" — official FAQ [P]). Both are out for Firecracker.

## Comparison (representative KVM-capable configs)

| Provider / SKU | CPU (c/t) | RAM | Price/mo | ~$/thread | Traffic | Regions | Notes |
|---|---|---|---|---|---|---|---|
| **Hetzner auction** [P: live feed 2026-07-12] | i7-7700 4/8 … i5-12500 6/12 | 64 GB | **€51–71** | ~€2.0–2.5 | Unlimited 1 Gbit | DE/FI | €0 setup, minutes-fast, non-ECC, delivered in rescue system |
| Hetzner AX42-1-LTD [P] | Ryzen 8700GE 8/16 | 64 GB DDR5 | €77.30 + €39 setup | €4.8 | Unlimited 1 Gbit | DE/FI | LTD = while supply lasts; AX42-1 list is €97.30 |
| Hetzner AX102-1 [P] | Ryzen 7950X3D 16/32 | 128 GB | €257.30 + €129 | €8.0 | Unlimited 1 Gbit | DE/FI | AX102-1-LTD €157.30 when in stock |
| Hetzner AX162-1 [P] | EPYC 9454P 48/96 | 128 GB ECC | €612.30 + €304 | €6.4 | Unlimited 1 Gbit | DE/FI | AX162-1-LTD €317.30; hourly billing now available |
| OVH Kimsufi KS-1 [P] | Xeon-D 1520 4/8 | 32 GB | $18.80 + $18.80 | $2.35 | Unlimited 500 Mbps | EU/CA | Absolute floor; ancient CPU, chronic stock-outs |
| OVH SYS-1 [~P] | Xeon-E 2136 6/12 | 32 GB | $33.20 + setup | $2.77 | Unlimited 500 Mbps | EU/CA | |
| OVH RISE-1 [P] | Xeon-E 2386G 6/12 | 32 GB | $70 + $70 | $5.83 | Unlimited 1 Gbps | EU/**US**/CA | Included anti-DDoS, Terraform provider, IPMI |
| OVH RISE-XL [P] | EPYC 9455 48/96 | 128 GB | $354 + $354 | $3.69 | Unlimited 1–3 Gbps | EU/US/CA | Best US-soil $/thread at scale |
| Scaleway EM-A610R [P] | Ryzen 3600 6/12 | 32 GB | €39.99 or **€0.11/h** | €3.33 | 500 Mbps unmetered | EU only | Minutes provisioning, excellent API, €0 setup hourly |
| Latitude.sh c3.small [P] | E-2386G 6/12 | 32 GB | $190 | $15.8 | 20 TB incl. | US+global | Best cloud-like API/instant provisioning; premium |
| Vultr Bare Metal [S] | E-2388G 8/16 | 128 GB | ~$350 | ~$22 | 10 TB, 10 Gbps | US+global | Hourly billing |
| Interserver [S] | varies | 64 GB | ~$64–80 | ~$4–7 | Unmetered 1 Gbps | US (NJ/LA) | Budget US pick |
| ReliableSite [S] | Ryzen 7950X 16/32 | 128 GB | ~$259–269 | ~$8.2 | Unmetered + 20 Gbps DDoS | US (NY/MIA/LA) | <10-min deploys claimed |
| Contabo VDS S [S] | 3 dedicated cores | 24 GB | ~$35 | ~$11.5/core | "Unlimited" | EU/US | Cheapest VPS with **official** nested virt [P: Contabo help]; oversell reputation |
| AWS m7i.2xlarge + nested [S] | 8 vCPU | 32 GB | ~$294 | ~$37 | $0.09/GB egress | Global | Elastic burst only |
| GCP c3-standard-192-metal [S] | 192 vCPU | 768 GB | ~$7,064 | ~$37 | ~$0.08–0.12/GB | 9 regions | Expensive baseline |

**IPv4** [P]: Hetzner primary €1.70/mo (now quoted separately from server
price); extra single €1.70 + €4.90 setup; /29 €13.60/mo, /28 €27.20/mo. OVH
additional IPs $2.40/mo (US) / €1.99 (EU). Scaleway flexible IP ~€3.65/mo.
This is why the design keeps VMs off public IPs entirely — at ~100 sandboxes
per host, per-VM IPv4 would roughly double the infra bill.

**Egress** is the hidden differentiator: hyperscalers charge ~$90/TB while
every dedicated provider above is unmetered or 10–20 TB included. Sandbox
fleets pulling container images and serving artifacts feel this quickly.

## Recommendations

- **(a) Production fleet, 1–5 boxes (EU ok):** Hetzner — shop the **auction**
  first (€51–71, €0 setup, minutes to provision, API-orderable via Robot),
  step up to AX102/AX162(-LTD) for density. Even post-hikes it's ~10–15×
  cheaper per thread than hyperscaler metal. Robot API covers ordering,
  rescue-mode boot, and unattended `installimage` installs — fully
  automatable. **No US dedicated exists at Hetzner** [P].
- **(b) Cheap dev/staging:** Hetzner auction again; or **Scaleway Elastic
  Metal hourly** (€0.11/h, real bare metal in minutes, delete when done) for
  ephemeral experiments; Kimsufi $18.80 if it's ever in stock; Contabo VDS
  ~$35 as the nested-virt VPS of last resort.
- **(c) US latency:** OVH US (RISE-1 $70, Vint Hill VA / Hillsboro OR) for
  value; Latitude.sh for the best API/instant provisioning; ReliableSite /
  Interserver for budget unmetered boxes.
- **(d) Firecracker gotchas:** host kernel must be a Firecracker-supported LTS
  (6.1 support ended Oct 2025; 6.18 needs Firecracker ≥ v1.16); Hetzner
  auction boxes arrive in the rescue system (a feature — scripted
  `installimage` with XFS for reflinks); disabling SMT for hard multi-tenant
  isolation halves effective threads — price that in when comparing SKUs;
  nested-virt hosts (AWS/GCP/Contabo) carry a ≥10% CPU and larger I/O penalty
  — fine for dev/CI, budget it out of prod.

## Concrete starting point

One **Hetzner auction box** (€53–65: 6–14 cores, 64 GB, 2× NVMe) runs the
whole MVP: sparkbox + ~50–100 concurrent 1 GB active sandboxes
(memory-bound), unlimited suspended ones on NVMe, unlimited traffic, one
€1.70 IPv4 for the gateway. Second box + OVH US when latency or capacity
demands it.

### Key sources

Hetzner [price adjustment](https://docs.hetzner.com/general/infrastructure-and-availability/price-adjustment/) ·
[IPv4 pricing](https://docs.hetzner.com/general/infrastructure-and-availability/ipv4-pricing/) ·
[auction](https://www.hetzner.com/sb/) ·
[cloud FAQ — no nested virt](https://docs.hetzner.com/cloud/servers/faq/) ·
[Robot API](https://robot.hetzner.com/doc/webservice/en.html) ·
OVH [eco lines](https://eco.ovhcloud.com/en/) · [Rise US](https://eco.us.ovhcloud.com/rise/) ·
[Scaleway Elastic Metal](https://www.scaleway.com/en/pricing/elastic-metal/) ·
[Latitude.sh](https://www.latitude.sh/pricing) ·
[AWS nested virt](https://docs.aws.amazon.com/AWSEC2/latest/UserGuide/amazon-ec2-nested-virtualization.html) ·
[Contabo nested virt](https://help.contabo.com/en/support/solutions/articles/103000271595) ·
[netcup forum — nested virt removed](https://forum.netcup.de/thread/22070-can-you-virtualise-on-a-root-server/) ·
[Firecracker kernel policy](https://github.com/firecracker-microvm/firecracker/blob/main/docs/kernel-policy.md)
