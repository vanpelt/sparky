#!/usr/bin/env bash
# Run the REAL gateway Pod entrypoint natively on this machine against a
# working-tree build. No container, no VM, no Kubernetes.
#
# deploy/kubernetes/gateway-entrypoint.sh is executed unmodified: every input it
# takes is an environment variable with a shell default, and the one thing that
# was not — the binary path — is now "${SPARKBOX_BIN:-/usr/local/bin/sparkbox}".
# So the script a gateway change is exercised against here is byte-identical to
# the one CKS runs, and a flag that only exists in this file cannot drift away
# from production.
#
# Everything after the entrypoint's own flags is appended by it as "$@", and Go's
# flag package takes the LAST occurrence of a repeated flag. That is how the
# three listen addresses below move to loopback and off the ports OrbStack and
# the Pod both want, without editing the production command line. Verified by
# the startup line it logs: `ssh=127.0.0.1:2222 api=127.0.0.1:18080`.
#
# NO SILENT CAPS. This runs the real control plane — SSH gateway, edge proxy,
# consoles, REST API, OIDC issuer, launch links, passkeys, roster — with the
# mock VM driver, which is what --gateway-only requires in production too. It
# deviates from the Pod in exactly three announced ways:
#
#   1. listeners are bound to 127.0.0.1 rather than 0.0.0.0, because users.conf
#      here holds the developer's own real public key.
#   2. TLS is off (SPARKBOX_PROXY_TLS=false): there is no public DNS name and no
#      ACME challenge that could be answered for one.
#   3. the fleet identity is minted locally into .dev/gateway/keys instead of
#      hydrated from a Secret, so this gateway is a different fleet than CKS.
#
# What it therefore does NOT exercise is listed in hack/dev/README.md. Read it
# before treating a green run here as evidence about the cluster.
#
# Usage:
#   hack/dev/gateway.sh start|stop|restart|status|logs [-f|LINES]
#
# Env:
#   SPARKBOX_BIN              binary to run (default: build .dev/bin/sparkbox)
#   SPARKBOX_PROXY_DOMAIN     default dev.localhost — browsers resolve *.localhost
#                             to 127.0.0.1 with no /etc/hosts entry
#   SPARKBOX_DEV_EDGE_PORT    default 8081
#   SPARKBOX_DEV_SSH_PORT     default 2222
#   SPARKBOX_DEV_API_PORT     default 18080 (the Pod's 8080 is usually OrbStack's)
#   SPARKBOX_DEV_HANDLE       operator handle seeded into users.conf (default $USER)
#   SPARKBOX_DEV_SSH_KEY      public key for that handle (default ~/.ssh/id_*.pub)
#   SPARKBOX_GITHUB_APP_CLIENT_ID
#                             client id of a GitHub App to mint repo credentials
#                             with. Unset leaves repo attachments unavailable,
#                             which is what `repo add` reports today.
#   SPARKBOX_DEV_OP_ITEM      1Password secret reference to that App's private
#                             key, e.g. op://Hivemind-Dev/github-app-key/password.
#                             Read on start into the key dir, removed on stop.
#   SPARKBOX_DEV_OP_ACCOUNT   1Password account for the read (default: whichever
#                             account `op` resolves the reference in). Name it
#                             when more than one account is signed in.
#   SPARKBOX_GITHUB_APP_KEY_FILE
#                             use a PEM already on disk instead of 1Password.
#                             Takes precedence; the two are mutually exclusive.
set -euo pipefail

# --- bash floor -------------------------------------------------------------
# gateway-entrypoint.sh expands possibly-empty arrays ("${tls_args[@]}") under
# `set -u`. Before bash 4.4 that IS an unbound-variable error, so the production
# script aborts on macOS's /bin/bash (3.2.57, measured) before it reaches exec.
# Nothing about that is worth working around in the production script; find a
# newer bash instead, or refuse and say which one is missing.
if [ "${BASH_VERSINFO[0]}" -lt 4 ] ||
  { [ "${BASH_VERSINFO[0]}" -eq 4 ] && [ "${BASH_VERSINFO[1]}" -lt 4 ]; }; then
  for candidate in /opt/homebrew/bin/bash /usr/local/bin/bash /opt/local/bin/bash; do
    [ -x "$candidate" ] || continue
    ok=$("$candidate" -c 'echo $((BASH_VERSINFO[0] * 100 + BASH_VERSINFO[1]))' 2>/dev/null || echo 0)
    [ "$ok" -ge 404 ] || continue
    exec "$candidate" "$0" "$@"
  done
  echo "hack/dev/gateway.sh needs bash >= 4.4; this is $BASH_VERSION ($BASH)" >&2
  echo "reason: deploy/kubernetes/gateway-entrypoint.sh expands empty arrays under" >&2
  echo "        'set -u', which bash 3.2 (all macOS ships) treats as unbound and aborts on" >&2
  echo "fix:    brew install bash   # installs /opt/homebrew/bin/bash" >&2
  exit 1
