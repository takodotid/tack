#!/usr/bin/env bash
# Builds the Tack end-to-end environment from nothing. Safe to re-run: each
# step keeps what already exists.
set -euo pipefail

# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

TIMINGS=()
START_ALL=$(date +%s)
PHASE=""
PHASE_START=0

phase() {
  phase_done
  PHASE=$1
  PHASE_START=$(date +%s)
  log "== $PHASE"
}

phase_done() {
  if [[ -n $PHASE ]]; then
    TIMINGS+=("$(printf '%-28s %4ss' "$PHASE" "$(($(date +%s) - PHASE_START))")")
  fi
  PHASE=""
}

preflight() {
  local tool
  for tool in docker kind kubectl flux helm git openssl curl sha256sum; do
    command -v "$tool" >/dev/null || die "missing tool: $tool"
  done
  flux version --client 2>/dev/null | grep -q "flux: $FLUX_VERSION" ||
    die "flux CLI must be $FLUX_VERSION"
  mkdir -p "$WORK/logs" "$WORK/cache"
  (umask 077 && mkdir -p "$WORK/secrets")
}

ensure_network() {
  if ! docker network inspect "$NETWORK" >/dev/null 2>&1; then
    docker network create "$NETWORK" >/dev/null
  fi
}

ensure_git_server() {
  docker build -q -t "$GIT_IMAGE" "$REPO/test/e2e/gitserver" >/dev/null
  local state
  state=$(docker inspect -f '{{.State.Status}}' "$GIT_CONTAINER" 2>/dev/null || true)
  case $state in
    running) ;;
    "")
      docker run -d --name "$GIT_CONTAINER" --network "$NETWORK" \
        --restart unless-stopped -p "127.0.0.1:$GIT_HOST_PORT:8080" "$GIT_IMAGE" \
        -repos "$GIT_REPO" >/dev/null
      ;;
    *) docker start "$GIT_CONTAINER" >/dev/null ;;
  esac
  local hostport
  hostport=$(docker port "$GIT_CONTAINER" 8080/tcp | head -n1)
  GIT_HOST_URL=http://$hostport/$GIT_REPO.git
  local i
  for i in $(seq 1 30); do
    curl -fsS "http://$hostport/healthz" >/dev/null 2>&1 && return 0
    sleep 1
  done
  die "Git server did not answer on $hostport"
}

ensure_kind_clusters() {
  local site pids=() name
  export KIND_EXPERIMENTAL_DOCKER_NETWORK=$NETWORK
  for site in "${SITES[@]}"; do
    name=$(cluster_name "$site")
    if kind get clusters 2>/dev/null | grep -qx "$name"; then
      kind export kubeconfig --name "$name" --kubeconfig "$WORK/kubeconfig-$site" \
        >"$WORK/logs/kind-$site.log" 2>&1
    else
      cat >"$WORK/kind-$site.yaml" <<EOF
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  podSubnet: ${POD_SUBNET[$site]}
  serviceSubnet: ${SERVICE_SUBNET[$site]}
EOF
      kind create cluster --name "$name" --image "$KIND_NODE_IMAGE" \
        --config "$WORK/kind-$site.yaml" --kubeconfig "$WORK/kubeconfig-$site" --wait 3m \
        >"$WORK/logs/kind-$site.log" 2>&1 &
      pids+=("$!:$site")
    fi
  done
  local entry
  for entry in "${pids[@]}"; do
    wait "${entry%%:*}" || {
      cat "$WORK/logs/kind-${entry##*:}.log" >&2
      die "kind create cluster failed for site ${entry##*:}"
    }
  done
  KUBECONFIG="$WORK/kubeconfig-a:$WORK/kubeconfig-b" kubectl config view --flatten \
    >"$WORK/kubeconfig.tmp"
  chmod 600 "$WORK/kubeconfig.tmp"
  mv "$WORK/kubeconfig.tmp" "$KUBECONFIG_FILE"
  for site in "${SITES[@]}"; do
    kc "$site" get nodes -o name >/dev/null
  done
}

# Starts image pulls inside the nodes and returns at once. The kubelet pulls
# anything still missing. A registry pull inside the node is faster than
# `kind load`, which also fails on multi-platform images with the containerd
# image store.
prepull_images() {
  local site image
  for site in "${SITES[@]}"; do
    for image in "${PREPULL_IMAGES[@]}"; do
      docker exec -d "$(cluster_name "$site")-control-plane" crictl pull "$image"
    done
  done
}

