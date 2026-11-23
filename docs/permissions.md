# Permissions

Tack uses the operator's credentials for each site's kube context. Every site needs the same rows: any site can be `from` or `to`.

| # | Resource | Verbs | Namespace | Commands | For |
|---|---|---|---|---|---|
| 1 | non-resource URL `/readyz` | get | | `plan`, `run`, `resume`, `status -f`, `abandon -f`, `prune` | the API check ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 5) |
| 2 | non-resource URLs `/api`, `/apis` and below | get | | all | API discovery |
| 3 | `namespaces`, name `kube-system` | get | | `plan`, `run`, `resume` | the site's cluster ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 19) |
| 4 | `sitefailovers` (`failover.tako.id`) | list | the ledger's | all | the search for runs |
| 5 | `sitefailovers` | get, create | the ledger's | `run`, `resume`; `get` also `abandon` | the ledger: dry-run create, create at `to`, reading a run |
| 6 | `sitefailovers/status` | update | the ledger's | `run`, `resume`, `abandon` | every step; the mark on each run taken over; the mark of `tack abandon` |
| 7 | `clusters`, `poolers` (`postgresql.cnpg.io`) named in the Workload | get | the member's | `plan`, `run`, `resume`; `prune` for a run with a follow-up `Rebuild` | reading each site, owner labels, `Wait`, `Capture`, checks |
| 8 | the same | update, patch | the member's | `run`, `resume` | `update`: hold annotations. `patch`: Failover steps, and the server-side dry-run apply before release |
| 9 | `deployments` (`apps`) named after each Pooler | get | the Pooler's | `run`, `resume` | the Pooler waits |
| 10 | `kustomizations` (`kustomize.toolkit.fluxcd.io`) that apply the objects of rows 7 and 8 | get, patch | Flux's | `run`, `resume` | the reconcile request (an annotation), the waits, the render |
| 11 | `gitrepositories` (`source.toolkit.fluxcd.io`) of those Kustomizations | get, patch | Flux's | `run`, `resume` | the reconcile request; the render reads `spec.ignore`, `spec.include` |
| 12 | `configmaps`, `secrets` named in those Kustomizations' `spec.postBuild.substituteFrom` | get | the Kustomization's | `run`, `resume` | the render's variables |
| 13 | `siteservices` (`strait.tako.id`) named in `spec.routing` | get | the SiteService's | `run`, `resume` | the routing check |
| 14 | `events` | create | the ledger's | `run`, `resume`, `abandon` | the ledger's Events. Optional: a run goes on without it and says so once ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 23) |
| 15 | `selfsubjectreviews` (`authentication.k8s.io`) | create | | `run`, `resume`, `abandon` | the user in the ledger's history. Optional: without it the history names no user, and the run says so once |
| 16 | `sitefailovers` | delete | the ledger's | `prune` | deleting old ledgers ([Old ledgers](old-ledgers.md)) |
| 17 | `leases` (`coordination.k8s.io`) | get, create, update | the ledger's | `run`, `resume`; `get` also `abandon` | the run's Lease ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 28). Optional: without it a run holds no Lease and says so once |
| 18 | `pods`, `pods/proxy` | get | the member's | `plan`, `run`, `resume` | each site's WAL position: the primary's `/pg/status` (port 8000) through the API server, a replica's `/metrics` (port 9187) ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 13). Optional: without it a plan says it could not read the position, and a site-loss run records no fork point |
| 19 | `backups` (`postgresql.cnpg.io`) | get, create | the member's | `run`, `resume` | the old primary's snapshot after a site loss, when the Workload sets `cnpg.snapshot` ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 15) |
| 20 | `objectstores` (`barmancloud.cnpg.io`) named by the Clusters' barman-cloud plugin | get | the member's | `plan`, `run` | checking that each site archives to its own destination ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 16). Optional: without it the plan warns it could not check |

