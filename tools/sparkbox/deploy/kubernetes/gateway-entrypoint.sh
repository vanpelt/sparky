#!/usr/bin/env bash
# Run the public control plane without host devices, host mounts, or a local VM
# driver. This script must remain usable by the numeric non-root UID in the Pod.
set -euo pipefail
umask 077

readonly durable_dir="${SPARKBOX_DURABLE_DIR:-/mnt/sparkbox-durable}"
readonly state_dir="${SPARKBOX_STATE_DIR:-$durable_dir/gateway/control}"
readonly vm_state_dir="${SPARKBOX_VM_STATE_DIR:-/run/sparkbox/vm}"
readonly key_dir="${SPARKBOX_KEY_DIR:-/run/sparkbox/keys}"
readonly users_file="${SPARKBOX_USERS_FILE:-/etc/sparkbox/users/users.conf}"
readonly proxy_domain="${SPARKBOX_PROXY_DOMAIN:?SPARKBOX_PROXY_DOMAIN is required}"
readonly proxy_tls="${SPARKBOX_PROXY_TLS:-true}"
readonly tls_provider="${SPARKBOX_TLS_PROVIDER:-autocert}"
readonly tls_email="${SPARKBOX_TLS_EMAIL:-}"
readonly ssh_advertise_host="${SPARKBOX_SSH_ADVERTISE_HOST:-ssh.$proxy_domain}"
# The GitHub App that mints repository credentials. Optional and separate from
# the account-linking app: only this one has a private key, and a host without
# both the id and github_app_key.pem simply offers no repo attachments rather
# than failing to start.
readonly github_app_client_id="${SPARKBOX_GITHUB_APP_CLIENT_ID:-}"
readonly node_name="${SPARKBOX_NODE_NAME:-cks-gateway}"
readonly cluster_id="${SPARKBOX_CLUSTER_ID:-cks-poc}"
proxy_advertise_port="${SPARKBOX_PROXY_ADVERTISE_PORT:-}"
if [ -z "$proxy_advertise_port" ]; then
  if [ "$proxy_tls" = true ]; then
    proxy_advertise_port=443
  else
    proxy_advertise_port=80
  fi
fi
readonly proxy_advertise_port

mkdir -p "$state_dir" "$vm_state_dir"

tls_args=()
if [ "$proxy_tls" = true ]; then
  tls_args+=(--proxy-tls --tls-provider "$tls_provider")
  if [ -n "$tls_email" ]; then
    tls_args+=(--tls-email "$tls_email")
  fi
fi

echo "starting control-plane-only Sparkbox gateway for *.$proxy_domain (TLS: $proxy_tls)"
github_app_args=()
if [ -n "$github_app_client_id" ]; then
  github_app_args=(--github-app-client-id "$github_app_client_id")
fi

exec /usr/local/bin/sparkbox serve \
  --gateway-only \
  --driver mock \
  --state-dir "$state_dir" \
  --vm-state-dir "$vm_state_dir" \
  --key-dir "$key_dir" \
  --require-keys \
  --users "$users_file" \
  --checkpoint-dir "$durable_dir" \
  --checkpoint-prefix checkpoints \
  --metadata-addr "" \
  --ssh-addr :2222 \
  --ssh-advertise-host "$ssh_advertise_host" \
  --ssh-advertise-port 22 \
  --api-addr 127.0.0.1:8080 \
  --metrics-addr "" \
  --proxy-addr :8081 \
  --proxy-advertise-port "$proxy_advertise_port" \
  --proxy-domain "$proxy_domain" \
  --node-name "$node_name" \
  --cluster-id "$cluster_id" \
  --host-mem-mb 1 \
  --mem-admission-pct 100 \
  --max-running-per-owner 2 \
  "${tls_args[@]}" \
  "${github_app_args[@]}" \
  "$@"
