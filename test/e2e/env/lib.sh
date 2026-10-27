# Shared names and helpers for up.sh and down.sh. Source it, do not run it.

ENV_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$ENV_DIR/../../.." && pwd)
WORK=${TACK_E2E_WORKDIR:-$REPO/.e2e}
KUBECONFIG_FILE=$WORK/kubeconfig

NETWORK=tack-e2e
SITES=(a b)
GIT_CONTAINER=tack-e2e-git
GIT_IMAGE=tack-e2e-gitserver:dev
GIT_REPO=gitops
GIT_INTERNAL_URL=http://$GIT_CONTAINER:8080/$GIT_REPO.git
# A fixed host port: a run records the Git URL in its ledger, and the e2e
# stops and starts the Git server.
GIT_HOST_PORT=${TACK_E2E_GIT_PORT:-38180}

KIND_NODE_IMAGE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
FLUX_VERSION=v2.10.0
FLUX_COMPONENTS=source-controller,kustomize-controller
CNPG_VERSION=1.30.1
CNPG_MANIFEST_URL=https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml
CNPG_MANIFEST_SHA256=37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb

# Images pulled inside both nodes in the background while the controllers
# install, so that pods start sooner.
PREPULL_IMAGES=(
  ghcr.io/cloudnative-pg/cloudnative-pg:1.30.1
  ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie
  ghcr.io/cloudnative-pg/pgbouncer:1.25.2
  ghcr.io/fluxcd/source-controller:v1.10.1
  ghcr.io/fluxcd/kustomize-controller:v1.10.1
)

DB_NAMESPACE=db

cluster_name() { echo "tack-e2e-$1"; }
context_name() { echo "kind-tack-e2e-$1"; }

# kc SITE ARGS... runs kubectl against one site, always with an explicit
# kubeconfig and context.
kc() {
  local site=$1
  shift
  kubectl --kubeconfig "$KUBECONFIG_FILE" --context "$(context_name "$site")" "$@"
}

log() { printf '%s %s\n' "$(date -u +%H:%M:%S)" "$*"; }
die() {
  log "ERROR: $*" >&2
  exit 1
}
