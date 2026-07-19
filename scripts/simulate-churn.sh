#!/usr/bin/env bash
set -euo pipefail

CONTEXT="${AGONES_CONTEXT:-kind-agones-dev}"
NAMESPACE="${AGONES_NAMESPACE:-default}"
FLEET="${1:-simple-fleet}"

echo "Simulating match churn against fleet '$FLEET' in namespace '$NAMESPACE' (context: $CONTEXT). Ctrl+C to stop."

while true; do
  gs_name=$(kubectl --context "$CONTEXT" create -f - -o jsonpath='{.status.gameServerName}' <<EOF
apiVersion: allocation.agones.dev/v1
kind: GameServerAllocation
metadata:
  namespace: $NAMESPACE
spec:
  selectors:
    - matchLabels:
        agones.dev/fleet: $FLEET
EOF
) || gs_name=""

  if [ -z "$gs_name" ]; then
    echo "$(date +%T) allocation failed (no Ready servers?) - retrying in 10s"
    sleep 10
    continue
  fi

  echo "$(date +%T) match started on $gs_name"
  sleep $(( (RANDOM % 60) + 30 ))
  kubectl --context "$CONTEXT" delete gs "$gs_name" -n "$NAMESPACE" --wait=false >/dev/null
  echo "$(date +%T) match ended, $gs_name torn down"
  sleep $(( (RANDOM % 30) + 15 ))
done
