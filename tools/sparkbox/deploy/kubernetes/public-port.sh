#!/usr/bin/env bash
# Prototype arbitrary HTTPS ports by reconciling one port at a time onto the
# existing CKS LoadBalancer Service. Every managed public port targets the
# gateway's single named `https` container port; Sparkbox reads the non-default
# port from the HTTP Host/:authority value after terminating TLS.
set -euo pipefail

context="${SPARKBOX_KUBE_CONTEXT:-}"
namespace="${SPARKBOX_KUBE_NAMESPACE:-sparkbox-poc}"
service="${SPARKBOX_KUBE_SERVICE:-sparkbox}"

usage() {
  cat >&2 <<'EOF'
Usage: public-port.sh [--context CONTEXT] [--namespace NAMESPACE] [--service SERVICE] list
       public-port.sh [--context CONTEXT] [--namespace NAMESPACE] [--service SERVICE] add PORT
       public-port.sh [--context CONTEXT] [--namespace NAMESPACE] [--service SERVICE] remove PORT

Adds or removes a named HTTPS port on the Sparkbox LoadBalancer. Removing a
port is an operator action: first verify that no remaining route uses it.
EOF
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --context)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      context=$2
      shift 2
      ;;
    --namespace)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      namespace=$2
      shift 2
      ;;
    --service)
      [ "$#" -ge 2 ] || { usage; exit 2; }
      service=$2
      shift 2
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      break
      ;;
  esac
done

[ "$#" -ge 1 ] || { usage; exit 2; }
action=$1
shift

k=(kubectl)
if [ -n "$context" ]; then
  k+=(--context "$context")
fi

service_ports() {
  "${k[@]}" -n "$namespace" get service "$service" \
    -o 'jsonpath={range .spec.ports[*]}{.port}{"\t"}{.name}{"\t"}{.targetPort}{"\n"}{end}'
}

if [ "$action" = list ]; then
  [ "$#" -eq 0 ] || { usage; exit 2; }
  printf 'PORT\tNAME\tTARGET\n'
  service_ports | sort -n
  exit 0
fi

case "$action" in
  add|remove) ;;
  *) usage; exit 2 ;;
esac
[ "$#" -eq 1 ] || { usage; exit 2; }
port=$1
case "$port" in
  ''|*[!0-9]*) echo "PORT must be an integer from 1 to 65535" >&2; exit 2 ;;
esac
port=$((10#$port))
if [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
  echo "PORT must be an integer from 1 to 65535" >&2
  exit 2
fi

managed_name="https-$port"
existing=$(service_ports | awk -v port="$port" '$1 == port { print $2 "\t" $3; exit }')

if [ "$action" = add ]; then
  if [ -n "$existing" ]; then
    existing_name=${existing%%$'\t'*}
    existing_target=${existing#*$'\t'}
    if [ "$existing_name" = "$managed_name" ] && [ "$existing_target" = https ]; then
      echo "public HTTPS port $port is already exposed"
      exit 0
    fi
    echo "Service port $port already exists as $existing_name -> $existing_target; refusing to replace it" >&2
    exit 1
  fi
  patch=$(printf '{"spec":{"ports":[{"name":"%s","port":%d,"protocol":"TCP","targetPort":"https"}]}}' \
    "$managed_name" "$port")
  "${k[@]}" -n "$namespace" patch service "$service" --type=strategic --patch "$patch"
  echo "exposed https://<sandbox>.<domain>:$port through the Sparkbox gateway"
  exit 0
fi

if [ -z "$existing" ]; then
  echo "public port $port is not present"
  exit 0
fi
existing_name=${existing%%$'\t'*}
if [ "$existing_name" != "$managed_name" ]; then
  echo "Service port $port is named $existing_name, not $managed_name; refusing to remove it" >&2
  exit 1
fi
patch=$(printf '{"spec":{"ports":[{"port":%d,"$patch":"delete"}]}}' "$port")
"${k[@]}" -n "$namespace" patch service "$service" --type=strategic --patch "$patch"
echo "removed public HTTPS port $port; existing connections may drain at the provider's discretion"
