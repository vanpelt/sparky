---
name: deploy
description: Deploy sparkbox to the CoreWeave CKS cluster that serves catnip.sh. Use when asked to deploy, roll, ship, or update sparkbox on CKS — a main build, an unmerged branch build, a release tag — or to pin a hivemind release candidate onto real hardware.
---

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
