# Local Dev Quickstart

The simplest way to run hiveqa: your machine + Tailscale SaaS. No headscale, no ACME, no cloud infrastructure. Each compose stack gets a `https://<name>.<tailnet>.ts.net` URL with automatic TLS.

**Time to set up: ~5 minutes.**

## Prerequisites

- Go 1.24+ installed
- Docker + Docker Compose installed and running
- A [Tailscale](https://tailscale.com) account (free tier works — up to 100 devices)

## Step 1: Build hiveqa

```bash
cd tools/hiveqa
go build -o ~/.local/bin/hiveqad  ./cmd/hiveqad
go build -o ~/.local/bin/hiveqactl ./cmd/hiveqactl
```

## Step 2: Create a Tailscale auth key

1. Go to [Tailscale Admin → Settings → Keys](https://login.tailscale.com/admin/settings/keys)
2. Click **Generate auth key**
3. Check **Reusable** and **Ephemeral**
4. Set expiry to 90 days (or whatever you prefer)
5. Copy the key (starts with `tskey-auth-`)

## Step 3: Enable HTTPS certificates

1. Go to [Tailscale Admin → DNS](https://login.tailscale.com/admin/dns)
2. Make sure **MagicDNS** is enabled
3. Go to [HTTPS Certificates](https://login.tailscale.com/admin/settings/general) and enable HTTPS

## Step 4: Start the daemon

```bash
export TS_AUTHKEY="tskey-auth-xxxxx"
hiveqad
```

You should see:

```
hiveqad starting (state: /home/you/.local/share/hiveqa, control: , tls: tailscale)
control socket listening on /home/you/.local/share/hiveqa/hiveqad.sock
```

Leave this running in a terminal (or use `tmux`/`screen`).

## Step 5: Run your first stack

Say you have a `docker-compose.yml`:

```yaml
services:
  web:
    image: nginx
    ports:
      - "8080:80"
  db:
    image: postgres:16
    ports:
      - "5432:5432"
    environment:
      POSTGRES_PASSWORD: dev
volumes:
  pgdata:
```

Bring it up:

```bash
hiveqactl up --name my-app --compose ./docker-compose.yml
```

Output:

```
Stack "my-app" is up!
  URL:      https://my-app.yak-bebop.ts.net
  Services: web:80, db:5432
```

That's it. Open `https://my-app.yak-bebop.ts.net` in your browser (from any device on your tailnet) and you'll see the nginx welcome page with a valid TLS cert.

## Step 6: Run it again (parallel environments!)

```bash
hiveqactl up --name my-app-v2 --compose ./docker-compose.yml
```

Same compose file, completely isolated:
- Separate containers (`hiveqa-my-app-v2-web-1`, `hiveqa-my-app-v2-db-1`)
- Separate volumes (`hiveqa-my-app-v2-pgdata`)
- Separate URL (`https://my-app-v2.yak-bebop.ts.net`)
- No port conflicts

## Managing stacks

```bash
# List all running stacks
hiveqactl list

# Tear down a stack (stops containers, removes volumes)
hiveqactl down --name my-app-v2
```

## What hiveqa does to your compose file

When you run `hiveqactl up --name foo`:

1. **Strips `ports:`** — no host port bindings, no conflicts
2. **Namespaces volumes** — `pgdata` becomes `hiveqa-foo-pgdata`
3. **Sets project name** — `docker compose -p hiveqa-foo` isolates container names and networks
4. **Starts a tsnet proxy** — ephemeral Tailscale node `foo` proxies HTTPS → container

Original files are never modified. The rewritten compose file lives in `~/.local/share/hiveqa/stacks/foo/docker-compose.yml`.

## Tips

### Specify which service to proxy

By default, hiveqa discovers services from `ports:` entries. You can be explicit:

```bash
hiveqactl up --name my-app --compose ./docker-compose.yml --service web:3000
```

### Multiple services

With multiple `--service` flags, services are routed by path prefix:

```bash
hiveqactl up --name my-app --compose ./docker-compose.yml \
  --service web:3000 \
  --service api:8080
```

- `https://my-app.ts.net/` → web:3000
- `https://my-app.ts.net/api/` → api:8080

### Use in git worktrees

Perfect for Claude agents working on different branches:

```bash
# Agent 1 in worktree for feature-auth
hiveqactl up --name feature-auth --compose /path/to/worktree-1/docker-compose.yml

# Agent 2 in worktree for feature-payments
hiveqactl up --name feature-payments --compose /path/to/worktree-2/docker-compose.yml
```

Each gets its own isolated environment at a unique URL.

### Custom state directory

```bash
HIVEQA_STATE_DIR=/tmp/hiveqa hiveqad
```

## Troubleshooting

### "tsnet up: ... auth key expired"

Generate a new auth key at https://login.tailscale.com/admin/settings/keys.

### "listen TLS :443: ... HTTPS not enabled"

Enable HTTPS certificates in your Tailscale admin console (DNS → HTTPS Certificates).

### Stack is up but URL doesn't load

1. Check you're on the same tailnet: `tailscale status`
2. Check the stack is running: `hiveqactl list`
3. Check Docker: `docker ps | grep hiveqa-my-app`

### Port still in use

hiveqa strips `ports:` from the compose file, so there should be no host port conflicts. If you see port conflicts, make sure you're using `hiveqactl up` (not `docker compose up` directly).
