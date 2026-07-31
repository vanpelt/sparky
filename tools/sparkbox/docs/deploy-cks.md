# Sparkbox on CoreWeave Kubernetes Service

This is a deliberately small proof of concept: one privileged Sparkbox Pod on
one CKS bare-metal CPU Node, with Firecracker guests nested under the Pod. A
public CKS LoadBalancer provides SSH and wildcard HTTP routing under
`coreweave.app`.

## What it creates

- Namespace `sparkbox-poc`.
- One `Deployment`, pinned to an amd64 CKS NodePool.
- A 100 GiB local `emptyDir` for the rootfs template and sandbox state.
- Host device mounts for `/dev/kvm` and `/dev/net/tun`.
- A public `LoadBalancer` Service with a `*.coreweave.app` record and ports 80,
  443, and 22.
- A Secret containing one operator's **public** SSH key.

The Pod is privileged, but it does not use `hostNetwork`. TAP devices, sysctls,
NAT, and packet-filter rules therefore live in the Pod's network namespace
instead of modifying the CKS Node's Calico network namespace.

State is intentionally ephemeral. Deleting or rescheduling the Pod deletes all
sandboxes. Do not treat this manifest as a production deployment.

The release rootfs is also deliberately left bare in this first POC. The host
installer normally patches `claude`, `codex`, `pi`, and `hivemind` into that
template after downloading it. The Kubernetes bootstrap does not yet perform
that loop-mount patch, so this proves the Firecracker runtime, SSH gateway,
workload-identity metadata, and HTTP proxy rather than the complete agent
toolchain.

## Runtime image from GitHub Actions

`.github/workflows/sparkbox-cks-image.yml` builds this Containerfile for
`linux/amd64` and publishes it to GitHub Container Registry on every push that
changes Sparkbox or the workflow. It needs no registry secret: the workflow's
`GITHUB_TOKEN` receives `packages: write`.

Every build receives an immutable full-SHA tag and a branch tag. The default
branch also updates `edge`:

```sh
IMAGE=ghcr.io/vanpelt/sparkbox-cks:edge
```

GHCR packages are private until their visibility is changed. After the first
workflow run, open the `sparkbox-cks` package settings on GitHub and change its
visibility to public so CKS can pull it without an image-pull secret. The image
contains the current Sparkbox binary and host networking tools. At Pod startup
it downloads and SHA-256-verifies the Firecracker, kernel, and universal rootfs
artifacts pinned to Sparkbox `v0.5.3`. The first start downloads about 750 MB
and decompresses a sparse ext4 image.

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

The script creates the LoadBalancer first, reads its `ExternalRecords` status
condition, removes the leading `*.`, and passes the resulting base domain to
Sparkbox. It then creates `sparkbox-users`, renders the image/domain/NodePool
values into the Deployment, and waits up to 20 minutes for first boot. Sparkbox
uses its `autocert` provider to issue per-host Let's Encrypt certificates on
first use. Port 80 serves the ACME challenge and redirects ordinary requests to
port 443.

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
24 GiB RAM, and 100 GiB ephemeral storage.

## Observe and troubleshoot

```sh
kubectl -n sparkbox-poc get pods,service
kubectl -n sparkbox-poc logs -f deployment/sparkbox
kubectl -n sparkbox-poc describe pod -l app.kubernetes.io/name=sparkbox
```

The startup probe allows 20 minutes because the rootfs is large. Common hard
failures are:

- `/dev/kvm` or `/dev/net/tun` is unavailable to the privileged Pod.
- The selected NodePool label does not exist.
- GitHub release downloads are blocked.
- The mounted local filesystem cannot perform reflink copies.
- The cluster or organization has no public LoadBalancer/IP quota.

The certificate cache is on the same ephemeral volume as the rest of the POC.
Deleting the Pod therefore causes certificate reissuance and can eventually
hit Let's Encrypt rate limits. Use persistent certificate storage or
cert-manager before treating this as a durable endpoint.

## Remove the POC

Deleting the namespace removes the Service, public address, Pod, Secret, and all
ephemeral sandbox data:

```sh
kubectl delete namespace sparkbox-poc
```
