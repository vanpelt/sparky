#!/usr/bin/env bash
# Tear down a sparkbox host and stop the meter — the inverse of launch-host.sh.
#
# Crucially, it DETACHES any reserved (flexible) IPs from the server *before*
# deleting it, leaving them cleanly unattached and ready to reattach to the next
# box. (Deleting a server does NOT auto-detach its flexible IPs — they stay
# "attached" to the now-dead server id and get orphaned, which is how the
# original sparkbox-1 v6 /64 stranded itself.) The flexible IPs are kept, not
# released, since they carry your DNS — pass DELETE_FLEXIBLE_IPS=1 to release.
#
# Usage:
#   SERVER_ID=<id> ./teardown-host.sh      # explicit server
#   NAME=sparkbox-3 ./teardown-host.sh     # by name
#   ./teardown-host.sh                      # auto-detect the single sparkbox box
#
#   KEEP_ATTACHED=1     ./teardown-host.sh  # skip detaching (they'll orphan)
#   DELETE_FLEXIBLE_IPS=1 ./teardown-host.sh # also release the flexible IPs
set -euo pipefail
ZONE=${ZONE:-fr-par-1}

# Resolve the target server id.
if [ -n "${SERVER_ID:-}" ]; then
  SRV="$SERVER_ID"
elif [ -n "${NAME:-}" ]; then
  SRV=$(scw baremetal server list zone="$ZONE" -o json \
    | NAME="$NAME" python3 -c 'import json,os,sys
m=[s["id"] for s in json.load(sys.stdin) if s["name"]==os.environ["NAME"]]
print(m[0] if m else "")')
  [ -n "$SRV" ] || { echo "no server named $NAME in $ZONE"; exit 1; }
else
  # Auto-detect: only safe when there is exactly one server.
  mapfile -t _srvs < <(scw baremetal server list zone="$ZONE" -o json \
    | python3 -c 'import json,sys; [print(s["id"], s["name"]) for s in json.load(sys.stdin)]')
  [ "${#_srvs[@]}" -eq 1 ] || { echo "expected exactly one server, found ${#_srvs[@]}; set SERVER_ID or NAME:"; printf '  %s\n' "${_srvs[@]}"; exit 1; }
  SRV=${_srvs[0]%% *}
  echo "auto-detected server: ${_srvs[0]}"
fi

echo "== flexible IPs attached to $SRV =="
mapfile -t FIPS < <(scw fip ip list zone="$ZONE" -o json \
  | SRV="$SRV" python3 -c 'import json,os,sys
for f in json.load(sys.stdin):
    if f.get("server_id")==os.environ["SRV"]: print(f["id"], f["ip_address"])')
if [ "${#FIPS[@]}" -eq 0 ]; then
  echo "   (none)"
else
  printf '   %s\n' "${FIPS[@]}"
fi

# Detach (default) so the reserved IPs don't orphan onto the dead server id.
if [ "${#FIPS[@]}" -gt 0 ] && [ -z "${KEEP_ATTACHED:-}" ]; then
  for entry in "${FIPS[@]}"; do
    id=${entry%% *}
    echo "== detaching ${entry#* } ($id) =="
    scw fip ip detach fips-ids.0="$id" zone="$ZONE" >/dev/null
    if [ -n "${DELETE_FLEXIBLE_IPS:-}" ]; then
      echo "   releasing $id (DELETE_FLEXIBLE_IPS=1)"
      scw fip ip delete "$id" zone="$ZONE" >/dev/null
    fi
  done
fi

echo "== deleting server $SRV — billing stops =="
scw baremetal server delete "$SRV" zone="$ZONE" >/dev/null
echo "== done =="
if [ "${#FIPS[@]}" -gt 0 ] && [ -z "${DELETE_FLEXIBLE_IPS:-}" ] && [ -z "${KEEP_ATTACHED:-}" ]; then
  echo "   flexible IPs kept + detached; reattach on the next launch with:"
  ids=$(IFS=,; echo "${FIPS[*]%% *}")
  echo "   FLEXIBLE_FIP_IDS=$ids"
fi
