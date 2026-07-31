#!/usr/bin/env bash
# Deploy one ephemeral Sparkbox gateway to a CKS CPU node pool.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: deploy.sh --image IMAGE [options]

Options:
  --context CONTEXT       kubectl context (default: current context)
  --node-pool NAME        CKS NodePool label value (default: default-node-pool)
  --public-key PATH       operator SSH public key (default: ~/.ssh/id_ed25519.pub)
  --user HANDLE           operator handle in users.conf (default: local username)
  --image IMAGE           linux/amd64 Sparkbox image to deploy (required)
EOF
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
context=$(kubectl config current-context)
node_pool=default-node-pool
public_key="${HOME}/.ssh/id_ed25519.pub"
operator=$(id -un)
image=
namespace=sparkbox-poc

while [ "$#" -gt 0 ]; do
  case "$1" in
    --context)
      context=${2:?--context requires a value}
      shift 2
      ;;
    --node-pool)
      node_pool=${2:?--node-pool requires a value}
      shift 2
      ;;
    --public-key)
      public_key=${2:?--public-key requires a value}
      shift 2
      ;;
    --user)
      operator=${2:?--user requires a value}
      shift 2
      ;;
    --image)
      image=${2:?--image requires a value}
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[ -n "$image" ] || { echo "--image is required" >&2; exit 2; }
[ -f "$public_key" ] || { echo "public key not found: $public_key" >&2; exit 2; }
case "$operator" in
  *[!a-z0-9_-]*|'')
    echo "--user must contain only lowercase letters, digits, underscores, and dashes" >&2
    exit 2
    ;;
esac
case "$node_pool" in
  *[!a-zA-Z0-9_.-]*|'')
    echo "--node-pool contains unsupported characters" >&2
    exit 2
    ;;
esac
case "$image" in
  *'|'*|*'
'*)
    echo "--image contains unsupported characters" >&2
    exit 2
    ;;
esac

key=$(tr -d '\r\n' < "$public_key")
case "$key" in
  ssh-*|ecdsa-*|sk-*)
    ;;
  *)
    echo "$public_key does not look like an OpenSSH public key" >&2
    exit 2
    ;;
esac

k=(kubectl --context "$context")
echo "Deploying to kubectl context: $context"
"${k[@]}" apply -f "$script_dir/namespace.yaml"
"${k[@]}" apply -f "$script_dir/service.yaml"

echo "Waiting for CoreWeave to allocate the wildcard DNS record..."
deadline=$((SECONDS + 600))
external_record=
while [ "$SECONDS" -lt "$deadline" ]; do
  external_record=$("${k[@]}" -n "$namespace" get service sparkbox \
    -o=jsonpath='{.status.conditions[?(@.type=="ExternalRecords")].message}' 2>/dev/null || true)
  [ -n "$external_record" ] && break
  sleep 5
done
[ -n "$external_record" ] || {
  echo "timed out waiting for the Service ExternalRecords condition" >&2
  "${k[@]}" -n "$namespace" describe service sparkbox >&2 || true
  exit 1
}

proxy_domain=${external_record#*.}
case "$proxy_domain" in
  *.coreweave.app)
    ;;
  *)
    echo "unexpected ExternalRecords value: $external_record" >&2
    exit 1
    ;;
esac

temporary_dir=$(mktemp -d)
users_file="$temporary_dir/users.conf"
cleanup() {
  rm -f "$users_file"
  rmdir "$temporary_dir" 2>/dev/null || true
}
trap cleanup EXIT
printf '%s %s\n' "$operator" "$key" > "$users_file"

"${k[@]}" -n "$namespace" create secret generic sparkbox-users \
  --from-file="users.conf=$users_file" \
  --dry-run=client -o yaml | "${k[@]}" apply -f -

if command -v sha256sum >/dev/null 2>&1; then
  users_hash=$(sha256sum "$users_file" | awk '{print $1}')
else
  users_hash=$(shasum -a 256 "$users_file" | awk '{print $1}')
fi

sed \
  -e "s|__SPARKBOX_IMAGE__|$image|g" \
  -e "s|__SPARKBOX_PROXY_DOMAIN__|$proxy_domain|g" \
  -e "s|__SPARKBOX_NODE_POOL__|$node_pool|g" \
  -e "s|__SPARKBOX_USERS_HASH__|$users_hash|g" \
  "$script_dir/deployment.yaml" | "${k[@]}" apply -f -

echo "Waiting for Sparkbox to become available (first boot downloads ~750 MB)..."
"${k[@]}" -n "$namespace" rollout status deployment/sparkbox --timeout=20m

echo
echo "Sparkbox is ready."
echo "  SSH:  ssh -p 22 ctl@ssh.$proxy_domain help"
echo "  New:  ssh -p 22 new@ssh.$proxy_domain"
echo "  Web:  http://<sandbox>.$proxy_domain"
echo "  Logs: kubectl --context $context -n $namespace logs -f deployment/sparkbox"
