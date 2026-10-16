# Kubernetes Deployment

Deploy headscale + hiveqad as a single pod in Kubernetes. This is the recommended production deployment for teams or per-customer instances.

**What you get:**
- Headscale control plane with persistent state
- hiveqad managing compose stacks on the node
- cert-manager providing ACME TLS for the headscale endpoint
- Optional: certmagic DNS-01 for stack URLs

**Time to set up: ~30 minutes** (assuming an existing K8s cluster with cert-manager).

## Prerequisites

- Kubernetes cluster (GKE, EKS, AKS, k3s, etc.)
- `kubectl` configured
- [cert-manager](https://cert-manager.io/docs/installation/) installed (for automatic TLS)
- Docker socket available on nodes (for compose stack management)
- A domain pointing to your ingress controller

## Architecture

```
Internet → Ingress (TLS) → headscale:8080
                             │
                             ├── tsnet nodes ← hiveqad (same pod)
                             │                    │
                             │                    ├── compose stack A
                             │                    ├── compose stack B
                             │                    └── compose stack C
                             │
                             └── DERP relay (embedded)
```

## Step 1: Build and push the image

```bash
cd tools/hiveqa

# Build the all-in-one image
docker build -t gcr.io/YOUR_PROJECT/hiveqa:latest .

# Push to your registry
docker push gcr.io/YOUR_PROJECT/hiveqa:latest
```

## Step 2: Create the namespace and secrets

```bash
kubectl create namespace hiveqa

# Optional: provide a pre-generated auth key.
# If omitted, the entrypoint auto-generates one from headscale.
kubectl -n hiveqa create secret generic hiveqa-secrets \
  --from-literal=ts-authkey=""
```

## Step 3: Configure the deployment

Edit `deploy/k8s.yaml` and replace the placeholder values:

| Placeholder | Replace with |
|---|---|
| `gcr.io/PROJECT/hiveqa:latest` | Your image URL |
| `headscale.CUSTOMER.example.com` | Your headscale domain |
| `PROJECT:REGION:headscale` | (Only if using Cloud SQL — remove the annotation otherwise) |

### Key configuration decisions

**TLS for the headscale endpoint:**

The K8s Ingress with cert-manager handles this. The annotation `cert-manager.io/cluster-issuer: "letsencrypt-prod"` auto-provisions a cert for your domain.

If you don't have cert-manager, you can use any other method (manual cert, cloud load balancer TLS, etc.).

**TLS for stack URLs:**

By default, `HIVEQA_TLS=none` in the K8s manifest — stacks are HTTP-only on the tailnet. This is fine if you only access stacks from trusted devices on the tailnet.

For HTTPS stack URLs, set:

```yaml
env:
  - name: HIVEQA_TLS
    value: "acme"
  - name: HIVEQA_ACME_DOMAIN
    value: "stacks.yourdomain.com"
  - name: HIVEQA_ACME_EMAIL
    value: "admin@yourdomain.com"
  - name: HIVEQA_ACME_DNS_PROVIDER
    value: "cloudflare"  # or gcloud, route53
  - name: CLOUDFLARE_API_TOKEN
    valueFrom:
      secretKeyRef:
        name: hiveqa-secrets
        key: cloudflare-token
```

## Step 4: Set up cert-manager ClusterIssuer

If you don't already have a `letsencrypt-prod` ClusterIssuer:

```yaml
apiVersion: cert-manager.io/v1
kind: ClusterIssuer
metadata:
  name: letsencrypt-prod
spec:
  acme:
    server: https://acme-v02.api.letsencrypt.org/directory
    email: admin@yourdomain.com
    privateKeySecretRef:
      name: letsencrypt-prod-key
    solvers:
      - http01:
          ingress:
            class: nginx
```

```bash
kubectl apply -f cluster-issuer.yaml
```

## Step 5: Deploy

```bash
kubectl apply -f deploy/k8s.yaml
```

Watch the rollout:

```bash
kubectl -n hiveqa rollout status deployment/hiveqa
kubectl -n hiveqa logs -f deployment/hiveqa
```

You should see:

```
=== hiveqa single-deployment entrypoint ===
Starting headscale...
Headscale is ready.
Generated ephemeral auth key.
Starting hiveqad...
  Control URL: http://localhost:8080
  TLS mode:    none
  Auto-set TLS=none (headscale mode, use external TLS termination)
hiveqad starting (state: /data/hiveqa, control: http://localhost:8080, tls: none)
control socket listening on /data/hiveqa/hiveqad.sock
```

## Step 6: Verify

Check headscale is reachable:

```bash
curl -k https://headscale.yourdomain.com/health
```

Check hiveqad is running:

```bash
kubectl -n hiveqa exec deployment/hiveqa -- hiveqactl list
```

## Step 7: Use it

From inside the pod (or via `kubectl exec`):

```bash
kubectl -n hiveqa exec deployment/hiveqa -- \
  hiveqactl up --name my-app --compose /path/to/docker-compose.yml
```

For remote CLI access, you can port-forward the control socket:

```bash
# On your machine
kubectl -n hiveqa port-forward deployment/hiveqa 9999:8080 &

# Then use hiveqactl with a TCP control endpoint (future feature)
# For now, kubectl exec is the simplest path
```

## Per-customer deployment

To deploy one instance per customer, template the K8s manifest:

```bash
# Using envsubst, helm, kustomize, or any templating tool
export CUSTOMER=acme-corp
export DOMAIN=acme-corp.hiveqa.dev
export IMAGE=gcr.io/your-project/hiveqa:latest

envsubst < deploy/k8s.yaml | kubectl apply -f -
```

Each customer gets:
- Their own namespace (`hiveqa-acme-corp`)
- Their own headscale instance
- Their own PVC for state
- Their own Ingress with TLS cert

## Storage considerations

**SQLite (default):** Fine for single-replica deployments. State lives on the PVC.

**Postgres:** Required if you need HA or run on platforms without persistent disks. Edit the headscale config in the ConfigMap:

```yaml
database:
  type: postgres
  postgres:
    host: "your-postgres-host"
    port: 5432
    name: "headscale"
    user: "headscale"
    pass: "secret"
    ssl: true
```

See `deploy/cloudrun/headscale-postgres.yaml` for a complete Postgres config.

## Monitoring

Headscale exposes Prometheus metrics on `:9090`. The K8s Service already exposes this port.

Add a ServiceMonitor (if using Prometheus Operator):

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: hiveqa
  namespace: hiveqa
spec:
  selector:
    matchLabels:
      app: hiveqa
  endpoints:
    - port: metrics
      interval: 30s
```

## Troubleshooting

### Pod keeps restarting

Check logs: `kubectl -n hiveqa logs deployment/hiveqa --previous`

Common causes:
- PVC not bound (check `kubectl -n hiveqa get pvc`)
- Docker socket not available (check node has Docker and the hostPath mount works)

### Ingress doesn't get a TLS cert

- Check cert-manager logs: `kubectl -n cert-manager logs deployment/cert-manager`
- Check the Certificate resource: `kubectl -n hiveqa describe certificate hiveqa-tls`
- Make sure DNS for your domain points to the ingress controller's IP

### headscale health check fails

The `/health` endpoint may not exist in all headscale versions. If the liveness probe fails, try changing it to a TCP probe:

```yaml
livenessProbe:
  tcpSocket:
    port: 8080
```

### "Recreate" strategy causes downtime

The `Recreate` strategy is required because headscale's SQLite database doesn't support concurrent access. The PVC must be unmounted from the old pod before the new one starts. Downtime is typically <30 seconds.

For zero-downtime upgrades, switch to Postgres.
