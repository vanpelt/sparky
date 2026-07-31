# Sparkbox on CoreWeave Kubernetes Service

This proof of concept splits Sparkbox across two trust domains: a non-root
public gateway and one capability-scoped Firecracker node pinned to a CKS
bare-metal CPU Node. A public CKS LoadBalancer provides SSH and wildcard HTTPS
routing under `coreweave.app`; the VM node has no public Service.

## What it creates

- Namespace `sparkbox-poc`.
- `sparkbox-gateway`, an unprivileged Deployment with no host devices,
  hostPath, Linux capabilities, or writable root filesystem.
- `sparkbox-node`, a non-privileged, capability-scoped Deployment pinned to one
  exact amd64 Node.
- `sparkbox-device-plugin`, a capability-free DaemonSet that advertises one
  `sparkbox.dev/kvm`, `sparkbox.dev/tun`, and temporary `sparkbox.dev/loop`
  allocation per eligible Node.
- A Node-local XFS `hostPath` at `/mnt/local/sparkbox-poc` for the VM inventory,
  rootfs template, live sandbox disks, and node identity.
- A 100 GiB `shared-vast` PVC mounted at `/mnt/sparkbox-durable` for durable
  gateway databases, edge certificate cache, and checkpoint objects.
- Kubelet-managed device allocations for `/dev/kvm`, `/dev/net/tun`, and the
  loop-device bundle used by the remaining guest-disk mount paths. The VM node
  has no raw device `hostPath` volumes.
- A public `LoadBalancer` Service selecting only the gateway on ports 443 and
  22, plus an internal ClusterIP Service for the authenticated fleet link.
- Default-deny ingress, with only the gateway's SSH/fleet and HTTPS ports
  admitted. The node has no admitted ingress.
- A Secret containing one operator's **public** SSH key.
- A separately provisioned `sparkbox-identity` Secret containing the stable
  gateway, OIDC, and node-control identity, mounted only by the gateway. The
  node receives a separate Secret containing only the gateway's public host-key
  pin.

The VM node runs as root but with `privileged: false`. It drops the default
capability set and receives only the capabilities currently required for the
jailer, TAP/network setup, loop mounts, file ownership, and terminating
per-VM UIDs. `CAP_SYS_ADMIN` and an unconfined outer seccomp/AppArmor profile
remain significant privileges while jail construction and guest-disk mounts
stay in this process; this is an intermediate boundary, not the final runtime
shim. The Pod does not use host PID, network, IPC, or user namespaces. TAP
devices, sysctls, NAT, and packet-filter rules therefore live in the Pod's
network namespace instead of modifying the CKS Node's Cilium network
namespace.

The named host path survives deletion and replacement of the node Pod on the
same Node. It is still an ephemeral hot tier: CoreWeave local storage is lost
when the Node is replaced. The gateway database, identity Secret, and VAST
objects survive that loss; VM inventory and guest disks do not. Do not treat
this manifest as a production deployment.

On the first split rollout, `deploy.sh` stops the combined Pod, copies its
SQLite databases and edge caches to VAST, and leaves `sandboxes.json` and VM
files on the pinned Node. Before the VM node starts, its init container
removes the retired gateway databases, edge TLS cache, and fleet private keys
from the hostPath. The identity Secret is a required precondition and remains
the recovery source for those keys.

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
Gateway SQLite databases live under `/mnt/sparkbox-durable/gateway/control` and
checkpoint objects under `/mnt/sparkbox-durable/checkpoints`. Live Firecracker
disks and `sandboxes.json` remain on Node-local XFS; do not move them onto VAST.

## Manual durable checkpoints

The one-node checkpoint command is temporarily unavailable in the split
deployment: the gateway no longer has the VM disk, and the VM node deliberately
does not mount the durable control volume. Existing immutable checkpoint
objects are retained. Restoring this feature requires a fleet checkpoint RPC
or a narrow transfer helper; remounting the gateway PVC into the VM
node would undo the secret/state separation.

The original combined-Pod behavior was:

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

The script resolves the exact Node, verifies `sparkbox-identity`, derives a
public-only gateway host-key pin, and reads the LoadBalancer's `ExternalRecords`
domain. It stops the legacy Deployment, runs the idempotent state-migration
Job, starts the device-plugin DaemonSet and waits for all three extended
resources, starts both role-specific Deployments, and uses the operator key to
approve the node's authenticated fleet enrollment. The public gateway is then
selected by the existing LoadBalancer and the ingress policies are applied.
Sparkbox's `autocert` provider obtains certificates on the HTTPS listener.

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
kubectl -n sparkbox-poc get pods,daemonset,service,pvc,networkpolicy
kubectl get node -o custom-columns='NAME:.metadata.name,KVM:.status.allocatable.sparkbox\.dev/kvm,TUN:.status.allocatable.sparkbox\.dev/tun,LOOP:.status.allocatable.sparkbox\.dev/loop'
kubectl -n sparkbox-poc logs daemonset/sparkbox-device-plugin
kubectl -n sparkbox-poc logs -f deployment/sparkbox-gateway
kubectl -n sparkbox-poc logs -f deployment/sparkbox-node
kubectl -n sparkbox-poc describe pod -l app.kubernetes.io/name=sparkbox
```

The startup probe allows 20 minutes because the rootfs is large. Common hard
failures are:

- The device plugin cannot see `/dev/kvm`, `/dev/net/tun`, `/dev/loop-control`,
  or the eight loop block devices on the selected Node.
- `sparkbox.dev/kvm`, `sparkbox.dev/tun`, or `sparkbox.dev/loop` does not become
  allocatable before the VM-node rollout.
- The selected Node is not ready, schedulable, amd64, and in the selected
  NodePool.
- `sparkbox-identity` is absent, incomplete, or contains invalid key material.
- The `sparkbox-durable` PVC cannot bind to `shared-vast`.
- GitHub release downloads are blocked.
- An agent CLI release endpoint is unavailable.
- The mounted local filesystem cannot perform reflink copies.
- The cluster or organization has no public LoadBalancer/IP quota.

The certificate cache and control databases are on VAST; guest disks and VM
inventory remain Node-local. Private identity comes from the gateway-only
Kubernetes Secret. The VM node identity is fixed at `cks-poc`, rather than
inheriting the changing Kubernetes Pod name, so placement records remain stable
across rollouts. A node Deployment pinned to a lost Node remains pending until
it is redeployed for a replacement Node. Automated checkpoint creation,
checkpoint discovery, and automatic recovery remain work described in
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
