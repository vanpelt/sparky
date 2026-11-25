# Fly.io Deployment

Run headscale on [Fly.io](https://fly.io) — the cheapest managed option that actually works with headscale. Fly provides persistent volumes, full TCP/UDP support, and automatic TLS. No custom Upgrade header issues (unlike Cloudflare).

hiveqad runs separately on a Docker-capable VM, pointed at your Fly-hosted headscale.

**Cost: ~$3-5/month** for a shared-cpu-1x VM with 1GB persistent volume.

**Time to set up: ~20 minutes.**

## Architecture

```
┌─ Fly.io ────────────────────────┐    ┌─ Your VM ─────────────────────────┐
│                                  │    │                                    │
│  headscale (control plane)       │◄───│  hiveqad                           │
│  SQLite on persistent volume     │    │    ├── tsnet node: feature-auth     │
│  Embedded DERP + STUN (UDP)      │    │    ├── tsnet node: feature-pay      │
│  TLS via Fly proxy (automatic)   │    │    └── tsnet node: staging          │
│                                  │    │                                    │
└──────────────────────────────────┘    │  Docker compose stacks...          │
                                        └────────────────────────────────────┘
```

## Why Fly.io works (and Cloudflare doesn't)

| Requirement | Fly.io | Cloudflare |
|---|---|---|
| Persistent disk | Volumes (survives restarts) | Ephemeral only |
| UDP (STUN/DERP) | Full UDP support | No UDP |
| Custom Upgrade headers | Passes through as-is | Strips non-websocket Upgrade |
| Long-running process | Always-on VMs | May sleep/restart anytime |
| TLS | Automatic via Fly proxy | N/A (can't run headscale) |

## Prerequisites

- [Fly CLI](https://fly.io/docs/flyctl/install/) installed: `curl -L https://fly.io/install.sh | sh`
- Fly.io account: `fly auth signup`
- A Docker-capable VM for hiveqad (local machine, GCE, Hetzner, etc.)

## Step 1: Create the Fly app

```bash
cd tools/hiveqa/deploy/fly

# Create the app (don't deploy yet)
fly apps create hiveqa-headscale
```

Pick a region close to your VM. List regions with `fly platform regions`.

## Step 2: Create a persistent volume

```bash
fly volumes create headscale_data --app hiveqa-headscale --size 1 --region ord
```

1 GB is more than enough for headscale state. Adjust `--region` to match your app.

## Step 3: Set the server URL

```bash
fly secrets set HEADSCALE_SERVER_URL=https://hiveqa-headscale.fly.dev --app hiveqa-headscale
```

Replace `hiveqa-headscale` with your actual app name.

## Step 4: Deploy

```bash
fly deploy --app hiveqa-headscale
```

Watch it come up:

```bash
fly logs --app hiveqa-headscale
```

You should see:

```
=== headscale on Fly.io ===
server_url set to https://hiveqa-headscale.fly.dev
Starting headscale...
```

Verify it's healthy:

```bash
curl https://hiveqa-headscale.fly.dev/health
```

## Step 5: Create a user and auth key

SSH into the Fly machine:

```bash
fly ssh console --app hiveqa-headscale
```

Inside the console:

```bash
# Create a user for hiveqa
headscale users create hiveqa

# Generate a reusable, ephemeral auth key (30 days)
headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 720h
```

Copy the printed auth key. Exit the console.

## Step 6: Configure hiveqad on your VM

### Install hiveqa

```bash
cd tools/hiveqa
go build -o ~/.local/bin/hiveqad  ./cmd/hiveqad
go build -o ~/.local/bin/hiveqactl ./cmd/hiveqactl
```

### Start with headscale pointing at Fly

```bash
export TS_AUTHKEY="your-preauth-key-from-step-5"

hiveqad \
  --control-url=https://hiveqa-headscale.fly.dev \
  --tls=none
```

Stacks are accessible over HTTP on the tailnet. For HTTPS stack URLs, add ACME:

```bash
export TS_AUTHKEY="your-preauth-key"
export CLOUDFLARE_API_TOKEN="your-token"  # or GCP_PROJECT for gcloud

hiveqad \
  --control-url=https://hiveqa-headscale.fly.dev \
  --tls=acme \
  --acme-domain=dev.yourdomain.com \
  --acme-email=you@yourdomain.com \
  --acme-dns-provider=cloudflare
```

## Step 7: Use it

```bash
hiveqactl up --name my-app --compose ./docker-compose.yml
```

If using `--tls=none`: accessible at `http://my-app.hiveqa.internal` on the tailnet.
If using `--tls=acme`: accessible at `https://my-app.dev.yourdomain.com`.

## Custom domain for headscale (optional)

Instead of `hiveqa-headscale.fly.dev`, use your own domain:

```bash
fly certs add headscale.yourdomain.com --app hiveqa-headscale
```

Then add a CNAME record: `headscale.yourdomain.com → hiveqa-headscale.fly.dev`

Fly provisions a TLS cert automatically.

Update the server URL:

```bash
fly secrets set HEADSCALE_SERVER_URL=https://headscale.yourdomain.com --app hiveqa-headscale
fly deploy --app hiveqa-headscale
```

## Per-customer deployment

Each customer gets their own Fly app:

```bash
# Template for customer "acme-corp"
fly apps create hiveqa-acme-corp
fly volumes create headscale_data --app hiveqa-acme-corp --size 1 --region ord
fly secrets set HEADSCALE_SERVER_URL=https://hiveqa-acme-corp.fly.dev --app hiveqa-acme-corp
fly deploy --app hiveqa-acme-corp
```

Automate with a script:

```bash
#!/bin/bash
CUSTOMER=$1
REGION=${2:-ord}

fly apps create "hiveqa-${CUSTOMER}"
fly volumes create headscale_data --app "hiveqa-${CUSTOMER}" --size 1 --region "$REGION"
fly secrets set "HEADSCALE_SERVER_URL=https://hiveqa-${CUSTOMER}.fly.dev" --app "hiveqa-${CUSTOMER}"
fly deploy --app "hiveqa-${CUSTOMER}"

echo "Headscale for ${CUSTOMER}: https://hiveqa-${CUSTOMER}.fly.dev"
echo "Now SSH in and create auth keys:"
echo "  fly ssh console --app hiveqa-${CUSTOMER}"
echo "  headscale users create hiveqa"
echo "  headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 720h"
```

## Monitoring

Fly provides built-in metrics. For headscale-specific metrics:

```bash
# Prometheus metrics endpoint (internal only)
fly ssh console --app hiveqa-headscale
curl localhost:9090/metrics
```

To expose metrics externally, add a Grafana Cloud or Prometheus remote-write integration via Fly's built-in metrics export.

## Cost breakdown

| Resource | Cost |
|---|---|
| shared-cpu-1x VM (256MB, always-on) | ~$1.94/month |
| 1 GB persistent volume | ~$0.15/month |
| Outbound bandwidth (1GB included) | $0.02/GB after |
| **Total** | **~$3-5/month** |

This is 5-10x cheaper than Cloud Run + Cloud SQL for the same functionality.

## Scaling up

If you need more capacity (many concurrent tsnet connections):

```bash
# Edit fly.toml
[vm]
  size = "shared-cpu-2x"
  memory = 512
```

Then `fly deploy`.

## Troubleshooting

### "connection refused" from hiveqad

- Check headscale is running: `curl https://hiveqa-headscale.fly.dev/health`
- Check Fly logs: `fly logs --app hiveqa-headscale`
- Make sure the volume is mounted: `fly ssh console` then `ls /data/headscale/`

### Auth key doesn't work

SSH in and generate a new one:
```bash
fly ssh console --app hiveqa-headscale
headscale preauthkeys list --user hiveqa
headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 720h
```

### Volume data lost after deploy

Make sure the volume name in `fly.toml` matches the created volume:
```bash
fly volumes list --app hiveqa-headscale
```

The `source` in `[[mounts]]` must match the volume name exactly.

### Machine keeps restarting

Check logs for OOM or crash:
```bash
fly logs --app hiveqa-headscale
fly machine status --app hiveqa-headscale
```

256MB should be plenty for headscale with <100 nodes. If you see OOM, bump to 512MB.

### DERP/STUN not working

Verify UDP port 3478 is exposed in `fly.toml` under `[[services]]` with `protocol = "udp"`. Check with:
```bash
# From your VM
nc -u hiveqa-headscale.fly.dev 3478
```
