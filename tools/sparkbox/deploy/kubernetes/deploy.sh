#!/usr/bin/env bash
# Split Sparkbox into an unprivileged public gateway and a private VM node.
set -euo pipefail

usage() {
  cat <<'EOF'
usage: deploy.sh --image IMAGE [options]

Options:
  --context CONTEXT       kubectl context (default: current context)
  --node-pool NAME        CKS NodePool label value (default: default-node-pool)
  --node NAME             exact CKS Node (default: sole ready eligible pool Node)
  --proxy-domain DOMAIN   public Sparkbox domain (default: the domain the live
                          gateway already publishes, else the allocated
                          coreweave.app domain)
  --github-app-client-id ID
                          client id of the GitHub App that mints repository
                          credentials (default: whatever the live gateway
                          already uses). Its private key belongs in the
                          sparkbox-github-app Secret as private-key.pem.
  --hivemind-api ORIGIN   HiveMind API origin to federate with, e.g.
                          https://hivemind.wandb.tools (default: whatever the
                          live gateway already uses; empty turns the presence
                          lease and `ctl sessions` off)
  --hivemind-signin-orgs ORGS
                          comma-separated GitHub orgs whose HiveMind users may
                          sign in at https://login.<domain>/handoff, getting an
                          account on first arrival (default: whatever the live
                          gateway already uses; empty closes the door). Needs
                          --hivemind-api, which is the back channel the
                          single-use handoff code is redeemed over
  --hivemind-manifest URL release manifest deciding which hivemind new sandboxes
                          get, e.g. .../manifests/hivemind-1.0.8rc1.json
                          (default: the latest release). Use it to put a release
                          candidate on real hardware; NOT carried forward, so a
                          later run without it returns to latest.
  --public-key PATH       operator SSH public key (default: ~/.ssh/id_ed25519.pub)
  --private-key PATH      matching operator key used to approve the VM node
                          (default: public-key path without .pub; optional on re-runs)
  --user HANDLE           operator handle in users.conf (default: local username)
  --image IMAGE           linux/amd64 Sparkbox image to deploy (required)
EOF
}

script_dir=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
context=$(kubectl config current-context)
node_pool=default-node-pool
node=
requested_proxy_domain=
requested_github_app_client_id=
requested_hivemind_api=
requested_hivemind_signin_orgs=
requested_hivemind_manifest=
public_key="${HOME}/.ssh/id_ed25519.pub"
private_key=
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
    --proxy-domain)
      requested_proxy_domain=${2:?--proxy-domain requires a value}
      shift 2
      ;;
    --github-app-client-id)
      requested_github_app_client_id=${2:?--github-app-client-id requires a value}
      shift 2
      ;;
    --hivemind-manifest)
      requested_hivemind_manifest=${2:?--hivemind-manifest requires a value}
      shift 2
      ;;
    --hivemind-api)
      requested_hivemind_api=${2:?--hivemind-api requires a value}
      shift 2
      ;;
    --hivemind-signin-orgs)
      requested_hivemind_signin_orgs=${2:?--hivemind-signin-orgs requires a value}
      shift 2
      ;;
    --public-key)
      public_key=${2:?--public-key requires a value}
      shift 2
      ;;
    --private-key)
      private_key=${2:?--private-key requires a value}
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
if [ -z "$private_key" ]; then
  private_key=${public_key%.pub}
fi
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
case "$requested_proxy_domain" in
  '')
    ;;
  *)
    if [[ ! "$requested_proxy_domain" =~ ^([a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z]{2,63}$ ]]; then
      echo "--proxy-domain must be a valid DNS name" >&2
      exit 2
    fi
    if [ "${#requested_proxy_domain}" -gt 253 ]; then
      echo "--proxy-domain must be at most 253 characters" >&2
      exit 2
    fi
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

cloudflare_secret=sparkbox-cloudflare
if ! "${k[@]}" -n "$namespace" get secret "$cloudflare_secret" >/dev/null 2>&1; then
  echo "required Secret $namespace/$cloudflare_secret is absent" >&2
  echo "create it with a scoped Cloudflare Zone:Read + DNS:Edit token under the api-token key" >&2
  exit 1
fi
cloudflare_token_present=$(
  "${k[@]}" -n "$namespace" get secret "$cloudflare_secret" \
    -o 'go-template={{if index .data "api-token"}}yes{{end}}'
)
if [ "$cloudflare_token_present" != yes ]; then
  echo "Secret $namespace/$cloudflare_secret has no api-token key" >&2
  exit 1
fi

"${k[@]}" apply -f "$script_dir/durable-pvc.yaml"
if ! "${k[@]}" -n "$namespace" get service sparkbox >/dev/null 2>&1; then
  "${k[@]}" apply -f "$script_dir/service.yaml"
fi

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

allocated_domain=${external_record#*.}
case "$allocated_domain" in
  *.coreweave.app)
    ;;
  *)
    echo "unexpected ExternalRecords value: $external_record" >&2
    exit 1
    ;;