install_flux() {
  local site
  for site in "${SITES[@]}"; do
    flux install --kubeconfig "$KUBECONFIG_FILE" --context "$(context_name "$site")" \
      --components "$FLUX_COMPONENTS" --timeout 5m >"$WORK/logs/flux-$site.log" 2>&1 ||
      die "flux install failed for site $site, see $WORK/logs/flux-$site.log"
  done
}

install_cnpg() {
  local manifest=$WORK/cache/cnpg-$CNPG_VERSION.yaml site
  if [[ ! -s $manifest ]] || ! echo "$CNPG_MANIFEST_SHA256  $manifest" | sha256sum -c --status; then
    curl -fsSL -o "$manifest.tmp" "$CNPG_MANIFEST_URL"
    echo "$CNPG_MANIFEST_SHA256  $manifest.tmp" | sha256sum -c --status ||
      die "checksum mismatch for $CNPG_MANIFEST_URL"
    mv "$manifest.tmp" "$manifest"
  fi
  for site in "${SITES[@]}"; do
    kc "$site" apply --server-side --force-conflicts -f "$manifest" >/dev/null
  done
  for site in "${SITES[@]}"; do
    kc "$site" -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout 5m
  done
}

# Secrets are generated once into the work dir and loaded into both clusters.
# Their values are never printed.
generate_secrets() {
  local dir=$WORK/secrets kind
  (
    umask 077
    for kind in server client; do
      if [[ ! -s $dir/$kind-ca.key || ! -s $dir/$kind-ca.crt ]]; then
        openssl ecparam -name prime256v1 -genkey -noout -out "$dir/$kind-ca.key"
        openssl req -x509 -new -key "$dir/$kind-ca.key" -sha256 -days 3650 \
          -subj "/CN=tack-e2e-$kind-ca" \
          -addext "basicConstraints=critical,CA:TRUE" \
          -addext "keyUsage=critical,keyCertSign,cRLSign" \
          -out "$dir/$kind-ca.crt"
      fi
    done
    if [[ ! -s $dir/app-password ]]; then
      openssl rand -hex 24 | tr -d '\n' >"$dir/app-password"
    fi
  )
}

apply_secret() {
  local site=$1
  shift
  kc "$site" -n "$DB_NAMESPACE" create secret "$@" --dry-run=client -o yaml |
    kc "$site" apply --server-side --field-manager tack-e2e-up -f - >/dev/null
}

load_secrets() {
  local dir=$WORK/secrets site kind
  for site in "${SITES[@]}"; do
    kc "$site" create namespace "$DB_NAMESPACE" --dry-run=client -o yaml |
      kc "$site" apply --server-side --field-manager tack-e2e-up -f - >/dev/null
    for kind in server client; do
      apply_secret "$site" generic "tack-e2e-$kind-ca" \
        --from-file=ca.crt="$dir/$kind-ca.crt" --from-file=ca.key="$dir/$kind-ca.key"
    done
    apply_secret "$site" generic app-credentials --type=kubernetes.io/basic-auth \
      --from-literal=username=app --from-file=password="$dir/app-password"
  done
}

push_fixture() {
  local rc=0
  git ls-remote --exit-code --heads "$GIT_HOST_URL" main >/dev/null 2>&1 || rc=$?
  case $rc in
    0)
      log "Git repository already has main, left as it is"
      return 0
      ;;
    2) ;;
    *) die "cannot reach $GIT_HOST_URL (git ls-remote exit $rc)" ;;
  esac
  local seed=$WORK/tmp/seed
  rm -rf "$seed"
  mkdir -p "$seed"
  cp -R "$REPO/test/e2e/fixtures/gitops/." "$seed/"
  git -C "$seed" init -q -b main
  git -C "$seed" add -A
  git -C "$seed" -c user.name=tack-e2e -c user.email=tack-e2e@example.invalid \
    -c commit.gpgsign=false commit -q -m "chore(e2e): seed the GitOps fixture"
  git -C "$seed" push -q "$GIT_HOST_URL" main
  rm -rf "$seed"
}

