# Sparkbox on CoreWeave Kubernetes Service

This is a deliberately small proof of concept: one privileged Sparkbox Pod on
one CKS bare-metal CPU Node, with Firecracker guests nested under the Pod. A
public CKS LoadBalancer provides SSH and wildcard HTTP routing under
`coreweave.app`.

## What it creates

- Namespace `sparkbox-poc`.
- One `Deployment`, pinned to one exact amd64 Node in a CKS NodePool.
- A Node-local XFS `hostPath` at `/mnt/local/sparkbox-poc` for control state,
  the rootfs template, and live sandbox disks.
- A 100 GiB `shared-vast` PVC mounted at `/mnt/sparkbox-durable` for durable
  checkpoint objects.
- Host device mounts for `/dev/kvm` and `/dev/net/tun`.
- A public `LoadBalancer` Service with a `*.coreweave.app` record and ports 80,
  443, and 22.
- A Secret containing one operator's **public** SSH key.
- A separately provisioned `sparkbox-identity` Secret containing the stable
  gateway, OIDC, and node-control identity.

The Pod is privileged, but it does not use `hostNetwork`. TAP devices, sysctls,
NAT, and packet-filter rules therefore live in the Pod's network namespace
instead of modifying the CKS Node's Calico network namespace.

The named host path survives deletion and replacement of the Pod on the same
Node. The exact hostname selector makes that placement explicit. It is still an
ephemeral hot tier: CoreWeave local storage is encrypted with an in-memory key
and is lost when the Node reboots or is replaced. The Kubernetes identity
Secret and VAST checkpoint volume survive that Node loss; local control records
and guest disks do not. If that happens, redeploy against a new Node with the
same Secret and restore what has a committed checkpoint. Do not treat this
manifest as a production deployment.

Applying this version over the original `emptyDir` POC does not migrate that
Pod's data into the host path. The first rollout starts with empty state and
removes the old Pod's sandboxes, certificates, and account database. Capture
the generated identity into `sparkbox-identity` before that rollout, and export
anything else worth keeping before the one-time cutover.

The proposed path to durable identity and recoverable reflink-backed guest
disks is documented in
[`cks-reflink-persistence-plan.md`](cks-reflink-persistence-plan.md).

The released rootfs deliberately leaves out fast-moving agent CLIs. On every
Pod start, the Kubernetes bootstrap runs the same atomic template refresher as
other Sparkbox hosts. It installs current `claude`, `codex`, `pi`, and
`hivemind` binaries plus the guest workload-identity services before opening the
gateway. New sandboxes therefore have the complete agent toolchain without a
rootfs rebuild. Existing sandbox disks are not rewritten by a later refresh.

## Runtime image from GitHub Actions

`.github/workflows/sparkbox-cks-image.yml` builds this Containerfile for
`linux/amd64` and publishes it to GitHub Container Registry on every push that
changes Sparkbox or the workflow. It needs no registry secret: the workflow's
`GITHUB_TOKEN` receives `packages: write`.

Every build receives a traceable full-SHA tag and a branch tag. The default
branch also updates `edge`. Tags can still be moved, including the SHA tag, so
resolve the workflow result once and deploy the OCI index digest:

```sh
SHA=<the full commit SHA built by the successful workflow>
REPO=ghcr.io/vanpelt/sparkbox-cks
DIGEST=$(crane digest "$REPO:sha-$SHA")
IMAGE="$REPO@$DIGEST"
```

The digest is the immutable runtime image identity. The index includes the
linux/amd64 image plus its provenance attestation; pin the index digest rather
than copying the child image digest reported by one runtime. The agent CLI
versions remain intentionally fast-moving: the immutable image contains the
bootstrap logic, while the entrypoint resolves current Claude, Codex, Pi, and
HiveMind releases before patching a stale template.

GHCR packages are private until their visibility is changed. After the first
workflow run, open the `sparkbox-cks` package settings on GitHub and change its
visibility to public so CKS can pull it without an image-pull secret. The image
contains the current Sparkbox binary, host networking tools, and the template
refresher. At Pod startup it downloads and SHA-256-verifies the Firecracker,
kernel, and universal rootfs artifacts pinned to Sparkbox `v0.5.3`, then
downloads the current agent CLI bundles and patches the template. The first
start downloads the roughly 750 MB release payload plus those CLI bundles and
decompresses a sparse ext4 image. Later same-Node Pod starts reuse the cached
artifacts and template.

## Preserve the gateway identity

