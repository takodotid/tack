#!/usr/bin/env bash
# Deletes the Tack end-to-end environment: both kind clusters, the Git server
# container and the Docker network. Safe to re-run. Nothing else is touched.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

for site in "${SITES[@]}"; do
  name=$(cluster_name "$site")
  if kind get clusters 2>/dev/null | grep -qx "$name"; then
    log "deleting kind cluster $name"
    kind delete cluster --name "$name" --kubeconfig "$KUBECONFIG_FILE"
  fi
done

if docker container inspect "$GIT_CONTAINER" >/dev/null 2>&1; then
  log "deleting container $GIT_CONTAINER"
  docker rm -f "$GIT_CONTAINER" >/dev/null
fi

if docker network inspect "$NETWORK" >/dev/null 2>&1; then
  log "deleting network $NETWORK"
  docker network rm "$NETWORK" >/dev/null
fi

log "environment deleted"