| Fact | Detail |
|---|---|
| A read-only role | Rows 1 to 4 and the `get` of row 7 serve `tack plan` and `tack status`. |
| Row 16 | Only `tack prune` needs it. The chart grants it only with `rbac.prune`. |
| Rows 1, 2 and 15 | Kubernetes grants them to every authenticated user (`system:public-info-viewer`, `system:discovery`, `system:basic-user`). Without those bindings, grant them. |
| `resourceNames` | Rows 3, 7 to 13 and 20 can name their objects. Rows 4 to 6 cannot: run names are generated, and `list` and `create` take no name. |
| Row 19 | It cannot name its Backups: their names include the run's name. |
| Row 18 | It cannot name its Pods: CloudNativePG generates their names. `get` on `pods/proxy` reaches any port of any Pod in the namespace, including unauthenticated endpoints. Grant it only in the members' namespaces. |
| Row 17 | Without it, `tack abandon` and `tack run --take-over` judge the run by its ledger and refuse for longer after a killed process ([Interrupting a run](operations.md#interrupting-a-run)). |

Each runbook names the extra access its `kubectl` commands need.

## The chart's roles

With `rbac.create`, the chart creates roles that grant exactly the rows above, and row 16 only with `rbac.prune` too ([RFC#0009](../rfcs/RFC0009.md#the-chart), Requirement 29).

| Role | Kind, namespace | Rows |
|---|---|---|
| `<release>-cluster` | ClusterRole | 1, 2, 3 and 15 |
| `<release>-ledger` | Role, the release's namespace (install the chart in the ledger namespace) | 4, 5, 6, 14 and 17; 16 with `rbac.prune` |
| `<release>-objects` | Role, each namespace of `rbac.objects` | 7 to 13 and 20, for the objects that entry names; 18 and 19 where the entry names `clusters` |

| Value | Default | Meaning |
|---|---|---|
| `rbac.create` | `false` | Create the roles. |
| `rbac.prune` | `false` | Also grant row 16. |
| `rbac.subjects` | `[]` | Bind each role to these subjects: a ClusterRoleBinding, and a RoleBinding next to each Role. |
| `rbac.objects` | `[]` | Per namespace, the names of the Workload files' objects: `clusters`, `poolers`, `kustomizations`, `gitRepositories`, `configMaps`, `secrets`, `siteServices`, `objectStores` (the barman-cloud ObjectStores the Clusters archive to). Each list is optional; a kind without names gets no rule. `poolers` also grants `get` on the Deployment of the same name (row 9). The chart fails on an entry without a namespace or names, and on a namespace listed twice. Each namespace must exist before the install. |

```sh
helm --kube-context "$ctx" upgrade --install tack oci://ghcr.io/takodotid/charts/tack --version "${VERSION#v}" \
  --namespace tack-system --create-namespace -f rbac.yaml
```

```yaml
# rbac.yaml
rbac:
  create: true
  subjects:
  - {apiGroup: rbac.authorization.k8s.io, kind: Group, name: dba-oncall}
  objects:
  - namespace: db
    clusters: [pg-a, pg-b]
    poolers: [pg-a-pooler-rw, pg-b-pooler-rw]
  - namespace: flux-system
    kustomizations: [apps]
    gitRepositories: [gitops]
```

One values file can name every site's objects and serve all sites.

## Who may write ledgers

A ledger holds the steps, kube contexts and Git URL that `tack resume` uses with the operator's credentials, so writing one is privileged. Each search for runs selects ledgers by the label `failover.tako.id/workload`, and what it finds decides whether a run starts or resumes ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 8 and 18).

| Access | What it lets its holder do |
|---|---|
| `create` on `sitefailovers` | plant a run for an operator to resume |
| `update`, `patch` on `sitefailovers/status` | change a run's state, captured values, holds and follow-ups |
| `update`, `patch` on `sitefailovers` | change a ledger's labels. A run without its workload's label drops out of every search, so a second run starts or an older run resumes while it is not done |
| `delete`, `deletecollection` on `sitefailovers` | remove a run's record, with the same effect |

| Rule | Why |
|---|---|
| Grant rows 5 and 6 to the operators only, and row 16 only to those who prune, in the ledger namespace at every site. | Tack needs no other write. |
| Keep the ledger namespace out of every Flux path, and `SiteFailover` objects out of Git. | A Kustomization applies Git as kustomize-controller's service account, or the one its `spec.serviceAccountName` names. |
| Run the check below at every site: before the first run, and after each change of who is bound to which role. | A role with a wildcard resource or verb, such as `cluster-admin`, includes every access above. RBAC only adds access; neither the chart nor Tack can remove it. |

As a cluster admin (`--as` needs `impersonate`), list who is bound, then ask for each subject that is not an operator, such as a GitOps controller's or CI system's ServiceAccount. Every answer must be `no`:

```sh
ns=tack-system   # the ledger namespace
kubectl --context "$ctx" get clusterrolebindings -o wide
kubectl --context "$ctx" -n "$ns" get rolebindings -o wide
as=system:serviceaccount:flux-system:kustomize-controller   # repeat for each such subject
for verb in create update patch delete deletecollection; do
  echo "$verb: $(kubectl --context "$ctx" -n "$ns" auth can-i "$verb" sitefailovers.failover.tako.id --as "$as")"
done
for verb in update patch; do
  echo "$verb status: $(kubectl --context "$ctx" -n "$ns" auth can-i "$verb" sitefailovers.failover.tako.id --subresource=status --as "$as")"
done
```

For a group, pass any user name and the group: `--as someone --as-group GROUP`.

What `tack resume` checks in a ledger:

| Part of the ledger | Checked |
|---|---|
| `spec.steps` | Before the confirmation, they must equal the plan the driver composes from the ledger's Workload and request ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 21). At the Git step, each change's file and path must be where the Workload's mapping or the object's manifest puts it ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 15). |
| `spec.workload`: contexts, Clusters, Git URL | No: a planted Workload composes its own plan. The confirmation prints each site's context, API server, recorded cluster and the Git URL; the operator compares them with the Workload file. |
| `status`: captured values, Git revisions, step states, holds, follow-ups | No. |

## Git access

The operator's `git` configuration needs read access for `tack plan` and push access to the branch for `tack run`. Tack pushes without force and skips client-side hooks; server-side rules still apply ([RFC#0006](../rfcs/RFC0006.md#requirements)).
