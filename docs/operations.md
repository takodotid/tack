# Operations

## Reading a run

| Command | Shows |
|---|---|
| `tack status -f workload.yaml` | the workload's runs at every reachable site: site, run, mode, from, to, state, reason, start |
| `tack status RUN -f workload.yaml` | one run: state and reason, message, Git base and commit, captured values by name, held objects, each step with its state and last result, follow-ups |
| `tack status [RUN] --context site-b` | the same from one context, without the Workload file; `--namespace` when the ledger namespace is not `tack-system` |
| `tack status RUN -f workload.yaml -o yaml` | the whole `SiteFailover` object |
| `kubectl --context site-b -n tack-system get sfo` | the ledgers at one site, with workload, mode, from, to, state and phase |

A run's state is `Running`, `Waiting`, `Stopped` or `Succeeded`; each step's state is `Pending`, `Running`, `Succeeded`, `Failed` or `Deferred` ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 9 and 11). A run left `Running` by a process that ended, such as a killed terminal, goes on with `tack resume`. A run shown as `NotStarted` never recorded its start: its process ended between creating the ledger and its first write, so none of its steps ran. It cannot resume; plan again and start a new run with `--take-over RUN`.

A run that stopped because the Git branch moved, abbreviated:

```text
$ tack status -f workload.yaml
SITE  RUN                      MODE     FROM  TO  STATE    REASON      STARTED
b     pg-main-20261027-101500  Planned  a     b   Stopped  GitChanged  2026-10-27 10:15:00Z

$ tack status pg-main-20261027-101500 -f workload.yaml
Run tack-system/pg-main-20261027-101500: workload pg-main, Planned, site a to site b
State: Stopped (GitChanged)
Message: step git-commit: branch main of ssh://***@git.example.com/gitops.git is at a2c8b964f945fe157a56c4f87f8b0b2a3176e7a3, but the run's base is 6dd30f74794e7c2d1ef0405918b9b92bb00a4a11: Git changed during the run
Started: 2026-10-27 10:15:00Z
Git: base main@sha1:6dd30f74794e7c2d1ef0405918b9b92bb00a4a11, commit none
Captured: demotionToken
Held objects:
  site a: Cluster db/pg-a (held)
  site a: Pooler db/pg-a-pooler-rw (held)
  site b: Cluster db/pg-b (held)
  site b: Pooler db/pg-b-pooler-rw (held)
...
PromoteToGitOps
  18. [Failed] git-commit  Git
      commit the live state to ssh://***@git.example.com/gitops.git branch main
...
        result: branch main of ssh://***@git.example.com/gitops.git is at a2c8b964f945fe157a56c4f87f8b0b2a3176e7a3, but the run's base is 6dd30f74794e7c2d1ef0405918b9b92bb00a4a11: Git changed during the run
  19. [Pending] reconcile-git-a  site a
...
```

Here the switch is done live and the objects are still held. Flux applies nothing to them until the run releases them.

## Stopped and waiting runs

`tack run` and `tack resume` exit with code 3 when the run stops for the operator, and 4 when it waits for something outside it. The ledger's reason says why. Each reason and the operator's way on are in the table of [RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 10.

| Runbook | For |
|---|---|
| [runbooks/stop-reasons.md](runbooks/stop-reasons.md) | each reason: what stopped, what to check, the way on |
| [runbooks/no-primary-after-demotion.md](runbooks/no-primary-after-demotion.md) | a planned run that stopped after its demotion and before its promotion: no site takes writes |
| [runbooks/abandon-run.md](runbooks/abandon-run.md) | a run that must not go on, and holds left behind |

Most reasons go on with a plain `tack resume RUN -f workload.yaml` once the cause is fixed. Three decisions have their own flag:

| Flag | When | Example |
|---|---|---|
| `--accept-git-head SHA` | The run stopped with `GitChanged`; or with `GitDiffersFromLive` once Git is fixed, or `FluxChanged` for an expected change ([runbooks/stop-reasons.md](runbooks/stop-reasons.md)). | Review the commits between the base and the new head named in the message, such as `git log --oneline 6dd30f7..a2c8b96`, then `tack resume RUN -f workload.yaml --accept-git-head a2c8b96`. Before the run's commit, the accepted head becomes its base. After it, the release checks Git against the accepted head, which must contain the run's commit. |
| `--fence SITE` | The run stopped with `FencedSiteIsPrimary`. | A lost site came back and answers as primary. Check it, then `tack resume RUN -f workload.yaml --fence a` declares the new primary there. |
| `--take-over RUN` | Another run of the workload can no longer finish, such as when its ledger is at a lost site. Without the flag, the new run is refused while that run is listed and not `Succeeded`, or stops on its holds. | Pass it when starting the new run: `tack run -f workload.yaml --to b --mode site-loss --take-over RUN` takes over the holds of `RUN`, and records it in its ledger. `RUN` can then never resume. |

A hold set outside Tack, which stops a run with `HeldByAnotherRun`, stays until the operator removes it: Tack never removes a hold it did not set ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)).

After `--accept-git-head`, the check before release reports differences outside the run's own fields instead of stopping, and the check after release does not stop on a changed generation ([RFC#0005](../rfcs/RFC0005.md#requirements), Requirements 8 and 10).

`tack resume` refuses a run that another run took over, or that another run of the workload started after: its plan may no longer match the sites. Resume or check the newer run instead. It refuses before it asks for the workload's name, with exit code 1, and searches again after the name is typed.

