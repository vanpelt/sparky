# Cloud Run + VM Deployment

Split architecture: headscale control plane on Cloud Run (serverless, managed TLS), hiveqad on a GCE VM (Docker-capable, runs compose stacks).

**Best for:** Teams that want managed infrastructure for the control plane with minimal ops, while running dev environments on beefy VMs.

**What you get:**
- Headscale on Cloud Run with automatic TLS and Cloud SQL Postgres
- hiveqad on a GCE VM managing compose stacks
- ACME DNS-01 via Google Cloud DNS for stack HTTPS URLs
- Per-customer isolation (one Cloud Run service + VM per customer)

**Time to set up: ~45 minutes.**

## Architecture

```
┌─ Cloud Run ─────────────────────┐    ┌─ GCE VM ──────────────────────────┐
│                                  │    │                                    │
│  headscale (control plane)       │◄───│  hiveqad                           │
│  Cloud SQL Postgres              │    │    ├── tsnet node: feature-auth     │
│  Managed TLS (*.run.app)         │    │    ├── tsnet node: feature-pay      │
│  Public DERP relays              │    │    └── tsnet node: staging          │
│                                  │    │                                    │
└──────────────────────────────────┘    │  Docker                            │
                                        │    ├── hiveqa-feature-auth stack    │
                                        │    ├── hiveqa-feature-pay stack     │
                                        │    └── hiveqa-staging stack         │
                                        │                                    │
                                        │  certmagic (DNS-01 via Cloud DNS)  │
                                        │    → *.customer.hiveqa.dev certs   │
                                        └────────────────────────────────────┘
```

## Prerequisites

- GCP project with billing enabled
- `gcloud` CLI installed and authenticated
- A domain managed by Google Cloud DNS (for ACME DNS-01)
- Docker installed on the VM

## Part 1: Cloud Run (headscale control plane)

### Step 1: Create Cloud SQL instance

```bash
PROJECT_ID=$(gcloud config get-value project)
REGION=us-central1

# Create Postgres instance (smallest tier for dev)
gcloud sql instances create headscale \
  --database-version=POSTGRES_15 \
  --tier=db-f1-micro \
  --region=$REGION \
  --storage-size=10GB

# Create database and user
gcloud sql databases create headscale --instance=headscale

DB_PASSWORD=$(openssl rand -base64 24)
gcloud sql users create headscale \
  --instance=headscale \
  --password="$DB_PASSWORD"

echo "Save this password: $DB_PASSWORD"
```

### Step 2: Store secrets

```bash
# Database password
echo -n "$DB_PASSWORD" | gcloud secrets create headscale-db-pass --data-file=-

# Headscale will generate its noise key on first start,
# but for Cloud Run (stateless) we need to persist it.
# Generate one and store it:
openssl rand -hex 32 | gcloud secrets create headscale-noise-key --data-file=-
```

Grant the default Cloud Run service account access:

```bash
SA="${PROJECT_ID}@appspot.gserviceaccount.com"

gcloud secrets add-iam-policy-binding headscale-db-pass \
  --member="serviceAccount:$SA" --role="roles/secretmanager.secretAccessor"

gcloud secrets add-iam-policy-binding headscale-noise-key \
  --member="serviceAccount:$SA" --role="roles/secretmanager.secretAccessor"

# Cloud SQL access
gcloud projects add-iam-policy-binding $PROJECT_ID \
  --member="serviceAccount:$SA" --role="roles/cloudsql.client"
```

### Step 3: Build and push the headscale image

```bash
cd tools/hiveqa

docker build \
  -f deploy/cloudrun/Dockerfile.headscale \
  -t gcr.io/$PROJECT_ID/headscale:latest \
  deploy/cloudrun/

docker push gcr.io/$PROJECT_ID/headscale:latest
```

### Step 4: Deploy to Cloud Run

Edit `deploy/cloudrun/service.yaml`:

| Placeholder | Replace with |
|---|---|
| `PROJECT` | Your GCP project ID |
| `REGION` | Your region (e.g., `us-central1`) |
| `CUSTOMER` | Customer identifier |

```bash
# Replace placeholders
sed -e "s/PROJECT/$PROJECT_ID/g" \
    -e "s/REGION/$REGION/g" \
    -e "s/CUSTOMER/myteam/g" \
    deploy/cloudrun/service.yaml > /tmp/service.yaml

gcloud run services replace /tmp/service.yaml --region=$REGION
```

Get the URL:

```bash
HEADSCALE_URL=$(gcloud run services describe headscale \
  --region=$REGION --format='value(status.url)')
echo "Headscale URL: $HEADSCALE_URL"
```

### Step 5: Create a headscale user and auth key

Cloud Run doesn't expose gRPC easily, so we'll use the headscale container locally to generate the key:

```bash
# Run headscale locally with the same Postgres config to generate keys
docker run --rm -it \
  -e HEADSCALE_DB_HOST="$CLOUD_SQL_IP" \
  -e HEADSCALE_DB_NAME=headscale \
  -e HEADSCALE_DB_USER=headscale \
  -e HEADSCALE_DB_PASS="$DB_PASSWORD" \
  gcr.io/$PROJECT_ID/headscale:latest \
  headscale users create hiveqa

docker run --rm -it \
  -e HEADSCALE_DB_HOST="$CLOUD_SQL_IP" \
  -e HEADSCALE_DB_NAME=headscale \
  -e HEADSCALE_DB_USER=headscale \
  -e HEADSCALE_DB_PASS="$DB_PASSWORD" \
  gcr.io/$PROJECT_ID/headscale:latest \
  headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 720h

# Save the printed auth key
```

