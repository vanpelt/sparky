#!/usr/bin/env bash
# Deploy one Node-local Sparkbox gateway to a CKS CPU node pool.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: deploy.sh --image IMAGE [options]

Options:
  --context CONTEXT       kubectl context (default: current context)
  --node-pool NAME        CKS NodePool label value (default: default-node-pool)
  --node NAME             exact CKS Node (default: sole ready eligible pool Node)
  --public-key PATH       operator SSH public key (default: ~/.ssh/id_ed25519.pub)
  --user HANDLE           operator handle in users.conf (default: local username)
  --image IMAGE           linux/amd64 Sparkbox image to deploy (required)
EOF
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
context=$(kubectl config current-context)
node_pool=default-node-pool
node=
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
    --node)
      node=${2:?--node requires a value}
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
case "$node" in
  '')
    ;;
  -*|*[!a-zA-Z0-9.-]*)
    echo "--node contains unsupported characters" >&2
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

# The hostPath hot tier belongs to one physical Node. Resolve and pin that Node
# before creating any resources so a later Pod replacement cannot silently
# start against another Node's empty /mnt/local directory.
eligible_nodes=()
candidate_nodes=$(
  "${k[@]}" get nodes \
    -l "compute.coreweave.com/node-pool=$node_pool,kubernetes.io/arch=amd64" \
    --field-selector spec.unschedulable=false \
    -o name
)
while IFS= read -r candidate; do
  [ -n "$candidate" ] || continue
  candidate=${candidate#node/}
  ready=$("${k[@]}" get node "$candidate" \
    -o=jsonpath='{.status.conditions[?(@.type=="Ready")].status}')
  [ "$ready" = True ] && eligible_nodes+=("$candidate")
done <<< "$candidate_nodes"

if [ -n "$node" ]; then
  node_is_eligible=0
  for candidate in "${eligible_nodes[@]}"; do
    if [ "$candidate" = "$node" ]; then
      node_is_eligible=1
      break
    fi
  done
  [ "$node_is_eligible" = 1 ] || {
    echo "node $node is not a ready, schedulable amd64 member of NodePool $node_pool" >&2
    exit 1
  }
else
  if [ "${#eligible_nodes[@]}" -ne 1 ]; then
    echo "NodePool $node_pool has ${#eligible_nodes[@]} ready, schedulable amd64 Nodes; pass --node explicitly" >&2
    if [ "${#eligible_nodes[@]}" -gt 0 ]; then
      printf '  %s\n' "${eligible_nodes[@]}" >&2
    fi
    exit 1
  fi
  node=${eligible_nodes[0]}
fi
echo "Pinning Sparkbox local state to Node: $node"

"${k[@]}" apply -f "$script_dir/namespace.yaml"

identity_secret=sparkbox-identity
if ! "${k[@]}" -n "$namespace" get secret "$identity_secret" >/dev/null 2>&1; then
  echo "required Secret $namespace/$identity_secret is absent" >&2
  echo "capture the current POC identity first:" >&2
  echo "  $script_dir/capture-identity.sh --context $context" >&2
  echo "or restore the six documented identity files from escrow into that Secret" >&2
  exit 1
fi
required_identity_files=(
  gateway_host_key.pem
  gateway_upstream_key.pem
  oidc_signing_key.pem
  node_ca_cert.pem
  node_ca_key.pem
  gateway_control_key.pem
)
missing_identity_files=()
for file in "${required_identity_files[@]}"; do
  escaped_file=${file//./\\.}
  value=$("${k[@]}" -n "$namespace" get secret "$identity_secret" \
    -o "jsonpath={.data.$escaped_file}")
  [ -n "$value" ] || missing_identity_files+=("$file")
done
if [ "${#missing_identity_files[@]}" -gt 0 ]; then
  echo "Secret $namespace/$identity_secret is incomplete; missing:" >&2
  printf '  %s\n' "${missing_identity_files[@]}" >&2
  echo "repair it before deploying; Sparkbox uses --require-keys and will not mint replacements" >&2
  exit 1
fi

"${k[@]}" apply -f "$script_dir/durable-pvc.yaml"
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
  -e "s|__SPARKBOX_NODE__|$node|g" \
  -e "s|__SPARKBOX_USERS_HASH__|$users_hash|g" \
  "$script_dir/deployment.yaml" | "${k[@]}" apply -f -

# The first checkpoint POC overlaid a content-addressed hostPath binary at
# /usr/local/bin/sparkbox. kubectl apply correctly preserves fields it did not
# create, so remove that one development-only mount explicitly after applying
# the real image. Do the mount, its appended args, and its marker annotation in
# one Pod-template patch so the Deployment rolls only to the final image-backed
# configuration.
runtime_mount_index=
mount_index=0
while IFS= read -r mount_path; do
  if [ "$mount_path" = /usr/local/bin/sparkbox ]; then
    runtime_mount_index=$mount_index
    break
  fi
  mount_index=$((mount_index + 1))
done < <(
  "${k[@]}" -n "$namespace" get deployment sparkbox \
    -o jsonpath='{range .spec.template.spec.containers[0].volumeMounts[*]}{.mountPath}{"\n"}{end}'
)
if [ -n "$runtime_mount_index" ]; then
  echo "Removing the temporary hostPath runtime overlay."
  runtime_patch='[{"op":"remove","path":"/spec/template/spec/containers/0/volumeMounts/'"$runtime_mount_index"'"}'
  runtime_args=$(
    "${k[@]}" -n "$namespace" get deployment sparkbox \
      -o jsonpath='{.spec.template.spec.containers[0].args}'
  )
  if [ -n "$runtime_args" ]; then
    runtime_patch+=',{"op":"remove","path":"/spec/template/spec/containers/0/args"}'
  fi
  runtime_marker=$(
    "${k[@]}" -n "$namespace" get deployment sparkbox \
      -o jsonpath='{.spec.template.metadata.annotations.sparkbox\.dev/runtime-candidate}'
  )
  if [ -n "$runtime_marker" ]; then
    runtime_patch+=',{"op":"remove","path":"/spec/template/metadata/annotations/sparkbox.dev~1runtime-candidate"}'
  fi
  runtime_patch+=']'
  "${k[@]}" -n "$namespace" patch deployment sparkbox \
    --type=json --patch "$runtime_patch"
fi

echo "Waiting for Sparkbox to become available (first boot downloads release artifacts and agent tools)..."
"${k[@]}" -n "$namespace" rollout status deployment/sparkbox --timeout=20m

echo
echo "Sparkbox is ready."
echo "  SSH:  ssh -p 22 ctl@ssh.$proxy_domain help"
echo "  New:  ssh -p 22 new@ssh.$proxy_domain"
echo "  Web:  https://my.$proxy_domain"
echo "  Logs: kubectl --context $context -n $namespace logs -f deployment/sparkbox"