fi
readonly dev_bash="$BASH"

# --- layout -----------------------------------------------------------------
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
readonly module_dir=$(cd -- "$script_dir/../.." && pwd)
readonly entrypoint="$module_dir/deploy/kubernetes/gateway-entrypoint.sh"
readonly dev_dir="$module_dir/.dev"
readonly gw_dir="$dev_dir/gateway"
readonly key_dir="$gw_dir/keys"
readonly durable_dir="$gw_dir/durable"
readonly state_dir="$durable_dir/gateway/control" # the entrypoint's own default
readonly users_file="$gw_dir/users.conf"
readonly log_file="$gw_dir/gateway.log"
readonly pid_file="$gw_dir/gateway.pid"
readonly default_bin="$dev_dir/bin/sparkbox"

readonly proxy_domain="${SPARKBOX_PROXY_DOMAIN:-dev.localhost}"
readonly edge_port="${SPARKBOX_DEV_EDGE_PORT:-8081}"
readonly ssh_port="${SPARKBOX_DEV_SSH_PORT:-2222}"
readonly api_port="${SPARKBOX_DEV_API_PORT:-18080}"

# The five private keys --require-keys refuses to mint, plus the two public
# halves that live in the state dir rather than the key dir. Names come from
# internal/nodepki and the loads in cmd/sparkbox/main.go. Six of the seven are
# what deploy.sh demands of the sparkbox-identity Secret; the seventh,
# gateway_control_cert.pem, is a short-lived leaf the gateway re-issues from
# the CA, so it is escrowed nowhere and only listed here to know a mint pass
# actually got that far.
readonly identity_keys=(
  gateway_host_key.pem
  gateway_upstream_key.pem
  oidc_signing_key.pem
  node_ca_key.pem
  gateway_control_key.pem
)
readonly identity_certs=(
  node_ca_cert.pem
  gateway_control_cert.pem
)

die() {
  echo "gateway.sh: $*" >&2
  exit 1
}

running_pid() {
  local pid
  [ -f "$pid_file" ] || return 1
  pid=$(cat "$pid_file" 2>/dev/null) || return 1
  [ -n "$pid" ] || return 1
  kill -0 "$pid" 2>/dev/null || return 1
  echo "$pid"
}

# --- identity ---------------------------------------------------------------
seed_users() {
  [ -f "$users_file" ] && return 0
  local handle key
  handle="${SPARKBOX_DEV_HANDLE:-$(id -un)}"
  # users.ValidHandle: 2-32 characters of a-z, 0-9, dash.
  handle=$(printf '%s' "$handle" | tr '[:upper:]' '[:lower:]' | tr -c 'a-z0-9-' '-')
  key="${SPARKBOX_DEV_SSH_KEY:-}"
  if [ -z "$key" ]; then
    for candidate in "$HOME/.ssh/id_ed25519.pub" "$HOME/.ssh/id_ecdsa.pub" "$HOME/.ssh/id_rsa.pub"; do
      [ -f "$candidate" ] || continue
      key=$candidate
      break
    done
  fi
  [ -n "$key" ] ||
    die "no SSH public key found for users.conf (looked at ~/.ssh/id_{ed25519,ecdsa,rsa}.pub)
     fix: ssh-keygen -t ed25519   — or set SPARKBOX_DEV_SSH_KEY=/path/to/key.pub"
  [ -f "$key" ] || die "SPARKBOX_DEV_SSH_KEY=$key does not exist"
  printf '%s %s\n' "$handle" "$(cat "$key")" > "$users_file"
  chmod 0600 "$users_file"
  echo "seeded $users_file: operator '$handle' from $key"
}

