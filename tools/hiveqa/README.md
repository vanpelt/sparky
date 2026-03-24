# hiveqa

Run the same `docker-compose.yml` multiple times on one machine — each copy gets its own network identity with unique IP, DNS hostname, and automatic HTTPS.

Built for parallel dev environments, especially Claude agents working in git worktrees on the same VM.

## How it works

```
hiveqactl up --name feature-auth --compose ./docker-compose.yml
# → https://feature-auth.your-tailnet.ts.net  (valid TLS, no port conflicts)

hiveqactl up --name feature-pay --compose ./docker-compose.yml
# → https://feature-pay.your-tailnet.ts.net   (same compose file, fully isolated)
```

For each stack, hiveqa:
1. **Rewrites the compose file** — strips `ports:` bindings (no conflicts), namespaces volumes
2. **Runs `docker compose up`** with a unique project name
3. **Starts a [tsnet](https://pkg.go.dev/tailscale.com/tsnet) node** that reverse-proxies HTTPS traffic to the stack's containers
4. Each stack is reachable at its own URL with a valid TLS certificate

## Architecture

```
tools/hiveqa/
├── cmd/
│   ├── hiveqad/         # daemon — manages stacks + tsnet instances
│   └── hiveqactl/       # CLI — up, down, list
├── internal/
│   ├── api/             # shared types (daemon ↔ CLI)
│   ├── certs/           # ACME cert manager (certmagic + DNS-01 providers)
│   ├── compose/         # docker-compose YAML rewriting
│   ├── config/          # daemon configuration (flags + env vars)
│   ├── daemon/          # orchestrator (compose lifecycle + proxy lifecycle)
│   └── proxy/           # tsnet.Server + reverse proxy per stack
├── deploy/
│   ├── Dockerfile       # all-in-one: hiveqad + headscale
│   ├── entrypoint.sh
│   ├── headscale.yaml   # headscale config (SQLite)
│   ├── k8s.yaml         # Kubernetes manifests
│   ├── cloudrun/        # Cloud Run headscale-only deployment
│   │   ├── Dockerfile.headscale
│   │   ├── headscale-postgres.yaml
│   │   ├── service.yaml
│   │   └── entrypoint.sh
│   └── fly/             # Fly.io headscale deployment
│       ├── Dockerfile.headscale
│       ├── fly.toml
│       ├── headscale.yaml
│       └── entrypoint.sh
└── docs/                # deployment guides
```

## Deployment options

| Environment | Control plane | TLS for stacks | Guide |
|---|---|---|---|
| **Local dev** | Tailscale SaaS | Automatic (`*.ts.net`) | [Quickstart](docs/local-dev-quickstart.md) |
| **Local self-hosted** | headscale on localhost | None or ACME DNS-01 | [Local headscale](docs/local-headscale.md) |
| **Kubernetes** | headscale in pod | cert-manager or ACME DNS-01 | [K8s deployment](docs/deploy-kubernetes.md) |
| **Cloud Run + VM** | headscale on Cloud Run | ACME DNS-01 (certmagic) | [Cloud Run](docs/deploy-cloud-run.md) |
| **Fly.io + VM** | headscale on Fly.io | ACME DNS-01 (certmagic) | [Fly.io](docs/deploy-fly.md) |

### Quick decision guide

- **Just want it to work?** → [Local dev quickstart](docs/local-dev-quickstart.md) (5 min)
- **No Tailscale account / air-gapped?** → [Local headscale](docs/local-headscale.md) (15 min)
- **Production / per-customer?** → [Kubernetes](docs/deploy-kubernetes.md) (30 min)
- **Cheapest managed control plane?** → [Fly.io + VM](docs/deploy-fly.md) (~$3/mo, 20 min)
- **GCP-native managed control plane?** → [Cloud Run + VM](docs/deploy-cloud-run.md) (45 min)

## CLI reference

### `hiveqactl up`

```bash
hiveqactl up --name <name> --compose <path> [--service <svc:port>...]
```

- `--name` — stack name (becomes the network hostname)
- `--compose` — path to docker-compose.yml
- `--service` — explicit service:port mapping (repeatable; auto-detected from `ports:` if omitted)

### `hiveqactl down`

```bash
hiveqactl down --name <name>
```

Stops containers, removes namespaced volumes, deregisters the network node.

### `hiveqactl list`

```bash
hiveqactl list
```

Shows all running stacks with their URLs and services.

## Daemon configuration

### Flags

| Flag | Env var | Default | Description |
|---|---|---|---|
| `--state-dir` | `HIVEQA_STATE_DIR` | `~/.local/share/hiveqa` | State directory |
| `--control-url` | `HIVEQA_CONTROL_URL` | _(Tailscale SaaS)_ | headscale URL |
| `--auth-key` | `TS_AUTHKEY` | _(required)_ | Pre-auth key |
| `--tls` | `HIVEQA_TLS` | `tailscale` | TLS mode: `tailscale`, `acme`, `none` |
| `--acme-domain` | `HIVEQA_ACME_DOMAIN` | | Parent domain for ACME certs |
| `--acme-email` | `HIVEQA_ACME_EMAIL` | | ACME registration email |
| `--acme-dns-provider` | `HIVEQA_ACME_DNS_PROVIDER` | | DNS-01 provider: `cloudflare`, `gcloud`, `route53` |

### DNS-01 provider credentials

| Provider | Required env vars |
|---|---|
| `cloudflare` | `CLOUDFLARE_API_TOKEN` |
| `gcloud` | `GCP_PROJECT` + Application Default Credentials |
| `route53` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_REGION` (or IAM role) |

## What gets rewritten

Given:

```yaml
services:
  web:
    image: nginx
    ports: ["8080:80"]
    volumes: [assets:/var/www]
  db:
    image: postgres
    ports: ["5432:5432"]
    volumes: [pgdata:/var/lib/postgresql/data]
volumes:
  assets:
  pgdata:
```

`hiveqactl up --name foo` produces:

| Original | Rewritten |
|---|---|
| `ports: ["8080:80"]` | _(removed)_ |
| `volumes: [assets:...]` | `volumes: [hiveqa-foo-assets:...]` |
| `volumes: pgdata:` | `volumes: hiveqa-foo-pgdata:` |
| Project name | `hiveqa-foo` |

## How it compares

| Approach | Port conflicts? | Setup | TLS | Self-hosted? |
|---|---|---|---|---|
| **hiveqa** | None | CLI command | Automatic | Optional |
| Port offsets (`8001:80`, `8002:80`) | Manual | Edit compose | None | Yes |
| Traefik reverse proxy | None | Host config | Manual | Yes |
| Tailscale sidecar per stack | None | Edit every compose | Automatic | Optional |
| ddev / lando | None | Tool-specific | Local CA | Yes |