apply_flux_sync() {
  local site
  for site in "${SITES[@]}"; do
    kc "$site" apply --server-side --field-manager tack-e2e-up -f - >/dev/null <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: tack-e2e
  namespace: flux-system
spec:
  interval: 30s
  url: $GIT_INTERNAL_URL
  ref:
    branch: main
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: site-$site
  namespace: flux-system
spec:
  interval: 30s
  retryInterval: 15s
  timeout: 2m
  path: ./clusters/$site
  prune: true
  wait: false
  sourceRef:
    kind: GitRepository
    name: tack-e2e
EOF
  done
  for site in "${SITES[@]}"; do
    kc "$site" -n flux-system wait gitrepository/tack-e2e --for=condition=Ready --timeout 3m >/dev/null
    kc "$site" -n flux-system wait "kustomization/site-$site" --for=condition=Ready --timeout 5m >/dev/null
  done
}

wait_clusters() {
  local site
  for site in "${SITES[@]}"; do
    kc "$site" -n "$DB_NAMESPACE" wait "cluster/pg-$site" --for=create --timeout 3m >/dev/null
    kc "$site" -n "$DB_NAMESPACE" wait "cluster/pg-$site" --for=condition=Ready --timeout 15m >/dev/null
    log "pg-$site Ready"
  done
}

# psql_at SITE SQL runs a query as postgres in the instance that is the
# primary or designated primary of the site's Cluster.
psql_at() {
  local site=$1 sql=$2 pod
  pod=$(kc "$site" -n "$DB_NAMESPACE" get "cluster/pg-$site" -o jsonpath='{.status.currentPrimary}')
  kc "$site" -n "$DB_NAMESPACE" exec "$pod" -c postgres -- psql -XAtq -c "$sql"
}

verify_replication() {
  local site recovery primaries=0 replica="" i status
  for site in "${SITES[@]}"; do
    recovery=$(psql_at "$site" 'select pg_is_in_recovery()')
    if [[ $recovery == f ]]; then
      primaries=$((primaries + 1))
      log "site $site: pg-$site is the primary"
    else
      replica=$site
    fi
  done
  [[ $primaries -eq 1 ]] || die "expected exactly one primary, found $primaries"
  [[ -n $replica ]] || die "no replica site found"
  for i in $(seq 1 60); do
    status=$(psql_at "$replica" 'select status from pg_stat_wal_receiver')
    [[ $status == streaming ]] && break
    sleep 5
  done
  [[ $status == streaming ]] || die "site $replica is not streaming (status '$status')"
  log "site $replica: pg-$replica streams from" \
    "$(psql_at "$replica" "select sender_host || ':' || sender_port || ' timeline ' || received_tli || ', replayed ' || pg_last_wal_replay_lsn() from pg_stat_wal_receiver")"
}

# Builds Strait's image from its source at the pinned commit, once.
build_strait_image() {
  local src=$WORK/cache/strait
  if [[ ! -d $src/.git ]]; then
    git clone -q "$STRAIT_GIT_URL" "$src"
  fi
  if ! git -C "$src" cat-file -e "$STRAIT_COMMIT^{commit}" 2>/dev/null; then
    git -C "$src" fetch -q origin "$STRAIT_COMMIT"
  fi
  git -C "$src" checkout -q --detach "$STRAIT_COMMIT"
  [[ $(git -C "$src" rev-parse HEAD) == "$STRAIT_COMMIT" ]] ||
    die "Strait checkout is not at $STRAIT_COMMIT"
  docker image inspect "$STRAIT_IMAGE" >/dev/null 2>&1 && return 0
  docker build --build-arg VERSION="${STRAIT_COMMIT:0:12}" -t "$STRAIT_IMAGE" "$src" \
    >"$WORK/logs/strait-image.log" 2>&1 ||
    die "Strait image build failed, see $WORK/logs/strait-image.log"
}

node_ip() {
  docker inspect -f "{{(index .NetworkSettings.Networks \"$NETWORK\").IPAddress}}" \
    "$(cluster_name "$1")-control-plane"
}

other_site() { if [[ $1 == a ]]; then echo b; else echo a; fi; }

