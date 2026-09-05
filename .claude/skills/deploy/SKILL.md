---
name: deploy
description: Deploy sparkbox to the CoreWeave CKS cluster that serves catnip.sh, or bring up / refresh the local Mac dev environment. Use when asked to deploy, roll, ship, or update sparkbox — on CKS (a main build, an unmerged branch build, a release tag), on the local dev box, or to pin a hivemind release candidate onto real hardware.
---

# Deploy sparkbox

Two targets, and they are not the same procedure. **CKS is the live one and is
everything below.** The local Mac dev environment has its own, much shorter,
one — see [Deploying to the Mac dev box](#deploying-to-the-mac-dev-box) at the
end. If the request does not say which, ask: "deploy" has meant CKS
historically, but rolling a change onto the dev box first is usually what
someone wants when they are mid-change.

# Deploy sparkbox to CKS

The live `catnip.sh` host is the **CoreWeave CKS** POC, not the DGX Spark. Any
doc or memory that says otherwise is stale.

| | |
|---|---|
| kubectl context | `cvp-hivemind-test-east-06_US-EAST-06A` |
| namespace | `sparkbox-poc` |
| pinned Node | `g084f44` (pool `default-node-pool`) |
| LoadBalancer | `166.19.38.111` (apex + wildcard are DNS-only A records) |
| public domain | `catnip.sh` |
| run from | `tools/sparkbox/` |

Depth lives in `tools/sparkbox/docs/deploy-cks.md` — read the relevant section
rather than reconstructing procedure. This skill is the operating checklist.

**Merging to main is not a deployment.** The CKS image workflow publishes an
image; the cluster keeps running whatever digest its Deployments name until
someone re-runs `deploy.sh`.

---

## 1. Preflight: is the kubeconfig token alive?

```sh
kubectl config current-context
kubectl get --raw /api
```

A bare **`403 Forbidden` with an empty message on `get --raw /api` is the
CoreWeave token being expired, not RBAC.** Every request answers 403, including
the server half of `kubectl version`, which reads exactly like a permissions
problem and is not. There is no `cw`/`coreweave` CLI on this laptop — the user
refreshes the token from the CoreWeave console. Stop and ask them.

## 2. Resolve an immutable image digest

`sparkbox-cks-image.yml` fires on push to **any** branch and tags `sha-<sha>`,
so an unmerged PR deploys the same way as main — no release needed.

Confirm the build finished before resolving:

```sh
gh run list --workflow 'sparkbox CKS image' --branch main --limit 5
```

Then:

```sh
SHA=$(git rev-parse origin/main)          # or the branch tip
REPO=ghcr.io/vanpelt/sparkbox-cks
IMAGE="$REPO@$(crane digest "$REPO:sha-$SHA")"
```

- Tags move; deploy the digest. Pin the **OCI index** digest (what `crane digest`
  prints), not the child image digest a runtime reports.
- Two commits merged in one push produce **one** image, for the tip. A missing
  tag for a middle commit is expected — use the tip.
- **The tip itself can have no `sha-` tag.** The workflow skips commits that
  cannot change the image — tests, `docs/`, `hack/`, the guest `images/` dir —
  so a branch ending in a test-only commit never built one. That is not a
  failed build; there is simply nothing new to deploy. Fall back to the branch
  tag, which names the newest image the branch produced (`/` becomes `-`):

  ```sh
  IMAGE="$REPO@$(crane digest "$REPO:$(git rev-parse --abbrev-ref HEAD | tr / -)")"
  ```

  Check `gh run list` first — a build still running looks the same from here.
- `:edge` also tracks main, but resolve it to a digest rather than deploying a
  moving tag.

### Deploying a release tag

Only deploy `:vX.Y.Z` when that release did **not** move the guest artifact pin.
A release image is built from the tagged commit, which still pins the *previous*
release's artifacts (the new checksums cannot exist until the release
publishes). When the pin moved, the right image is the **post-pin commit on
main**, not the tag. See `sparkbox-releases`; v0.7.0 was the trap, v0.7.2 was fine.