identity_complete() {
  local file
  for file in "${identity_keys[@]}"; do
    [ -f "$key_dir/$file" ] || return 1
  done
  for file in "${identity_certs[@]}"; do
    [ -f "$state_dir/$file" ] || return 1
  done
  return 0
}

# Mint the fleet identity the way a single-host run does: the same serve
# command with --require-keys turned back off, so every file is created by the
# code that will later load it rather than by an openssl invocation in this
# script that could drift from what nodepki/oidc actually parse.
#
# The served gateway is always --require-keys (verified: an empty key dir gets
# "host key: ... no such file" and mints nothing). This pass runs whenever the
# local set is incomplete, which is the deliberate difference — a half-deleted
# LOCAL identity is a scratch directory to rebuild, not a fleet to lock out.
mint_identity() {
  echo "minting a local fleet identity into $key_dir (one time)"
  local pid deadline
  serve --require-keys=false &
  pid=$!
  deadline=$((SECONDS + 30))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if identity_complete; then
      kill "$pid" 2>/dev/null || true
      wait "$pid" 2>/dev/null || true
      derive_trust_pubs
      return 0
    fi
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  kill "$pid" 2>/dev/null || true
  wait "$pid" 2>/dev/null || true
  tail -n 20 "$log_file" >&2 || true
  die "identity mint did not produce all seven files; if .dev/gateway holds a
     half-written identity the fix is to delete it: rm -rf $gw_dir"
}

# The two public keys a node is given to trust its gateway, derived exactly the
# way deploy.sh derives them for the sparkbox-node-trust Secret. Nothing in a
# gateway-only run reads them; they are here so `sparkbox devpod`'s node has a
# trust bundle to mount without re-deriving it.
derive_trust_pubs() {
  local name
  for name in gateway_host_key gateway_upstream_key; do
    [ -f "$key_dir/$name.pub" ] && continue
    ssh-keygen -y -f "$key_dir/$name.pem" > "$key_dir/$name.pub"
  done
}

# --- github app key ---------------------------------------------------------
# The one fleet secret that cannot be minted locally: GitHub holds the public
# half, so a dev box either gets an App's private key or answers "no GitHub App
# is configured on this host" to every `repo add`.
#
# It is read from 1Password on start rather than kept in the tree, and removed
# on stop, so the only durable copy is the one in the vault. That is the point:
# .dev/ is disposable and this key is not regenerable from here.
#
# Deliberately NOT `sparkbox fetch-secrets --provider op`, which is the
# production path for exactly this file. Fetch walks the entire secret manifest
# and writes every hit into --key-dir, so pointing it at a vault holding a real
# fleet identity would overwrite this box's locally minted gateway, upstream and
# OIDC keys with that fleet's -- and a dev gateway holding production identity
# keys is a worse outcome than a dev gateway with no GitHub App. A vault holding
# only this one key does not work either: three manifest entries are
# required:true and a missing one is a hard failure. So this reads the single
# reference it was given and touches nothing else.
readonly app_key_file="$key_dir/github_app_key.pem"

fetch_app_key() {
  local ref="${SPARKBOX_DEV_OP_ITEM:-}"
  # An explicit key file wins and suppresses the 1Password path entirely. Two
  # sources for one secret, silently preferring one, is how you end up debugging
  # an App that is not the App you edited.
  if [ -n "${SPARKBOX_GITHUB_APP_KEY_FILE:-}" ]; then
    [ -z "$ref" ] || die "set SPARKBOX_GITHUB_APP_KEY_FILE or SPARKBOX_DEV_OP_ITEM, not both"
    [ -r "$SPARKBOX_GITHUB_APP_KEY_FILE" ] ||
      die "SPARKBOX_GITHUB_APP_KEY_FILE=$SPARKBOX_GITHUB_APP_KEY_FILE is not readable"
    return 0
  fi
  # Unconditional, so a run without the reference cannot inherit the key a
  # previous run left behind and mint against an App nobody named.
  rm -f "$app_key_file"
  [ -n "$ref" ] || return 0
  command -v op > /dev/null 2>&1 ||
    die "SPARKBOX_DEV_OP_ITEM is set but the 1Password CLI is not on PATH (brew install 1password-cli)"

  local args=(read --no-newline "$ref")
  [ -n "${SPARKBOX_DEV_OP_ACCOUNT:-}" ] && args+=(--account "$SPARKBOX_DEV_OP_ACCOUNT")

  # Redirected straight to the destination under a restrictive umask rather than
  # captured into a variable: a private key in a shell variable is inherited by
  # every child of this script, and it would reach the gateway's own environment.
  if ! (umask 077; op "${args[@]}" > "$app_key_file"); then
    rm -f "$app_key_file"
    echo "gateway.sh: could not read $ref from 1Password." >&2
    echo "  A 'RequestDelegatedSession' error is the desktop-app integration, not the" >&2
    echo "  reference: Settings -> Developer -> Integrate with 1Password CLI, then restart" >&2
    echo "  the app fully. 'op signin' skips the desktop app entirely. Note that 'op whoami'" >&2
    echo "  answers from local config even while the session is broken, so it proves" >&2
    echo "  nothing -- 'op vault list' is the test." >&2
    die "1Password read failed"
  fi
  # op exits 0 having written nothing when the field exists and is empty, which
  # would otherwise surface as an unhelpful PEM parse error from ghapp.LoadKey.
  [ -s "$app_key_file" ] || {
    rm -f "$app_key_file"
    die "$ref resolved to an empty value; check the field name (a reference ends in its field)"
  }
  chmod 0600 "$app_key_file"
  export SPARKBOX_GITHUB_APP_KEY_FILE="$app_key_file"
  echo "read the GitHub App key from 1Password into $app_key_file"
}