# Installs Strait at each site from its Helm chart, and links the sites with
# one direct link over the tack-e2e network. A Site needs a WireGuard
# identity only for wireguard links, so there is none.
install_strait() {
  local site peer arch archive=$WORK/cache/strait-image.tar
  case $(uname -m) in
    x86_64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) die "unsupported architecture $(uname -m)" ;;
  esac
  build_strait_image
  docker save --platform "linux/$arch" -o "$archive" "$STRAIT_IMAGE"
  for site in "${SITES[@]}"; do
    kind load image-archive --name "$(cluster_name "$site")" "$archive" \
      >"$WORK/logs/strait-load-$site.log" 2>&1 ||
      die "loading the Strait image failed for site $site, see $WORK/logs/strait-load-$site.log"
  done
  for site in "${SITES[@]}"; do
    kc "$site" create namespace "$STRAIT_NAMESPACE" --dry-run=client -o yaml |
      kc "$site" apply --server-side --field-manager tack-e2e-up -f - >/dev/null
    kc "$site" label namespace "$STRAIT_NAMESPACE" --overwrite \
      pod-security.kubernetes.io/enforce=privileged >/dev/null
    cat >"$WORK/strait-values-$site.yaml" <<EOF
image: {repository: ${STRAIT_IMAGE%:*}, tag: ${STRAIT_IMAGE##*:}, pullPolicy: Never}
envoy: {image: {repository: ${ENVOY_IMAGE%:*}, tag: ${ENVOY_IMAGE##*:}, pullPolicy: IfNotPresent}}
site:
  name: $site
  spec:
    exchange: {pollInterval: 2s, peerTimeout: 6s}
    gateway: {directService: {type: NodePort}}
EOF
    helm upgrade --install strait "$WORK/cache/strait/deploy/helm/strait" \
      --kubeconfig "$KUBECONFIG_FILE" --kube-context "$(context_name "$site")" \
      --namespace "$STRAIT_NAMESPACE" --values "$WORK/strait-values-$site.yaml" \
      --wait --timeout 5m >"$WORK/logs/strait-$site.log" 2>&1 ||
      die "Strait install failed for site $site, see $WORK/logs/strait-$site.log"
  done
  for site in "${SITES[@]}"; do
    peer=$(other_site "$site")
    kc "$site" apply --server-side --field-manager tack-e2e-up -f - >/dev/null <<EOF
apiVersion: strait.tako.id/v1
kind: SitePeer
metadata:
  name: $peer
spec: {}
---
apiVersion: strait.tako.id/v1
kind: SiteLink
metadata:
  name: $peer-direct
spec:
  peer: $peer
  transport: direct
  cost: 10
  encryption: None
  direct: {address: $(node_ip "$peer")}
EOF
  done
  for site in "${SITES[@]}"; do
    peer=$(other_site "$site")
    kc "$site" wait "sitelink/$peer-direct" --for=jsonpath='{.status.active}'=true --timeout 3m >/dev/null
    kc "$site" wait "sitepeer/$peer" --for=jsonpath='{.status.reachable}'=true --timeout 3m >/dev/null
    log "site $site: Strait reaches site $peer"
  done
}

write_env_file() {
  cat >"$WORK/env" <<EOF
# Generated by test/e2e/env/up.sh. Source it to use the environment.
export KUBECONFIG=$KUBECONFIG_FILE
export TACK_E2E_CONTEXT_A=$(context_name a)
export TACK_E2E_CONTEXT_B=$(context_name b)
export TACK_E2E_GIT_URL=$GIT_HOST_URL
EOF
}

main() {
  phase "preflight"
  preflight
  phase "network and Git server"
  ensure_network
  ensure_git_server
  phase "kind clusters"
  ensure_kind_clusters
  prepull_images
  phase "Flux install"
  install_flux
  phase "CloudNativePG install"
  install_cnpg
  phase "runtime secrets"
  generate_secrets
  load_secrets
  phase "GitOps fixture and sync"
  push_fixture
  apply_flux_sync
  phase "Clusters ready"
  wait_clusters
  phase "replication streaming"
  verify_replication
  phase "Strait install"
  install_strait
  phase_done
  write_env_file

  log "== timings"
  printf '  %s\n' "${TIMINGS[@]}"
  printf '  %-28s %4ss\n' "total" "$(($(date +%s) - START_ALL))"
  log "environment ready: source $WORK/env"
}

main "$@"