## 3. Read the live config back — do not reconstruct flags

`deploy.sh` rebuilds every Pod template from the manifests in this repository,
not from the live objects, so **any setting that reached the cluster only as a
flag must be passed again.** The deployed env is the ground truth for what to
re-pass:

```sh
kubectl -n sparkbox-poc get deployment sparkbox-gateway \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep -E 'PROXY_DOMAIN|GITHUB_APP_CLIENT_ID|HIVEMIND'

kubectl -n sparkbox-poc get deployment sparkbox-node \
  -o jsonpath='{range .spec.template.spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep SPARKBOX_RELEASE          # the guest artifact pin

kubectl -n sparkbox-poc get pods \
  -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image'
```

`--proxy-domain` is the one that bites. The script now carries the live domain
forward and prints `Keeping the deployed public domain`, but pass it anyway —
reverting it invalidates every published sandbox URL and the WebAuthn origin.
`--node` likewise: the script infers it on a single-Node pool, but naming it
keeps the hot tier pinned where the guest disks already are.

## 4. Price the roll before you start it

```sh
hack/check-cks-pin.sh amd64
git diff <live SPARKBOX_RELEASE>..HEAD -- images/ hack/build-*.sh
```

- **Diff empty / pin unchanged** → the node reuses its cached firecracker,
  kernel and rootfs. Roll takes about a minute.
- **Pin moved** → the node re-downloads the ~750 MB release payload and
  decompresses a sparse ext4. Budget for it and warn the user.

## 5. Quiesce running sandboxes

`deploy.sh` rolls `sparkbox-node`, which hosts the microVMs. **The node restart
stops every running VM abruptly.** Disks survive on the hot tier; unsaved work
inside a running guest does not.

```sh
ssh -p 22 ctl@ssh.catnip.sh list
ssh -p 22 ctl@ssh.catnip.sh pause <name>
```

Sandboxes are scale-to-zero, so ssh-ing to one afterwards wakes it again.

## 6. Roll

```sh
deploy/kubernetes/deploy.sh \
  --image "$IMAGE" \
  --node-pool default-node-pool --node g084f44 \
  --proxy-domain catnip.sh \
  --github-app-client-id <from step 3> \
  --hivemind-api https://hivemind.wandb.tools \
  --hivemind-manifest https://raw.githubusercontent.com/wandb/hivemind/main/manifests/hivemind-<version>.json \
  --public-key ~/.ssh/id_ed25519.pub --user vanpelt
```

Idempotent: unchanged manifests report `unchanged`, and only Deployments whose
Pod template actually changed roll. Confirm the domain in the closing summary.

### The hivemind manifest is deliberately NOT carried forward

Unlike `--proxy-domain` and `--hivemind-api`, a hivemind pin is meant to end —
one that silently reinstated itself every deploy would be the worse failure. So
**re-pass `--hivemind-manifest` on every deploy or new sandboxes drop to
`latest`.** A run that drops one says so in its output.

Pin an exact `hivemind-<version>.json`. The sibling `hivemind-prerelease.json`
is a moving pointer that advances rc2 → rc3 → …, which is drift, not a pin.

This reaches **newly created** sandboxes only: the refresher patches the rootfs
template, and an existing VM keeps the hivemind on its own disk across
pause/resume.

## 7. Verify

```sh
kubectl -n sparkbox-poc get pods -o wide          # expect 3/3 ready, 0 restarts
kubectl -n sparkbox-poc exec deployment/sparkbox-gateway -- sparkbox version
kubectl -n sparkbox-poc exec deployment/sparkbox-node -c sparkbox-node -- sparkbox version

ssh -p 22 ctl@ssh.catnip.sh node ls    # both machines approved and online
ssh -p 22 ctl@ssh.catnip.sh list       # the inventory survived
curl -sS -o /dev/null -w '%{http_code}\n' https://my.catnip.sh/

ssh <name>@ssh.catnip.sh -- uname -a   # the only end-to-end VM check
```

