# Local Headscale Setup

Run headscale + hiveqa on the same machine. Fully self-hosted — no Tailscale account needed, no cloud, no external dependencies. Great for air-gapped environments or when you want complete control.

**Trade-off:** No automatic HTTPS for stack URLs (headscale can't provision `*.ts.net` certs). Stacks are accessible over plain HTTP on the tailnet, or you bring your own TLS.

**Time to set up: ~15 minutes.**

## Prerequisites

- Go 1.24+ installed
- Docker + Docker Compose installed
- [headscale](https://github.com/juanfont/headscale) installed (see below)

## Step 1: Install headscale

### Option A: Download binary

```bash
# Check https://github.com/juanfont/headscale/releases for latest version
HEADSCALE_VERSION=0.28.0
ARCH=$(uname -m | sed 's/x86_64/amd64/;s/aarch64/arm64/')
curl -fsSL "https://github.com/juanfont/headscale/releases/download/v${HEADSCALE_VERSION}/headscale_${HEADSCALE_VERSION}_linux_${ARCH}" \
  -o ~/.local/bin/headscale
chmod +x ~/.local/bin/headscale
```

### Option B: Docker

```bash
docker pull headscale/headscale:0.28.0
```

We'll use the binary approach below. Adjust if using Docker.

## Step 2: Configure headscale

```bash
mkdir -p ~/.local/share/headscale
```

Create `~/.local/share/headscale/config.yaml`:

```yaml
server_url: "http://localhost:8080"
listen_addr: "0.0.0.0:8080"
metrics_listen_addr: "0.0.0.0:9090"
grpc_listen_addr: "0.0.0.0:50443"
grpc_allow_insecure: true

noise:
  private_key_path: /home/YOU/.local/share/headscale/noise_private.key

prefixes:
  v4: "100.64.0.0/10"
  v6: "fd7a:115c:a1e0::/48"
  allocation: sequential

database:
  type: sqlite
  sqlite:
    path: /home/YOU/.local/share/headscale/db.sqlite
    write_ahead_log: true

derp:
  server:
    enabled: true
    region_id: 999
    region_code: "local"
    region_name: "local embedded"
    stun_listen_addr: "0.0.0.0:3478"
    private_key_path: /home/YOU/.local/share/headscale/derp_private.key
    automatically_add_embedded_derp_region: true
  urls: []
  auto_update_enabled: false

dns:
  magic_dns: true
  base_domain: "hiveqa.local"
  nameservers:
    global:
      - 1.1.1.1

ephemeral_node_inactivity_timeout: 5m
```

Replace `/home/YOU` with your actual home directory.

## Step 3: Start headscale

```bash
headscale serve --config ~/.local/share/headscale/config.yaml
```

In another terminal, verify it's running:

```bash
headscale --config ~/.local/share/headscale/config.yaml nodes list
```

## Step 4: Create a user and auth key

```bash
# Create a user for hiveqa
headscale users create hiveqa

# Generate a reusable, ephemeral pre-auth key
headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 24h
```

Copy the key that's printed. It looks like `nodekey:xxxxx` or similar (format varies by headscale version).

## Step 5: Build and start hiveqa

```bash
cd tools/hiveqa
go build -o ~/.local/bin/hiveqad  ./cmd/hiveqad
go build -o ~/.local/bin/hiveqactl ./cmd/hiveqactl
```

Start the daemon pointing at your local headscale:

```bash
export TS_AUTHKEY="your-preauth-key-here"
hiveqad --control-url=http://localhost:8080 --tls=none
```

Key flags:
- `--control-url=http://localhost:8080` — points tsnet at headscale instead of Tailscale SaaS
- `--tls=none` — headscale can't provision certs, so we use plain HTTP on the tailnet

## Step 6: Use it

```bash
hiveqactl up --name my-app --compose ./docker-compose.yml
```

The stack is accessible at `http://my-app.hiveqa.local` from any device on the headscale tailnet. Since we're running everything locally, that's just your machine.

## Step 7 (optional): Connect other machines

To access stacks from other machines, install the Tailscale client and point it at your headscale:

```bash
# On the other machine
tailscale up --login-server=http://YOUR_HEADSCALE_IP:8080
```

Then approve the node:

```bash
# On the headscale machine
headscale nodes register --user hiveqa --key nodekey:XXXXX
```

Or use the pre-auth key:

```bash
tailscale up --login-server=http://YOUR_HEADSCALE_IP:8080 --authkey=your-preauth-key
```

## Adding TLS (optional)

If you need HTTPS on stack URLs, you have two options:

### Option A: ACME with DNS-01

If you own a domain and use Cloudflare/GCP/AWS for DNS:

```bash
export TS_AUTHKEY="your-preauth-key"
export CLOUDFLARE_API_TOKEN="your-token"
hiveqad \
  --control-url=http://localhost:8080 \
  --tls=acme \
  --acme-domain=dev.yourdomain.com \
  --acme-email=you@yourdomain.com \
  --acme-dns-provider=cloudflare
```

Stacks get certs for `{name}.dev.yourdomain.com`.

### Option B: Reverse proxy with Caddy

Run Caddy on the host with automatic HTTPS:

```
# Caddyfile
*.dev.localhost {
    tls internal
    reverse_proxy {labels.0}.hiveqa.local:80
}
```

## All-in-one with Docker Compose

If you prefer running headscale + hiveqad via Docker:

```yaml
services:
  headscale:
    image: headscale/headscale:0.28.0
    command: serve
    volumes:
      - headscale-data:/var/lib/headscale
      - ./headscale-config.yaml:/etc/headscale/config.yaml
    ports:
      - "8080:8080"
      - "3478:3478/udp"

  hiveqad:
    build: .
    environment:
      - TS_AUTHKEY=${TS_AUTHKEY}
      - HIVEQA_CONTROL_URL=http://headscale:8080
      - HIVEQA_TLS=none
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - hiveqa-data:/data/hiveqa
    depends_on:
      - headscale

volumes:
  headscale-data:
  hiveqa-data:
```

Or just use the pre-built all-in-one image:

```bash
docker build -t hiveqa .
docker run -d \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v hiveqa-data:/data \
  hiveqa
```

## Troubleshooting

### "tsnet up: ... connection refused"

Headscale isn't reachable. Check:
- Is headscale running? `curl http://localhost:8080/health`
- Is the `--control-url` correct?

### "tsnet up: ... auth key invalid"

Generate a new pre-auth key:
```bash
headscale preauthkeys create --user hiveqa --reusable --ephemeral --expiration 24h
```

### Nodes don't appear in headscale

```bash
headscale nodes list
```

If empty, the auth key might be wrong or expired. Check headscale logs.

### Can't resolve MagicDNS names

MagicDNS requires the Tailscale client to be configured as the DNS resolver. On Linux, this usually means `systemd-resolved` integration. For local-only use, you can skip MagicDNS and use container IPs directly.