Sparkbox starts with `--key-dir=/run/sparkbox/keys --require-keys`. It never
silently creates a replacement identity in the local control directory. The
read-only `sparkbox-identity` Secret must contain:

```text
gateway_host_key.pem
gateway_upstream_key.pem
oidc_signing_key.pem
node_ca_cert.pem
node_ca_key.pem
gateway_control_key.pem
```

If OIDC key rotation is in progress, also include
`oidc_signing_key_prev.pem`. The other public keys are derived from their
private keys, and the renewable gateway control certificate is reissued from
the saved CA and gateway control key.

Before replacing the currently running, self-generated POC identity, capture it
directly from the sole running Sparkbox Pod:

```sh
deploy/kubernetes/capture-identity.sh
```

The helper auto-detects the original `/var/lib/sparkbox/state` layout and the
new `/var/lib/sparkbox/control` layout. It copies only the identity files
through a mode-`0700` temporary directory, validates that they look like PEM
keys/certificates, creates or updates `sparkbox-identity`, and removes the local
temporary copies. It prints filenames, never key material. Pass `--context`,
`--pod`, or `--source-dir` when auto-detection is ambiguous.

For a fresh namespace or disaster recovery, first apply `namespace.yaml`, then
create the same Secret from an escrowed directory without printing its data:

```sh
IDENTITY_DIR=/secure/path/sparkbox-identity
kubectl apply -f deploy/kubernetes/namespace.yaml
kubectl -n sparkbox-poc create secret generic sparkbox-identity \
  --from-file="gateway_host_key.pem=$IDENTITY_DIR/gateway_host_key.pem" \
  --from-file="gateway_upstream_key.pem=$IDENTITY_DIR/gateway_upstream_key.pem" \
  --from-file="oidc_signing_key.pem=$IDENTITY_DIR/oidc_signing_key.pem" \
  --from-file="node_ca_cert.pem=$IDENTITY_DIR/node_ca_cert.pem" \
  --from-file="node_ca_key.pem=$IDENTITY_DIR/node_ca_key.pem" \
  --from-file="gateway_control_key.pem=$IDENTITY_DIR/gateway_control_key.pem" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Add `--from-file=oidc_signing_key_prev.pem=...` when that rollover key exists.
The deploy script refuses to proceed when the Secret or any required entry is
missing.

Kubernetes Secret persistence is independent of the selected compute Node, but
it is not an off-cluster backup. Keep an approved escrow copy, especially of
`oidc_signing_key.pem`: it also protects access to encrypted user secrets.

The `sparkbox-durable` claim is a 100 GiB ReadWriteMany `shared-vast` volume.
The entrypoint creates `/mnt/sparkbox-durable/checkpoints` for checkpoint
objects. SQLite databases and live Firecracker disks remain under the
Node-local mount; do not move them onto VAST.

## Manual durable checkpoints

The CKS entrypoint enables a deliberately small manual checkpoint path:

```sh
ssh -p 22 ctl@ssh.<domain> checkpoint <sandbox>
ssh -p 22 ctl@ssh.<domain> checkpoint restore <sandbox>
```

Checkpointing pauses the guest, removes managed secret values from its disk,
checks and compacts the filesystem, compresses it, and copies a new immutable
object to VAST. A previously running guest then cold-boots. This creates more
downtime than the eventual pause-and-reflink design, but keeps the first
durable implementation synchronous and easy to reason about.

Only the latest committed checkpoint is exposed through the control command.
A failed upload leaves the previous pointer in place. Restore downloads and
decompresses beside the live rootfs, then atomically replaces it; it does not
consume the VAST object. The operation is owner-scoped through the existing
`ctl@` authorization path.

This is not yet automated Node-loss recovery. The latest-checkpoint pointer is
still in the Node-local control record, and this slice has no schedule,
retention policy, manifest index, or content digest. Keep the control directory
when replacing only the Pod. After loss of the whole Node, the immutable VAST
objects remain, but recovering their sandbox metadata is still an operator
procedure and a later milestone.

## Deploy

Check the target before applying anything:

```sh
kubectl config current-context
kubectl get nodes \
  -o custom-columns=NAME:.metadata.name,POOL:.metadata.labels.compute\\.coreweave\\.com/node-pool,TYPE:.metadata.labels.node\\.coreweave\\.cloud/type
```

Then deploy, selecting the NodePool and the public key that should be the first
Sparkbox operator:

```sh
deploy/kubernetes/deploy.sh \
  --image "$IMAGE" \
  --node-pool default-node-pool \
  --public-key ~/.ssh/id_ed25519.pub \
  --user vanpelt