`sparkbox version` prints `<full sha>-cks` for a main or branch build, and a
clean `sparkbox vX.Y.Z (linux/amd64)` for a release-tag image. It should match
the SHA you resolved, on **both** containers.

## 8. Expected noise, and what does not survive

- `WARN HiveMind monitor poll failed … nodelink: this node has no link to its
  gateway right now` in the node log at startup is **normal** — the link comes up
  milliseconds later. Do not chase it.
- The node re-enrols on start and keeps its `cks-poc` identity and approval. No
  re-approval needed.
- **Browser sessions are invalidated.** The `spk_v1` cookie stops authenticating
  and `go.catnip.sh` bounces to `login.catnip.sh` wanting a passkey. To verify
  guest-side behaviour without logging back in, use `ssh -tt <box>@catnip.sh` —
  it needs no session and exercises the same code.
- Extra Service ports added with `public-port.sh` **do not survive** the re-apply
  of `service.yaml`. Re-add any port outside the declared set afterwards.
- Certificates and control databases live on the `sparkbox-durable` PVC and are
  reused — no ACME re-issuance.

---

## Related traps

- **Merging main into a long-lived branch needs a THIRD `IDENTITY_REV`.** If both
  sides bumped it independently, one number names two different guest payloads
  and git shows an ordinary content conflict. Taking either side is wrong. Go to
  the first value neither side can have stamped, then run
  `go test ./deploy -run TestIdentityRevMovesWithThePayload` for the new
  `wantPayloadSum`. Get this wrong and there is **no error anywhere** — every
  host concludes "templates already current" and the guest change reaches no VM.
- **Never run an unbounded `grep -a` over a template ext4 inside the node
  container.** They are 25 GB sparse; it OOM-killed `sparkbox-node` into
  CrashLoopBackOff and stopped every running VM. Bounded form:
  `ulimit -v 262144; tr "\0" "\n" < f | grep -a -o "NAME=.\{0,6\}" | sort -u | head`.
- Pooled per-owner disk budgets got **more generous** on the roll that carried
  the baseline change (owners are charged `max(0, DiskMB - BaseDiskMB)`). It takes
  effect on a binary swap with no flag. Re-check `--disk-pool-mb-per-owner`
  against what you meant; reasoning in `docs/resource-model-design.md`.

---

# Deploying to the Mac dev box

A separate target with a separate procedure: `tools/sparkbox/hack/dev/`, a
gateway running natively on macOS plus the five-container node Pod inside
Apple's container machine, booting real aarch64 Firecracker guests. Nothing
here touches CKS and nothing above applies — no kubectl, no image digest, no
release pin.

**Read `tools/sparkbox/hack/dev/README.md` rather than reconstructing this.**
It is the reference; what follows is only the deploy-shaped part of it.

## Roll a working-tree change onto it

```sh
cd tools/sparkbox
hack/dev/up.sh                 # idempotent, safe to re-run, ~1 min warm
```

`up.sh` is the whole thing: it ensures the container machine, builds the image
**from the live working tree**, pushes it to the local registry, pulls it into
the machine, starts the gateway from the production entrypoint, seeds the trust
bundle, starts the node Pod and approves it by fingerprint.

Two things to know before running it:

- **A converged environment is left alone.** If the node is already online and
  carrying sandboxes, `up.sh` will not restart it, so a re-run does not cost
  somebody their build. To rebuild anyway: `hack/dev/up.sh down && hack/dev/up.sh`.
- **Gateway-only changes do not need it.** `hack/dev/gateway.sh restart` is
  1.7s warm, ~6s after a relink, and covers the edge proxy, auth, consoles,
  REST API and SSH doors. Reach for `up.sh` when the change is in the node, the
  guest payload, or the manifests.

`hack/dev/up.sh status` reports all three tiers read-only; `down` stops the
gateway and the Pod but keeps the machine, the image and the node's data.

## Sizing is the trap here, not flags

CKS's trap is a flag that must be re-passed. The dev box's is the opposite: a
default that is correct on a cluster node and ruinous on a laptop.