# Both halves or neither, and say which is missing. cmd/sparkbox logs this too,
# but it logs one line from inside a starting server; a box configured halfway
# should hear about it before the gateway comes up rather than when a clone
# fails inside a VM.
warn_app_config() {
  local have_key="" have_id=""
  [ -n "${SPARKBOX_GITHUB_APP_KEY_FILE:-}" ] && have_key=1
  [ -n "${SPARKBOX_GITHUB_APP_CLIENT_ID:-}" ] && have_id=1
  if [ -n "$have_key" ] && [ -z "$have_id" ]; then
    echo "gateway.sh: a GitHub App key is present but SPARKBOX_GITHUB_APP_CLIENT_ID is not" >&2
    echo "            set, so repo attachments stay disabled: a key cannot say which App" >&2
    echo "            it belongs to" >&2
  elif [ -z "$have_key" ] && [ -n "$have_id" ]; then
    echo "gateway.sh: SPARKBOX_GITHUB_APP_CLIENT_ID is set but no key was found; set" >&2
    echo "            SPARKBOX_DEV_OP_ITEM or SPARKBOX_GITHUB_APP_KEY_FILE" >&2
  fi
}

# --- run --------------------------------------------------------------------
# The production entrypoint, with the dev-only overrides appended. It is not
# executable in the tree (the Containerfile chmods it on the way into the
# image), so it is invoked through the bash we resolved above — which is also
# the only way to guarantee the 4.4 floor it needs.
#
# Always called as `serve &`, and it execs so that the recorded $! is the
# sparkbox process itself rather than a wrapper subshell that SIGTERM would
# leave the server orphaned behind.
serve() {
  export SPARKBOX_BIN="$bin"
  export SPARKBOX_DURABLE_DIR="$durable_dir"
  export SPARKBOX_VM_STATE_DIR="$gw_dir/run/vm"
  export SPARKBOX_KEY_DIR="$key_dir"
  export SPARKBOX_USERS_FILE="$users_file"
  export SPARKBOX_PROXY_DOMAIN="$proxy_domain"
  export SPARKBOX_PROXY_TLS=false
  export SPARKBOX_SSH_ADVERTISE_HOST="${SPARKBOX_SSH_ADVERTISE_HOST:-127.0.0.1}"
  export SPARKBOX_NODE_NAME="${SPARKBOX_NODE_NAME:-dev-gateway}"
  export SPARKBOX_CLUSTER_ID="${SPARKBOX_CLUSTER_ID:-dev}"
  exec "$dev_bash" "$entrypoint" \
    --ssh-addr "127.0.0.1:$ssh_port" \
    --proxy-addr "127.0.0.1:$edge_port" \
    --api-addr "127.0.0.1:$api_port" \
    --proxy-advertise-port "$edge_port" \
    --ssh-advertise-port "$ssh_port" \
    "$@" >> "$log_file" 2>&1
}

# The HTTP status the edge gives the user console, or 000 when it is not
# listening yet. curl already writes 000 for a connection failure, so the
# fallback here only covers curl itself being absent or killed.
edge_probe() {
  local code
  code=$(curl -s -o /dev/null -m 2 -w '%{http_code}' \
    -H "Host: my.$proxy_domain" "http://127.0.0.1:$edge_port/" 2>/dev/null) || true
  printf '%s' "${code:-000}"
}