esac
# A re-run without --proxy-domain must not silently revert a deployment that
# already publishes a custom domain: WebAuthn origins, issued certificates, and
# every published sandbox URL are derived from it. Carry the live gateway's
# current domain forward unless the operator names a different one.
deployed_proxy_domain=$(
  "${k[@]}" -n "$namespace" get deployment sparkbox-gateway \
    -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="SPARKBOX_PROXY_DOMAIN")].value}' \
    2>/dev/null || true
)
proxy_domain=${requested_proxy_domain:-${deployed_proxy_domain:-$allocated_domain}}
if [ -z "$requested_proxy_domain" ] && [ -n "$deployed_proxy_domain" ] &&
  [ "$deployed_proxy_domain" != "$allocated_domain" ]; then
  echo "Keeping the deployed public domain: $proxy_domain"
  echo "  Pass --proxy-domain $allocated_domain to move back to the allocated name."
fi
if [ "$proxy_domain" != "$allocated_domain" ]; then
  echo "Configuring custom public domain: $proxy_domain"
  echo "  DNS apex and wildcard must point to the LoadBalancer before HTTPS is tested."
fi

# Same rule as the domain, for the same reason: these flags are not recorded
# anywhere in the cluster, so a re-run that omits one must carry the live value
# forward rather than silently switching repo credentials off. A gateway with no
# client id keeps its repo attachments listed and refuses to mint for them,
# which looks exactly like a GitHub outage from inside a sandbox.
deployed_github_app_client_id=$(
  "${k[@]}" -n "$namespace" get deployment sparkbox-gateway \
    -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="SPARKBOX_GITHUB_APP_CLIENT_ID")].value}' \
    2>/dev/null || true
)
github_app_client_id=${requested_github_app_client_id:-$deployed_github_app_client_id}
if [ -n "$github_app_client_id" ]; then
  echo "GitHub App for repo credentials: $github_app_client_id"
	github_app_key_present=$(
	  "${k[@]}" -n "$namespace" get secret sparkbox-github-app \
	    -o 'go-template={{if index .data "private-key.pem"}}yes{{end}}' 2>/dev/null || true
	)
	legacy_github_app_key_present=$(
	  "${k[@]}" -n "$namespace" get secret "$identity_secret" \
	    -o 'go-template={{if index .data "github_app_key.pem"}}yes{{end}}' 2>/dev/null || true
	)
	if [ "$github_app_key_present" != yes ] && [ "$legacy_github_app_key_present" != yes ]; then
	  echo "GitHub App client id is configured, but no private key is available." >&2
	  echo "Add private-key.pem to Secret $namespace/sparkbox-github-app." >&2
	  exit 1
	fi
	if [ "$github_app_key_present" != yes ]; then
	  echo "  Using legacy sparkbox-identity/github_app_key.pem for this rollout; migrate it to sparkbox-github-app/private-key.pem."
	fi
else
  echo "No GitHub App configured; repo attachments will be unavailable."
  echo "  Pass --github-app-client-id and add private-key.pem to the sparkbox-github-app Secret."
fi

# Carried forward on a re-run for the same reason as the two above. Dropping it
# would silently stop refreshing presence leases, and the only symptom is a VM
# reaped out from under a working agent an hour later — about as far from the
# deploy as a symptom gets.
deployed_hivemind_api=$(
  "${k[@]}" -n "$namespace" get deployment sparkbox-gateway \
    -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="SPARKBOX_HIVEMIND_API")].value}' \
    2>/dev/null || true
)
hivemind_api=${requested_hivemind_api:-$deployed_hivemind_api}
if [ -n "$hivemind_api" ]; then
  echo "HiveMind federation: $hivemind_api"