```

When the NodePool has exactly one ready, schedulable amd64 Node, the script
selects it automatically. If the pool has more than one eligible Node, choose
the hot tier's owner explicitly:

```sh
deploy/kubernetes/deploy.sh \
  --image "$IMAGE" \
  --node-pool default-node-pool \
  --node g084f44 \
  --public-key ~/.ssh/id_ed25519.pub \
  --user vanpelt
```

The script resolves the exact Node, applies the namespace, verifies every
required entry in `sparkbox-identity`, and applies the `shared-vast` claim
before creating the LoadBalancer. It reads the Service's `ExternalRecords`
status condition, removes the leading `*.`, and passes the resulting base
domain to Sparkbox. It then creates `sparkbox-users`, renders the
image/domain/NodePool/exact-Node values into the Deployment, and waits up to 20
minutes for first boot. Sparkbox uses its `autocert` provider to issue per-host
Let's Encrypt certificates on first use. Port 80 serves the ACME challenge and
redirects ordinary requests to port 443.

The wildcard record does not represent the base name itself. Use any label,
such as `ssh`, for the SSH gateway. The Kubernetes entrypoint passes this
public hostname and the load balancer's public ports to Sparkbox, so the login
page and WebAuthn origin never advertise the Pod's internal names or ports:

```sh
DOMAIN=$(
  kubectl -n sparkbox-poc get service sparkbox \
    -o=jsonpath='{.status.conditions[?(@.type=="ExternalRecords")].message}' |
  sed 's/^\\*\\.//'
)

ssh -p 22 ctl@"ssh.$DOMAIN" help
ssh -p 22 new@"ssh.$DOMAIN"
```

The browser dashboard is:

```text
https://my.<domain>
```

New sandboxes receive two vCPUs and 8 GiB RAM by default. This POC admits at
most two running sandboxes for one owner and limits the Pod to eight CPUs,
24 GiB RAM, and 100 GiB Kubernetes-accounted ephemeral storage. The host path
is not quota-enforced by Kubernetes; monitor and clean
`/mnt/local/sparkbox-poc` as part of operating the Node.

## Observe and troubleshoot

```sh
kubectl -n sparkbox-poc get pods,service,pvc
kubectl -n sparkbox-poc logs -f deployment/sparkbox
kubectl -n sparkbox-poc describe pod -l app.kubernetes.io/name=sparkbox
```

The startup probe allows 20 minutes because the rootfs is large. Common hard
failures are:

- `/dev/kvm` or `/dev/net/tun` is unavailable to the privileged Pod.
- The selected Node is not ready, schedulable, amd64, and in the selected
  NodePool.
- `sparkbox-identity` is absent, incomplete, or contains invalid key material.
- The `sparkbox-durable` PVC cannot bind to `shared-vast`.
- GitHub release downloads are blocked.
- An agent CLI release endpoint is unavailable.
- The mounted local filesystem cannot perform reflink copies.
- The cluster or organization has no public LoadBalancer/IP quota.

The certificate cache, control database, and guest disks are on the same
Node-local path. They survive an ordinary Pod rollout on that Node and
disappear together on Node loss. Private identity instead comes from the
Kubernetes Secret, and the VAST mount provides a Node-independent checkpoint
target. The runtime node and cluster identity is fixed at `cks-poc`, rather
than inheriting the changing Kubernetes Pod name, so placement records remain
stable across rollouts. A Deployment pinned to a lost Node remains pending
until it is redeployed for a replacement Node. Automated checkpoint creation,
checkpoint discovery after control-state loss, and automatic recovery remain
work described in
[`cks-reflink-persistence-plan.md`](cks-reflink-persistence-plan.md).

## Remove the POC

Deleting the namespace removes the Service, public address, Pod, identity
Secret, and PVC claim:

```sh
kubectl delete namespace sparkbox-poc
```

Kubernetes does not remove the `hostPath`. Before releasing or repurposing the
Node, separately remove `/mnt/local/sparkbox-poc` through an authorized Node
maintenance workflow. A Node reboot also makes that encrypted local data
unrecoverable. The current `shared-vast` StorageClass uses a `Retain` reclaim
policy, so deleting the claim may leave its backing volume for separate
operator recovery or cleanup; verify that state before assuming either deletion
or recovery. Restore `sparkbox-identity` from escrow after namespace loss.