`tack resume` asks for the workload's name like `tack run`. `--yes` skips that, only for a resume a human approved.

## Break-glass

In a site loss, Git may be down too. Tack then switches live and commits later. The sequence is in [RFC#0007](../rfcs/RFC0007.md#design).

| Step | Command | Result |
|---|---|---|
| 1 | `tack plan -f workload.yaml --to b --mode site-loss` | Works with Git down. The notes say the Git step waits for Git, and fields found by `find` are resolved then. |
| 2 | `tack run -f workload.yaml --to b --mode site-loss` | Holds and promotes at b; the steps at the lost site are `Deferred`. Exit code 4, state `Waiting`, reason `GitUnreachable`. |
| 3 | `tack resume RUN -f workload.yaml` once Git answers | Commits, checks Git against live, releases. The run ends `Succeeded`, with the follow-up `Rebuild` for the lost site. |

While the run waits, the held objects stay held: Flux applies nothing to them, including unrelated Git changes. A planned run never starts with Git unreachable.

When the lost site answers again, `tack resume` runs its deferred steps: hold, then fence. If the site answers as primary, the run stops with `FencedSiteIsPrimary` first and changes nothing there until the operator passes `--fence`. The steps stay as they were, `Deferred` for a lost site. `--fence` holds for the resume it is passed to: each `tack run` and `tack resume` reads the site again before its first step there, and stops again while the site answers as primary. A run that already succeeded with deferred steps runs them on a later `tack resume`. If the site comes back after the commit and no resume runs, Flux at that site applies Git, which already declares the new primary. A later resume holds and releases the site at the run's commit; a later commit that Flux applied there stops it with `GitChanged` until the operator accepts that head.

## After a site loss

The old primary may hold writes the new one lacks, and cannot follow it. Rebuild it with [docs/runbooks/rebuild-old-primary.md](runbooks/rebuild-old-primary.md). Until it is rebuilt, `tack plan` refuses a planned switchover back to it.

## Permissions

Tack acts with the operator's credentials for each site's kube context. Every site needs the same rows: any site can be `from` or `to`.

| # | Resource | Verbs | Namespace | Commands | For |
|---|---|---|---|---|---|
| 1 | non-resource URL `/readyz` | get | | `plan`, `run`, `resume`, `status -f` | the check of a site's API ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 5) |
| 2 | non-resource URLs `/api`, `/apis` and below | get | | all | API discovery: the client maps each kind to its resource |
| 3 | `namespaces`, name `kube-system` | get | | `plan`, `run`, `resume` | the site's cluster ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 19) |
| 4 | `sitefailovers` (`failover.tako.id`) | list | the ledger's | all | the search for runs |
| 5 | `sitefailovers` | get, create | the ledger's | `run`, `resume` | the ledger: a dry-run create, then the create at `to`; reading a run, and a ledger that holds the run's name |
| 6 | `sitefailovers/status` | update | the ledger's | `run`, `resume` | every step; with `--take-over`, the mark on the run taken over, at that run's site |
| 7 | `clusters`, `poolers` (`postgresql.cnpg.io`) named in the Workload | get | the member's | `plan`, `run`, `resume` | reading each site, the owner labels, `Wait`, `Capture` and the checks |
| 8 | the same | update, patch | the member's | `run`, `resume` | `update`: adding and removing the hold annotations. `patch`: the Failover steps, and the server-side dry-run apply of the check before release |
| 9 | `deployments` (`apps`) named after each Pooler | get | the Pooler's | `run`, `resume` | the Pooler waits |
| 10 | `kustomizations` (`kustomize.toolkit.fluxcd.io`) that apply the objects of rows 7 and 8 | get, patch | Flux's | `run`, `resume` | the reconcile request, an annotation; the waits; the render |
| 11 | `gitrepositories` (`source.toolkit.fluxcd.io`) of those Kustomizations | get, patch | Flux's | `run`, `resume` | the reconcile request; the render reads `spec.ignore` and `spec.include` |
| 12 | `configmaps`, `secrets` named in those Kustomizations' `spec.postBuild.substituteFrom` | get | the Kustomization's | `run`, `resume` | the render's variables |
| 13 | `siteservices` (`strait.tako.id`) named in `spec.routing` | get | the SiteService's | `run`, `resume` | the routing check |

| Fact | Detail |
|---|---|
| A read-only role | Rows 1 to 4 and the `get` of row 7 are enough for `tack plan` and `tack status`. |
| Rows 1 and 2 | Kubernetes grants them to every authenticated user through the default ClusterRoleBindings `system:public-info-viewer` and `system:discovery`. A cluster without those bindings must grant them. |
| `resourceNames` | Rows 3 and 7 to 13 can name their objects. Rows 4 to 6 cannot: run names are generated, and `list` and `create` take no name. |
| Checked | On the e2e's kind clusters, a ServiceAccount with exactly these rows, each named where the table allows, ran `tack plan`, `tack status`, a planned run with the routing check and a Kustomization with `postBuild`, a resume, a run with `--take-over`, and a site-loss run. Without row 3, `tack plan` fails with `namespaces "kube-system" is forbidden`; without the ConfigMap of row 12, the check before release stops with `StepFailed`. |

The runbooks' own `kubectl` commands need more, such as `patch` on `sitefailovers/status`; each runbook says so.

For Git, the operator's `git` configuration needs read access for `tack plan`, and push access to the branch for `tack run`. Tack pushes without force and skips client-side hooks; server-side rules still apply ([RFC#0006](../rfcs/RFC0006.md#requirements)).