Alternatively, use Cloud SQL Auth Proxy locally to connect.

## Part 2: GCE VM (hiveqad)

### Step 6: Create the VM

```bash
gcloud compute instances create hiveqa-runner \
  --zone=${REGION}-a \
  --machine-type=e2-standard-4 \
  --image-project=cos-cloud \
  --image-family=cos-stable \
  --boot-disk-size=100GB \
  --scopes=cloud-platform
```

If using Container-Optimized OS (COS), Docker is pre-installed. Otherwise, install Docker on your preferred OS.

### Step 7: Install hiveqa on the VM

```bash
gcloud compute ssh hiveqa-runner --zone=${REGION}-a

# On the VM:
# Install Go
curl -fsSL https://go.dev/dl/go1.24.2.linux-amd64.tar.gz | sudo tar -C /usr/local -xz
export PATH=$PATH:/usr/local/go/bin

# Clone and build
git clone https://github.com/vanpelt/sparky.git
cd sparky/tools/hiveqa
go build -o /usr/local/bin/hiveqad  ./cmd/hiveqad
go build -o /usr/local/bin/hiveqactl ./cmd/hiveqactl
```

### Step 8: Set up Google Cloud DNS for ACME

If you don't already have a Cloud DNS zone for your domain:

```bash
gcloud dns managed-zones create hiveqa-dev \
  --dns-name="hiveqa.dev." \
  --description="hiveqa stack domains"
```

Note the nameservers and update your registrar:

```bash
gcloud dns managed-zones describe hiveqa-dev --format='value(nameServers)'
```

### Step 9: Start hiveqad

```bash
export TS_AUTHKEY="your-headscale-preauth-key"
export GCP_PROJECT="$PROJECT_ID"

hiveqad \
  --control-url="$HEADSCALE_URL" \
  --tls=acme \
  --acme-domain=hiveqa.dev \
  --acme-email=admin@hiveqa.dev \
  --acme-dns-provider=gcloud
```

### Step 10: Create a systemd service (production)

```bash
sudo tee /etc/systemd/system/hiveqad.service << 'EOF'
[Unit]
Description=hiveqa daemon
After=docker.service
Requires=docker.service

[Service]
Type=simple
Environment=TS_AUTHKEY=your-key-here
Environment=GCP_PROJECT=your-project
ExecStart=/usr/local/bin/hiveqad \
  --control-url=https://headscale-myteam.run.app \
  --tls=acme \
  --acme-domain=hiveqa.dev \
  --acme-email=admin@hiveqa.dev \
  --acme-dns-provider=gcloud
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF

sudo systemctl daemon-reload
sudo systemctl enable --now hiveqad
```

### Step 11: Use it

```bash
hiveqactl up --name feature-auth --compose ./docker-compose.yml
```

Stack is now at `https://feature-auth.hiveqa.dev` with a valid Let's Encrypt cert.

## DNS record automation

When a stack comes up, you need a DNS record pointing `feature-auth.hiveqa.dev` to the VM's tailnet IP (or public IP if using Funnel). Currently this is manual. Options:

1. **Wildcard DNS** — create `*.hiveqa.dev → VM_IP` once, all stacks work automatically
2. **Per-stack records** — future hiveqa feature to call Cloud DNS API on stack up/down
3. **Use tailnet MagicDNS only** — skip public DNS, access stacks via `feature-auth.hiveqa.local` on the tailnet

For most dev use cases, a wildcard A record is simplest:

```bash
VM_IP=$(gcloud compute instances describe hiveqa-runner \
  --zone=${REGION}-a --format='value(networkInterfaces[0].accessConfigs[0].natIP)')

gcloud dns record-sets create "*.hiveqa.dev." \
  --zone=hiveqa-dev \
  --type=A \
  --ttl=300 \
  --rrdatas="$VM_IP"
```

## Cost estimate

| Resource | Monthly cost (approx) |
|---|---|
| Cloud Run (1 instance, always-on) | ~$15 |
| Cloud SQL (db-f1-micro) | ~$10 |
| GCE VM (e2-standard-4) | ~$100 |
| Cloud DNS zone | ~$0.50 |
| **Total** | **~$125/month** |

Scale the VM up/down based on how many compose stacks you need to run concurrently.

## Troubleshooting

### hiveqad can't connect to headscale on Cloud Run

- Check the URL: `curl -I $HEADSCALE_URL/health`
- Cloud Run may require authentication. If so, add `--ingress=all` and `--allow-unauthenticated` or configure IAM:
  ```bash
  gcloud run services add-iam-policy-binding headscale \
    --region=$REGION \
    --member="allUsers" \
    --role="roles/run.invoker"
  ```

### ACME DNS-01 challenge fails

- Check GCP_PROJECT env var is set
- Check the service account has `dns.admin` role on the Cloud DNS zone
- Check the domain's NS records point to Cloud DNS nameservers
- Check certmagic logs in hiveqad output

### Cloud Run instance keeps restarting

- Check logs: `gcloud run services logs read headscale --region=$REGION`
- Make sure Cloud SQL connection string is correct (use `/cloudsql/project:region:instance` format)
- Make sure secrets are accessible
