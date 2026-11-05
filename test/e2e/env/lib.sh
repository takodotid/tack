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
# stops and starts the Git server. It is below the kernel's ephemeral port
# range, so an outgoing connection cannot hold it when the server restarts.
GIT_HOST_PORT=${TACK_E2E_GIT_PORT:-28180}

# Every image that the e2e pulls is named by the digest of its index, so a
# tag that moves changes nothing ([RFC#0009] Requirement 25). The fixtures
# name the same images.
KIND_NODE_IMAGE=kindest/node:v1.37.0@sha256:a1ed56cfb0e7b93589bdf97c8cd566405a265939e3620fc4f5de89adff580ae5
FLUX_VERSION=v2.10.0
FLUX_COMPONENTS=source-controller,kustomize-controller
FLUX_SOURCE_IMAGE=ghcr.io/fluxcd/source-controller:v1.10.1@sha256:611b6bca3eae95bcff471414d1fb14a4ab83d7f14e609cccef2bd389c121f37e
FLUX_KUSTOMIZE_IMAGE=ghcr.io/fluxcd/kustomize-controller:v1.10.1@sha256:37db29b25dae35b881a5e064acedb0cc954442da453a39eaa0eab7a90dbecc4e
CNPG_VERSION=1.30.1
CNPG_MANIFEST_URL=https://github.com/cloudnative-pg/cloudnative-pg/releases/download/v$CNPG_VERSION/cnpg-$CNPG_VERSION.yaml
CNPG_MANIFEST_SHA256=37237f145d8138256ea25ae830f87759255665ff08f8d552fdd8224a5ec032fb
CNPG_IMAGE=ghcr.io/cloudnative-pg/cloudnative-pg:$CNPG_VERSION@sha256:923c267ec29636db3bee20f993d0ec4973fa22998e1adad37da79e4d32b5bc07
POSTGRES_IMAGE=ghcr.io/cloudnative-pg/postgresql:18.6-system-trixie@sha256:44d17e8570dc25055376148a2918840def5bef0193969be3e79101afa25eafc2
PGBOUNCER_IMAGE=ghcr.io/cloudnative-pg/pgbouncer:1.25.2@sha256:b15c428057d1ffbc19fb70496911a5f1a47066dcc1315bf794f4e85ade29e2e5

# Strait, built from its source at a pinned commit, fetched by its SHA. The
# image is loaded into the nodes from an archive; Envoy is pulled in them.
STRAIT_GIT_URL=https://github.com/takodotid/strait.git
STRAIT_COMMIT=036aa99e8171ec55e37d2a02d1e66363c3c1907e
STRAIT_IMAGE=tack-e2e-strait:${STRAIT_COMMIT:0:12}
STRAIT_NAMESPACE=strait-system
ENVOY_REPOSITORY=envoyproxy/envoy
ENVOY_TAG=distroless-v1.40.2@sha256:09ca6f2140a61ad8df11779a4a30d52b48433d444466328494f9188c9c35eded
ENVOY_IMAGE=$ENVOY_REPOSITORY:$ENVOY_TAG

# Images pulled inside both nodes, by digest, in the background while the
# controllers install, so that pods start sooner.
PREPULL_IMAGES=(
  "$CNPG_IMAGE"
  "$POSTGRES_IMAGE"
  "$PGBOUNCER_IMAGE"
  "$FLUX_SOURCE_IMAGE"
  "$FLUX_KUSTOMIZE_IMAGE"
  "$ENVOY_IMAGE"
)

DB_NAMESPACE=db

# Pod and Service subnets per site. Strait needs them distinct between sites.
declare -A POD_SUBNET=([a]=10.210.0.0/16 [b]=10.220.0.0/16)
declare -A SERVICE_SUBNET=([a]=10.211.0.0/16 [b]=10.221.0.0/16)

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