else
  echo "No HiveMind API configured; live agent sessions will not protect a VM from"
  echo "  the idle reaper, and \`ctl sessions\` is unavailable. Pass --hivemind-api."
fi

# Carried forward for the same reason again, and with a sharper failure: dropping
# the allowlist does not degrade the sign-in door, it CLOSES it, and the only
# symptom is a colleague's HiveMind button answering "can't sign you in" with
# nothing in the deploy output to connect it to.
deployed_hivemind_signin_orgs=$(
  "${k[@]}" -n "$namespace" get deployment sparkbox-gateway \
    -o 'jsonpath={.spec.template.spec.containers[0].env[?(@.name=="SPARKBOX_HIVEMIND_SIGNIN_ORGS")].value}' \
    2>/dev/null || true
)
hivemind_signin_orgs=${requested_hivemind_signin_orgs:-$deployed_hivemind_signin_orgs}
if [ -n "$hivemind_signin_orgs" ]; then
  if [ -n "$hivemind_api" ]; then
    echo "HiveMind sign-in: $hivemind_signin_orgs"
  else
    # Not fatal, because the gateway itself only warns — but say it here too,
    # where somebody is watching, rather than leaving it in a pod log.
    echo "HiveMind sign-in orgs are set ($hivemind_signin_orgs) but --hivemind-api is"
    echo "  empty, so the door will NOT be mounted: there is nothing to redeem against."
  fi
fi

# Deliberately NOT carried forward, unlike the three above, and the asymmetry is
# the point. Those three are permanent facts about this deployment, so losing one
# is always a mistake. A manifest override is the opposite: it is how a release
# candidate gets onto real hardware, it is meant to end, and a test pin that
# silently reinstates itself on every future deploy is the worse failure.
#
# What it must not do is vanish QUIETLY. Before this flag existed the pin was a
# hand-edited live object that no file recorded, so a clean run by anyone dropped
# it with no output at all — and the only symptom was `hivemind: No such command`
# inside a sandbox created days later. Hence the notice below: the default is
# latest, and choosing it out loud is what makes that safe.
deployed_hivemind_manifest=$(
  "${k[@]}" -n "$namespace" get deployment sparkbox-node \
    -o 'jsonpath={.spec.template.spec.initContainers[?(@.name=="prepare-vm-assets")].env[?(@.name=="HIVEMIND_MANIFEST")].value}' \
    2>/dev/null || true
)
hivemind_manifest=$requested_hivemind_manifest
if [ -n "$hivemind_manifest" ]; then
  echo "Guest hivemind pinned: $hivemind_manifest"
  echo "  Only NEWLY CREATED sandboxes take this; existing ones keep what is on their disk."
elif [ -n "$deployed_hivemind_manifest" ]; then
  echo "NOTE: dropping the guest hivemind pin this deployment was carrying:"
  echo "  $deployed_hivemind_manifest"
  echo "  New sandboxes return to the latest release. Pass --hivemind-manifest to keep it."
else
  echo "Guest hivemind: latest release"
fi