build() {
  local started elapsed
  started=$SECONDS
  (cd "$module_dir" && go build -o "$default_bin" ./cmd/sparkbox) ||
    die "go build failed; the gateway was not restarted"
  elapsed=$((SECONDS - started))
  echo "built $default_bin in ${elapsed}s"
}

cmd_start() {
  local pid code
  if pid=$(running_pid); then
    echo "already running (pid $pid); use restart"
    return 0
  fi
  mkdir -p "$key_dir" "$state_dir" "$gw_dir/run/vm" "$dev_dir/bin"
  chmod 0700 "$key_dir"
  # An explicit SPARKBOX_BIN is taken as given (a release binary, another
  # worktree); otherwise this is the edit-to-observable loop and it rebuilds.
  if [ -n "${SPARKBOX_BIN:-}" ]; then
    bin="$SPARKBOX_BIN"
    [ -x "$bin" ] || die "SPARKBOX_BIN=$bin is not executable"
  else
    build
    bin="$default_bin"
  fi
  seed_users
  identity_complete || mint_identity
  fetch_app_key
  warn_app_config
  printf '\n=== %s: start %s ===\n' "$(date '+%Y-%m-%dT%H:%M:%S')" "$bin" >> "$log_file"
  serve &
  echo $! > "$pid_file"
  local deadline
  deadline=$((SECONDS + 20))
  while [ "$SECONDS" -lt "$deadline" ]; do
    running_pid > /dev/null || {
      tail -n 20 "$log_file" >&2
      rm -f "$pid_file"
      die "gateway exited during startup (log: $log_file)"
    }
    code=$(edge_probe)
    if [ "$code" != 000 ]; then
      echo "gateway up (pid $(cat "$pid_file")), edge answered $code"
      cmd_status
      return 0
    fi
    sleep 0.2
  done
  tail -n 20 "$log_file" >&2
  die "gateway did not answer on 127.0.0.1:$edge_port within 20s"
}

cmd_stop() {
  local pid deadline
  if ! pid=$(running_pid); then
    rm -f "$pid_file"
    echo "not running"
    return 0
  fi
  kill "$pid" 2>/dev/null || true
  deadline=$((SECONDS + 10))
  while [ "$SECONDS" -lt "$deadline" ]; do
    kill -0 "$pid" 2>/dev/null || break
    sleep 0.2
  done
  if kill -0 "$pid" 2>/dev/null; then
    echo "pid $pid ignored SIGTERM for 10s; sending SIGKILL" >&2
    kill -9 "$pid" 2>/dev/null || true
  fi
  rm -f "$pid_file"
  # The vault is the durable copy; nothing needs this one once the process
  # holding it is gone. Only the file this script wrote, never an operator's own
  # SPARKBOX_GITHUB_APP_KEY_FILE.
  rm -f "$app_key_file"
  echo "stopped (pid $pid)"
}

cmd_status() {
  local pid
  if pid=$(running_pid); then
    echo "running  pid $pid  bin ${bin:-${SPARKBOX_BIN:-$default_bin}}"
  else
    echo "stopped"
    return 1
  fi
  cat <<EOF
  edge     http://127.0.0.1:$edge_port   (Host: my.$proxy_domain — HTTP $(edge_probe))
  console  http://my.$proxy_domain:$edge_port
  api      http://api.$proxy_domain:$edge_port/openapi.json
  ssh      ssh -p $ssh_port ctl@127.0.0.1
  log      $log_file
EOF
}

cmd_logs() {
  [ -f "$log_file" ] || die "no log yet at $log_file"
  if [ "${1:-}" = "-f" ]; then
    tail -f "$log_file"
  else
    tail -n "${1:-40}" "$log_file"
  fi
}

usage() {
  echo "usage: hack/dev/gateway.sh start|stop|restart|status|logs [-f|LINES]" >&2
}

bin=""
case "${1:-}" in
  start) cmd_start ;;
  stop) cmd_stop ;;
  restart)
    cmd_stop
    cmd_start
    ;;
  status) cmd_status ;;
  logs)
    shift
    cmd_logs "$@"
    ;;
  *)
    usage
    exit 2
    ;;
esac
