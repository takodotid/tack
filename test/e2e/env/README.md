# End-to-end environment

Two sites, each a single-node kind cluster deployed by Flux from one local Git repository. Each site runs one CloudNativePG `Cluster` in the distributed topology, one read-write PgBouncer `Pooler`, and Strait. Site a is the primary at the start.

## Use

| Command | Effect |
|---|---|
| `test/e2e/env/up.sh` | Builds the environment, or completes a partial one. Safe to re-run. |
| `source .e2e/env` | Sets `KUBECONFIG`, `TACK_E2E_CONTEXT_A`, `TACK_E2E_CONTEXT_B` and `TACK_E2E_GIT_URL`. |
| `test/e2e/env/down.sh` | Deletes the environment. Safe to re-run. |

`TACK_E2E_WORKDIR` overrides the work dir, `.e2e/` at the repo root by default. The work dir is gitignored.

## What `up.sh` builds

| Item | Name | Notes |
|---|---|---|
| Docker network | `tack-e2e` | kind joins it through `KIND_EXPERIMENTAL_DOCKER_NETWORK`. |
| Git server | container `tack-e2e-git` | Smart HTTP with push, built from `test/e2e/gitserver/`. No authentication. |
| GitOps repository | `gitops.git` | Seeded from `test/e2e/fixtures/gitops/` only when the repository has no `main` branch. |
| kind clusters | `tack-e2e-a`, `tack-e2e-b` | Contexts `kind-tack-e2e-a`, `kind-tack-e2e-b`. |
| Images | `PREPULL_IMAGES` in `lib.sh` | Loaded into both nodes from the host's Docker when it has them, so a slow registry does not make the installs time out. The nodes pull the others. `up.sh` never pulls nor removes a host image. |
| Flux | v2.10.0 | source-controller and kustomize-controller only. |
| Flux sync | `GitRepository/tack-e2e`, `Kustomization/site-a` or `site-b` | Namespace `flux-system`. Path `./clusters/<site>`. Interval 30s. |
| CloudNativePG | v1.30.1 | Release manifest, checked against a pinned SHA-256. |
| Runtime Secrets | `tack-e2e-server-ca`, `tack-e2e-client-ca`, `app-credentials` | Namespace `db` at both sites. Generated once into `.e2e/secrets/`. Never in Git. |
| Strait image | `tack-e2e-strait:<commit>` | Built once from Strait's source at `STRAIT_COMMIT` in `lib.sh`, loaded into both nodes from an archive. |
| Strait | Helm release `strait`, namespace `strait-system` | Chart from the same commit. Pod Security `privileged`, for the gateway's `NET_ADMIN`. Exchange polls every 2s, peer timeout 6s. |
| Strait links | `SitePeer` and `SiteLink <peer>-direct` at each site | One direct link to the other node's address on `tack-e2e`. No WireGuard link, so no WireGuard key. |

## Sites

| | Site a | Site b |
|---|---|---|
| Cluster | `db/pg-a` | `db/pg-b` |
| Bootstrap | `initdb` | `pg_basebackup` from `pg-a` |
| Initial `.spec.replica.primary` | `pg-a` | `pg-a` |
| Pooler | `db/pg-a-pooler-rw`, 1 instance | `db/pg-b-pooler-rw`, 0 instances |
| Node container | `tack-e2e-a-control-plane` | `tack-e2e-b-control-plane` |
| Pod subnet | 10.210.0.0/16 | 10.220.0.0/16 |
| Service subnet | 10.211.0.0/16 | 10.221.0.0/16 |
| PostgreSQL NodePort | 30432 | 30432 |

## Cross-site replication

| Concern | How |
|---|---|
| Reach | Each Cluster exposes its `rw` pods on NodePort 30432 through `.spec.managed.services.additional`. The other site connects to the node container by name. Pods resolve the name through CoreDNS and Docker's embedded DNS. |
| Server TLS | Both Clusters use `tack-e2e-server-ca` as `serverCASecret`. CloudNativePG issues the server certificate. `serverAltDNSNames` adds the node container name, so `sslmode=verify-full` works. |
| Client TLS | Both Clusters use `tack-e2e-client-ca` as `clientCASecret`. Each site's `streaming_replica` certificate is accepted at the other site. |
| Credentials | `app-credentials` holds the same `app` password at both sites. |

## Host access

| Endpoint | Where |
|---|---|
| Kubernetes API | `.e2e/kubeconfig`, bound to 127.0.0.1. |
| Git | `TACK_E2E_GIT_URL` in `.e2e/env`: port 28180 on 127.0.0.1, or `TACK_E2E_GIT_PORT`. The port is fixed because a run records the Git URL in its ledger and the e2e restarts the server; it is below the kernel's ephemeral range, so an outgoing connection cannot hold it. |
| Git from pods | `http://tack-e2e-git:8080/gitops.git` |

## Work dir

| Path | Content |
|---|---|
| `.e2e/kubeconfig` | Both contexts. `kubeconfig-a` and `kubeconfig-b` are its sources. |
| `.e2e/env` | Variables for tests. |
| `.e2e/secrets/` | CA keys and the app password. Mode 0600. |
| `.e2e/cache/` | The CloudNativePG manifest, Strait's source and image archive. |
| `.e2e/logs/` | Output of the kind, Flux and Strait installs. |

`down.sh` deletes only the two kind clusters, the `tack-e2e-git` container and the `tack-e2e` network. It leaves the work dir and the `tack-e2e-gitserver:dev` and `tack-e2e-strait` images.