temporary_dir=$(mktemp -d)
users_file="$temporary_dir/users.conf"
gateway_private_file="$temporary_dir/gateway_host_key.pem"
gateway_public_file="$temporary_dir/gateway_host_key.pub"
gateway_upstream_private_file="$temporary_dir/gateway_upstream_key.pem"
gateway_upstream_public_file="$temporary_dir/gateway_upstream_key.pub"
known_hosts_file="$temporary_dir/known_hosts"
cleanup() {
  rm -f \
    "$users_file" "$gateway_private_file" "$gateway_public_file" \
    "$gateway_upstream_private_file" "$gateway_upstream_public_file" \
    "$known_hosts_file"
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

"${k[@]}" -n "$namespace" get secret "$identity_secret" \
  -o 'go-template={{index .data "gateway_host_key.pem" | base64decode}}' \
  > "$gateway_private_file"
chmod 0600 "$gateway_private_file"
ssh-keygen -y -f "$gateway_private_file" > "$gateway_public_file"
"${k[@]}" -n "$namespace" get secret "$identity_secret" \
  -o 'go-template={{index .data "gateway_upstream_key.pem" | base64decode}}' \
  > "$gateway_upstream_private_file"
chmod 0600 "$gateway_upstream_private_file"
ssh-keygen -y -f "$gateway_upstream_private_file" > "$gateway_upstream_public_file"
"${k[@]}" -n "$namespace" create secret generic sparkbox-node-trust \
  --from-file="gateway_host_key.pub=$gateway_public_file" \
  --from-file="gateway_upstream_key.pub=$gateway_upstream_public_file" \
  --dry-run=client -o yaml | "${k[@]}" apply -f -

# The old combined Pod must be stopped before its SQLite WAL and control
# database are copied. The migration Job copies only gateway-owned state to the
# RWX volume; sandboxes.json and the Firecracker hot tier remain on this Node.
legacy_deployment=0
if "${k[@]}" -n "$namespace" get deployment sparkbox >/dev/null 2>&1; then
  legacy_deployment=1
  echo "Stopping the combined gateway/VM Pod for the one-time state split..."
  "${k[@]}" -n "$namespace" scale deployment sparkbox --replicas=0
  "${k[@]}" -n "$namespace" rollout status deployment/sparkbox --timeout=5m
fi
if [ "$legacy_deployment" = 1 ]; then
  "${k[@]}" -n "$namespace" delete job sparkbox-split-gateway-state \
    --ignore-not-found --wait=true
  sed \
    -e "s|__SPARKBOX_IMAGE__|$image|g" \
    -e "s|__SPARKBOX_NODE__|$node|g" \
    "$script_dir/migration-job.yaml" | "${k[@]}" apply -f -
  if ! "${k[@]}" -n "$namespace" wait \
    --for=condition=complete job/sparkbox-split-gateway-state --timeout=10m; then
    "${k[@]}" -n "$namespace" logs job/sparkbox-split-gateway-state >&2 || true
    exit 1
  fi
else
  echo "No legacy combined Deployment found; using existing or fresh split gateway state."
fi

"${k[@]}" apply -f "$script_dir/service-accounts.yaml"
"${k[@]}" apply -f "$script_dir/internal-service.yaml"
"${k[@]}" apply -f "$script_dir/network-policy.yaml"

sed \
  -e "s|__SPARKBOX_IMAGE__|$image|g" \
  -e "s|__SPARKBOX_NODE_POOL__|$node_pool|g" \
  "$script_dir/device-plugin.yaml" | "${k[@]}" apply -f -
echo "Waiting for the KVM/TUN/loop device plugin..."
"${k[@]}" -n "$namespace" rollout status daemonset/sparkbox-device-plugin --timeout=5m
deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$deadline" ]; do
  kvm_allocatable=$("${k[@]}" get node "$node" -o 'go-template={{index .status.allocatable "sparkbox.dev/kvm"}}')
  tun_allocatable=$("${k[@]}" get node "$node" -o 'go-template={{index .status.allocatable "sparkbox.dev/tun"}}')
  loop_allocatable=$("${k[@]}" get node "$node" -o 'go-template={{index .status.allocatable "sparkbox.dev/loop"}}')
  if [ "$kvm_allocatable" = 1 ] && [ "$tun_allocatable" = 1 ] && [ "$loop_allocatable" = 1 ]; then
    break
  fi
  sleep 2
done
if [ "$kvm_allocatable" != 1 ] || [ "$tun_allocatable" != 1 ] || [ "$loop_allocatable" != 1 ]; then
  echo "device-plugin resources did not become allocatable on $node" >&2
  "${k[@]}" -n "$namespace" logs daemonset/sparkbox-device-plugin >&2 || true
  exit 1
fi

sed \
  -e "s|__SPARKBOX_IMAGE__|$image|g" \
  -e "s|__SPARKBOX_PROXY_DOMAIN__|$proxy_domain|g" \
  -e "s|__SPARKBOX_USERS_HASH__|$users_hash|g" \
  -e "s|__SPARKBOX_GITHUB_APP_CLIENT_ID__|$github_app_client_id|g" \
  -e "s|__SPARKBOX_HIVEMIND_API__|$hivemind_api|g" \
  -e "s|__SPARKBOX_HIVEMIND_SIGNIN_ORGS__|$hivemind_signin_orgs|g" \
  "$script_dir/gateway-deployment.yaml" | "${k[@]}" apply -f -
sed \
  -e "s|__SPARKBOX_IMAGE__|$image|g" \
  -e "s|__SPARKBOX_PROXY_DOMAIN__|$proxy_domain|g" \
  -e "s|__SPARKBOX_NODE_POOL__|$node_pool|g" \
  -e "s|__SPARKBOX_NODE__|$node|g" \
  -e "s|__SPARKBOX_HIVEMIND_API__|$hivemind_api|g" \
  -e "s|__HIVEMIND_MANIFEST__|$hivemind_manifest|g" \
  "$script_dir/deployment.yaml" | "${k[@]}" apply -f -
"${k[@]}" apply -f "$script_dir/service.yaml"

echo "Waiting for the public gateway..."
"${k[@]}" -n "$namespace" rollout status deployment/sparkbox-gateway --timeout=10m
echo "Waiting for the private VM node (first boot may refresh Firecracker and agent tools)..."
"${k[@]}" -n "$namespace" rollout status deployment/sparkbox-node --timeout=20m

# Pin the administrative SSH connection with the same public host key mounted
# into the VM node. If the matching operator private key is available, approve
# a newly enrolled node without an insecure host-key prompt.
# The controller deliberately has no passwd entry for UID 65532. OpenSSH's
# ssh-keygen asks libc for that entry even when it is only reading a public key,
# so perform this read-only fingerprint operation in the root helper container.
# Both containers already mount the same node-identity directory; this does not
# expose a new path or helper RPC to the controller.
node_fingerprint=$("${k[@]}" -n "$namespace" exec deployment/sparkbox-node -c vmm-helper -- \
  ssh-keygen -lf /var/lib/sparkbox/node-identity/node_key.pem -E sha256 | awk '{print $2}')
approval_host="ssh.$allocated_domain"
printf '%s %s\n' "$approval_host" "$(cat "$gateway_public_file")" > "$known_hosts_file"
if [ -f "$private_key" ]; then
  ssh_args=(
    ssh -F /dev/null -o BatchMode=yes -o StrictHostKeyChecking=yes
    -o "UserKnownHostsFile=$known_hosts_file" -i "$private_key"
    -p 22 "ctl@$approval_host"
  )
  deadline=$((SECONDS + 120))
  roster=
  while [ "$SECONDS" -lt "$deadline" ]; do
    roster=$("${ssh_args[@]}" node ls 2>/dev/null || true)
    printf '%s\n' "$roster" | grep -F "$node_fingerprint" >/dev/null && break
    sleep 3
  done
  if ! printf '%s\n' "$roster" | grep -F "$node_fingerprint" >/dev/null; then
    echo "VM node did not enrol with the gateway within 120 seconds" >&2
    exit 1
  fi
  if ! printf '%s\n' "$roster" | grep -F "$node_fingerprint" | grep -F approved >/dev/null; then
    "${ssh_args[@]}" node approve "$node_fingerprint" --guest-subnet 172.30.0.0/20
  fi
else
  echo "operator private key not found at $private_key; approve the pending node manually:" >&2
  echo "  ssh -p 22 ctl@$approval_host node approve $node_fingerprint --guest-subnet 172.30.0.0/20" >&2
fi

"${k[@]}" -n "$namespace" delete deployment sparkbox --ignore-not-found --wait=true

echo
echo "Sparkbox gateway and VM node are ready."
echo "  SSH:  ssh -p 22 ctl@ssh.$proxy_domain help"
echo "  New:  ssh -p 22 new@ssh.$proxy_domain"
echo "  Web:  https://my.$proxy_domain"
echo "  Gateway logs: kubectl --context $context -n $namespace logs -f deployment/sparkbox-gateway"
echo "  VM node logs: kubectl --context $context -n $namespace logs -f deployment/sparkbox-node -c sparkbox-node"
echo "  VMM helper logs: kubectl --context $context -n $namespace logs -f deployment/sparkbox-node -c vmm-helper"
echo "  The VM node init container removed the retired gateway databases, TLS cache, and fleet private keys from its hostPath."
echo "  KVM/TUN are visible only to the narrow VMM helper; the UID 65532 controller has no devices or Linux capabilities."