A sandbox nobody sized gets the binary's built-in **4 vCPU / 12288 MB**, and
the node's manifest declares `SPARKBOX_HOST_MEM_MB=480000`. Inside a container
machine with ~12 GB and 8 cores that means one guest is larger than the whole
machine, and **nothing refuses it** — admission control only asks whether a
sandbox fits the host's RAM, which one VM at the ceiling technically does. So
the guest is promised memory that does not exist, and half the machine's cores
go to a guest the node has to supervise from the other half.

`up.sh` now measures the machine and passes `-host-mem-mb`, `-default-mem-mb`
and `-default-vcpus` sized to it (a third of RAM, half the cores; override with
`SPARKBOX_DEV_SANDBOX_MEM_DIVISOR` / `_CPU_DIVISOR`). If you drive
`sparkbox devpod up` by hand instead, pass all three — `devpod plan` marks both
omissions `[BLOCKING]`, and that line is the only warning you get.

## A guest pegging its vCPUs is the balloon until proven otherwise

Right-sizing does **not** fix a wedged guest, and the first correctly-sized
replacement wedged identically. The cause is the idle reaper: it used to
squeeze any sandbox idle for two minutes down to `--mem-reserve-mb` (256 MB),
including one that had not finished booting. Check firecracker's own log first:

```sh
hack/dev/guest.sh console <name> | head        # or, for the raw API log:
container machine run -i --root --name sparkbox -- bash -s <<'EOF'
docker logs sparkbox-dev-vmm-helper 2>&1 | grep -F 'Patch request on "/balloon"'
EOF
```

An `{"amount_mib": …}` close to the guest's whole `mem_size_mib` is the
diagnosis. The guest cannot recover from it on its own: the balloon deflates on
*activity*, `--activity-cpu-pct` is **0 by default**, and a guest starved before
its agent started moves no network traffic — so being squeezed is what makes it
look idle, which is what keeps it squeezed.

The reaper no longer squeezes below 1.5x the guest's measured working set, does
nothing until an owner pool or the node budget is ~85% full, and `--idle-balloon`
now defaults to 20 minutes rather than 2. **This applies to CKS too** —
`deployment.yaml` never pinned `--idle-balloon`, so production was running the
two-minute default against 12 GB guests.

## When a guest will not answer

The supported ways into a sandbox — `ssh <name>@gateway`, the browser terminal,
the REST API — all go through the in-guest agent on `:8000`. A guest whose boot
never finished has no agent, so all three answer *"could not reach the
sandbox's shell; it may still be starting"* and there is no next step.

`hack/dev/guest.sh` is that next step:

```sh
hack/dev/guest.sh console <name>        # the serial console: the boot log
hack/dev/guest.sh console <name> -f     # follow it
```

`console` needs nothing of the guest at all and ends with a **"still waiting"**
summary naming the units systemd is stuck on, which is usually the answer.

`console` is the only subcommand. There was a `shell` that forwarded into the
pod's network namespace to reach the guest's own sshd; it was removed because it
never reliably worked, and `guest.sh`'s header carries the `docker exec … ssh`
recipe to use instead.

## Verify

```sh
ssh -p 2222 ctl@127.0.0.1 node ls    # macdev approved and online
ssh -p 2222 ctl@127.0.0.1 list       # the inventory survived
ssh -p 2222 new+mybox@127.0.0.1      # a real aarch64 Firecracker guest
```

Then the console at `http://my.dev.localhost:8081` — browsers resolve
`*.localhost` with no `/etc/hosts`, but `curl` does not, so send the name in a
header instead.

## What a green run here is not evidence about

`hack/dev/README.md` has the full list under **NO SILENT CAPS**; the short
version is that this is a darwin/arm64 gateway against an arm64 guest, with no
scheduler, no NetworkPolicy, no LoadBalancer, no VAST durable tier, and a dev
GitHub App rather than the fleet's. Deploying here is not a rehearsal of
deploying to CKS. It is a much faster way to find out whether the change works
at all.
