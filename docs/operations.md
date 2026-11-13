# Operations

## Reading a run

| Command | Shows |
|---|---|
| `tack status -f workload.yaml` | the workload's runs at every reachable site: site, run, mode, from, to, state, reason, start. The state is `Abandoned` for an abandoned run, and `TakenOver` for a run another run took over. After the list, each site that gave no answer, with its error: its runs are not listed, and the command exits with code 1. With `-o yaml`, those lines follow the ledgers as YAML comments |
| `tack status RUN -f workload.yaml` | one run: state and reason, message, the version of the `tack` that last wrote it, Git base and commit, captured values by name, held objects as the ledger records them, each phase and step with its state, start, end, attempts and last result, follow-ups, and the history: each start, stop, wait and success of the run, with the version, the user and the flags of the `tack` that recorded it |
| `tack status [RUN] --context site-b` | the same from one context, without the Workload file; `--namespace` when the ledger namespace is not `tack-system` |
| `tack status RUN -f workload.yaml -o yaml` | the whole `SiteFailover` object |
| `tack abandon RUN -f workload.yaml --reason TEXT` | marks a run that must never go on: [runbooks/abandon-run.md](runbooks/abandon-run.md) |
| `tack prune -f workload.yaml --keep N` | lists the workload's runs, and deletes the ledgers of old runs that succeeded: [Old ledgers](#old-ledgers) |
| `kubectl --context site-b -n tack-system get sfo` | the ledgers at one site, with workload, mode, from, to, state, reason, how long ago each was abandoned (empty otherwise), phase, age, and the run that took each over (empty otherwise). The State column of an abandoned or taken-over run keeps its last state, such as `Stopped`: `status.abandoned` and `status.supersededBy` mark it apart from `status.state`. The Abandoned column comes with the CRD of v1.4.0, the TakenOverBy column with the CRD of the release after v1.5.0 ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 15). A run whose take-over mark did not land, because its site gave no answer then, shows an empty TakenOverBy; `tack status` lists it as `TakenOver` |
| `kubectl --context site-b -n tack-system get sfo -o custom-columns=NAME:.metadata.name,STATE:.status.state,REASON:.status.reason,ABANDONED:.status.abandoned.at,TAKEN-OVER-BY:.status.supersededBy` | the ledgers at one site, with the time each was abandoned and the run that took each over, `<none>` otherwise, with a CRD of any v1.3.0 or later release |
| `kubectl --context site-b -n tack-system describe sfo RUN` | also the ledger's Events: each step's start and end, and each change of the run's state, for as long as the API server keeps Events (1 h by default) |

Every `tack` command starts each progress line, such as a step or a retried check, with its time in UTC (`2026-10-27T10:15:00Z`), so a terminal's log lines up with the ledger and the API servers' audit logs ([RFC#0009](../rfcs/RFC0009.md#progress-and-status), Requirement 27). A read checks all its sites at once; the lines of its checks come grouped by site, in the order of the Workload's sites, each with the time it was printed, so a later site's lines can carry earlier times ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 10). Before those checks, Tack obtains each site's credentials, one site at a time in the same order, with no time limit: a credential plugin that prompts on the terminal, such as an OIDC login, prompts alone, and its wait takes nothing of a check. A line `waiting   for the credentials of site S` says when a site's take more than 2 s. A site whose credentials cannot be obtained is not counted as lost: the command stops ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 12). The history in `status.history` keeps at most 100 entries: the first, and the newest ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 22).

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
Held objects, as the ledger records them:
  site a: Cluster db/pg-a (held)
  site a: Pooler db/pg-a-pooler-rw (held)
  site b: Cluster db/pg-b (held)
  site b: Pooler db/pg-b-pooler-rw (held)
...
PromoteToGitOps  (started 2026-10-27 10:16:40Z)
  18. [Failed] git-commit  Git
      commit the live state to ssh://***@git.example.com/gitops.git branch main
...
        started 2026-10-27 10:16:40Z, finished 2026-10-27 10:16:42Z, attempts 1
        result: branch main of ssh://***@git.example.com/gitops.git is at a2c8b964f945fe157a56c4f87f8b0b2a3176e7a3, but the run's base is 6dd30f74794e7c2d1ef0405918b9b92bb00a4a11: Git changed during the run
  19. [Pending] reconcile-git-a  site a
...

History:
  2026-10-27 10:15:00Z  Running  tack v1.3.0, user operator@example.com
  2026-10-27 10:16:42Z  Stopped (GitChanged)  tack v1.3.0, user operator@example.com
      step git-commit: branch main of ssh://***@git.example.com/gitops.git is at a2c8b964f945fe157a56c4f87f8b0b2a3176e7a3, but the run's base is 6dd30f74794e7c2d1ef0405918b9b92bb00a4a11: Git changed during the run
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
| `--accept-git-head SHA` | The run stopped with `GitChanged`; or with `GitDiffersFromLive` once Git is fixed, or `FluxChanged` for an expected change ([runbooks/stop-reasons.md](runbooks/stop-reasons.md)). | Review the commits between the base and the new head named in the message, such as `git log --oneline 6dd30f7..a2c8b96`, then `tack resume RUN -f workload.yaml --accept-git-head a2c8b96`. When the `GitChanged` message names one head, only that commit, or `HEAD` for it, is accepted; `tack status RUN` shows it. Otherwise `HEAD` is the branch head when `tack resume` starts: it prints that commit before it asks, and records it. Before the run's commit, the accepted head becomes its base. After it, the release checks Git against the accepted head, which must contain the run's commit. |
| `--fence SITE` | The run stopped with `FencedSiteIsPrimary`, and its message offers the flag. | A lost site came back and answers as primary, or declares itself primary. Check it, then `tack resume RUN -f workload.yaml --fence a` declares the new primary there. When no step of the run is left to fence the site, such as an old primary promoted again by hand, the flag cannot help, and the message says so ([runbooks/stop-reasons.md](runbooks/stop-reasons.md#fencedsiteisprimary)). |
| `--take-over RUN` | Another run of the workload can no longer finish, such as when its ledger is at a lost site. Without the flag, the new run is refused while that run is listed and not `Succeeded`, or stops on its holds. | Pass it when starting the new run: `tack run -f workload.yaml --to b --mode site-loss --take-over RUN` takes over the holds of `RUN`, and records it in its ledger. `RUN` can then never resume. Repeat the flag for each run to take over: `--take-over RUN1 --take-over RUN2`. |
| `--force` | `tack abandon RUN`, or `tack run --take-over RUN`, refused with `a tack process may still run the run`. | The process that runs a run holds the run's Lease, and renews it every 10 s ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 28). The refusal names that process, or, for a run without a Lease, such as one that v1.4.0 runs, when its ledger was last written. Stop that process, or check that it ended, then wait until the time the refusal names. Pass `--force` only once a human checked that no `tack` process runs `RUN`: a step in flight there still changes the workload. |

A hold set outside Tack, which stops a run with `HeldByAnotherRun`, stays until the operator removes it: Tack never removes a hold it did not set ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)).

After `--accept-git-head`, the check before release reports differences outside the run's own fields instead of stopping, and the check after release does not stop on a changed generation ([RFC#0005](../rfcs/RFC0005.md#requirements), Requirements 8 and 10).

`tack run` searches for other runs again once its ledger exists. When another run of the workload started at the same time, the run ends before any step, changes nothing, and is marked abandoned: `tack status` lists it as `Abandoned`, and it never resumes. Plan again once the other run is done.

`tack resume` refuses a run that another run took over, or that another run of the workload started after: its plan may no longer match the sites. Resume or check the newer run instead. It refuses before it asks for the workload's name, with exit code 1, and searches again after the confirmation.

`tack resume` asks for the workload's name like `tack run`. `--yes` skips that, only for a resume a human approved.

## Interrupting a run

The first Ctrl-C, SIGTERM or SIGHUP stops the run: the step in flight ends, and the ledger records the stop, which can take up to 30 s when the ledger's site is slow, then its Event, up to 5 s more. A second signal ends `tack` at once, without that record; once the ledger holds the stop, it loses only the Event: the ledger then shows the step `Running`, and `tack resume` runs it again. SIGHUP comes when the terminal or SSH session ends; run `tack` in `tmux` or `screen` on a remote host, so a dropped connection does not stop the run. A process that ends this way releases the run's Lease; one ended by a second signal, or killed, leaves the Lease to expire 30 s after its last renewal, and until 10 s after that, `tack abandon` and `tack run --take-over` refuse the run.

## Break-glass

In a site loss, Git may be down too. Tack then switches live and commits later. The sequence is in [RFC#0007](../rfcs/RFC0007.md#design).

| Step | Command | Result |
|---|---|---|
| 1 | `tack plan -f workload.yaml --to b --mode site-loss` | Works with Git down. The notes say the Git step waits for Git, and fields found by `find` are resolved then. |
| 2 | `tack run -f workload.yaml --to b --mode site-loss` | Holds and promotes at b; the steps at the lost site are `Deferred`. Exit code 4, state `Waiting`, reason `GitUnreachable`. |
| 3 | `tack resume RUN -f workload.yaml` once Git answers | Commits, checks Git against live, releases. The run ends `Succeeded`, with the follow-up `Rebuild` for the lost site. |

While the run waits, the held objects stay held: Flux applies nothing to them, including unrelated Git changes. A planned run never starts with Git unreachable. A git command that gets no answer ends within 2 minutes and counts as Git unreachable; over HTTP a stalled transfer ends after 30 s, and over SSH a connection that does not connect ends after 15 s, unless `GIT_SSH_COMMAND`, `GIT_SSH` or `core.sshCommand` names the operator's own ssh command ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 3).

When the lost site answers again, `tack resume` runs its deferred steps: hold, then fence. If the site answers as primary, or declares itself primary, the run stops with `FencedSiteIsPrimary` first and changes nothing there until the operator passes `--fence`. The steps stay as they were, `Deferred` for a lost site. `--fence` holds for the resume it is passed to: each `tack run` and `tack resume` reads the site again before its first step there, and stops again while the site answers as primary. Each also reads every site but the new primary before each Failover step after the old primary's demotion or fence, the promotion among them, in every mode, and stops while one that answers is primary ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 9). A run that already succeeded with deferred steps runs them on a later `tack resume`. If the site comes back after the commit and no resume runs, Flux at that site applies Git, which already declares the new primary. A later resume holds and releases the site at the run's commit; a later commit that Flux applied there stops it with `GitChanged` until the operator accepts that head.

## After a site loss

The old primary may hold writes the new one lacks, and cannot follow it. Rebuild it with [docs/runbooks/rebuild-old-primary.md](runbooks/rebuild-old-primary.md). Until it is rebuilt, `tack plan` refuses a planned switchover back to it.

## Old ledgers

Each search for runs reads every ledger of the workload at each site that answers, in pages of 100 ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 26). `tack plan` and `tack status -f` search once, `tack resume` three times, `tack run` four times. The cost grows with the number of runs.

| Measured for #35 row 47 | Size |
|---|---|
| A ledger as a planned run on 2 sites leaves it | 21 KB of JSON |
| The same on 10 sites | 86 KB |
| A ledger with a full history: 100 entries of 1024 bytes | 138 KB on 2 sites, 204 KB on 10 |
| A search at one site with 1,000 ledgers of 13.5 KB | 13.5 MB, in 10 answers of 1.35 MB |
| The list of `tack status -f` once its search is done, for 10,000 runs | 28 ms |

`tack prune -f workload.yaml --keep N` deletes the ledgers of old runs when their number slows the searches. It lists every run of the workload, newest first, with `delete` or why it keeps it, asks for the workload's name, searches again, then deletes one ledger at a time, oldest first. It needs row 16 of the [permissions](#permissions), and row 7's `get` to delete a run with a follow-up `Rebuild`. The rules ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 27):

| Rule | Why |
|---|---|
| Every site of the workload answers, and every run of the workload at every site is finished: `Succeeded` with no `Deferred` step, `Abandoned` or `TakenOver`. Otherwise prune deletes nothing. | Deleting a newer run lets an older unfinished run resume on a plan that no longer fits the sites ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 18). |
| Delete only a `Succeeded` run with no `Deferred` step. Keep `Abandoned` and `TakenOver` runs. | They record holds that may be left in the cluster, and the take-over that ended them ([runbooks/abandon-run.md](runbooks/abandon-run.md), step 6). Delete one by hand, below. |
| Keep the `--keep` newest runs that succeeded, at least 1. Abandoned and taken-over runs do not count among them: they are kept anyway. A run goes only once `--keep` of them were created certainly after it: at its own site, in a later second; at another site, 10 s or more later. Otherwise prune keeps it, `may be one of the N newest`, and names the run the clocks cannot order it against. | The newest that succeeded records the last switch. Each site's API server sets its ledgers' creation time, to the second, by its own clock. |
| Keep a run whose spec takes over a run that is kept, not abandoned, and not marked `status.supersededBy`. | That run counts as taken over only through this run's spec; without it, it would block every new run. |
| Keep a run that a newer `tack` last wrote (`status.writtenBy`). A `tack` whose version names no release, such as `dev`, keeps every run that a release wrote. | A newer release may need what it recorded. |
| Keep a run with a follow-up `Rebuild` until the member of its site, as the Workload file names it, was created again 10 s or more after the run's ledger was created (`metadata.creationTimestamp`). Prune reads the member only for such a run, and keeps the run when it cannot read it. | `tack plan` refuses a planned switchover to that member while such a run says it diverged ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 14). Both times come from API servers, the member's from its site's and the ledger's from the ledger site's: the 10 s cover their clocks. The run's start comes from the operator's host, whose clock may be behind. |
| Delete by name, each only while it is the object that was read; stop at the first error. | A run that started, or a ledger written, while the operator confirmed is not deleted on a stale list. |

To delete an abandoned or taken-over run by hand, once its record no longer matters, check each row first, with `RUN`, its site's context `T_CTX`, and the ledger namespace `tack-system`:

| Check | How |
|---|---|
| No object still names `RUN` as its holder. | `tack status RUN -f workload.yaml` lists each object the run held; for each, `kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations.failover\.tako\.id/held-by}{"\n"}'` prints something other than `RUN`. Otherwise remove the hold first ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)). |
| It has no follow-up `Rebuild`, or that site's member was created again 10 s or more after the run's ledger was created. | `tack status RUN -f workload.yaml` lists its follow-ups; `kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.metadata.creationTimestamp}{"\n"}'` prints the ledger's creation, and `kubectl --context SITE_CTX -n NS get cluster MEMBER -o jsonpath='{.metadata.creationTimestamp}{"\n"}'` that of the site's member, as the Workload file names it. |
| Its spec takes over no run that is still listed, neither abandoned nor marked `status.supersededBy`. | `kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.spec.takeOver} {.spec.takeOvers}{"\n"}'`, then `tack status` lists each run it names. Deleting `RUN` would make such a run block every new run. |

```sh
kubectl --context T_CTX -n tack-system delete sfo RUN
```

## Permissions

Tack acts with the operator's credentials for each site's kube context. Every site needs the same rows: any site can be `from` or `to`.

| # | Resource | Verbs | Namespace | Commands | For |
|---|---|---|---|---|---|
| 1 | non-resource URL `/readyz` | get | | `plan`, `run`, `resume`, `status -f`, `abandon -f`, `prune` | the check of a site's API ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 5) |
| 2 | non-resource URLs `/api`, `/apis` and below | get | | all | API discovery: the client maps each kind to its resource |
| 3 | `namespaces`, name `kube-system` | get | | `plan`, `run`, `resume` | the site's cluster ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 19) |
| 4 | `sitefailovers` (`failover.tako.id`) | list | the ledger's | all | the search for runs |
| 5 | `sitefailovers` | get, create | the ledger's | `run`, `resume`; `get` also `abandon` | the ledger: a dry-run create, then the create at `to`; reading a run, and a ledger that holds the run's name |
| 6 | `sitefailovers/status` | update | the ledger's | `run`, `resume`, `abandon` | every step; with `--take-over`, the mark on each run taken over, at that run's site; the mark of `tack abandon` |
| 7 | `clusters`, `poolers` (`postgresql.cnpg.io`) named in the Workload | get | the member's | `plan`, `run`, `resume`; `prune` for a run with a follow-up `Rebuild` | reading each site, the owner labels, `Wait`, `Capture` and the checks |
| 8 | the same | update, patch | the member's | `run`, `resume` | `update`: adding and removing the hold annotations. `patch`: the Failover steps, and the server-side dry-run apply of the check before release |
| 9 | `deployments` (`apps`) named after each Pooler | get | the Pooler's | `run`, `resume` | the Pooler waits |
| 10 | `kustomizations` (`kustomize.toolkit.fluxcd.io`) that apply the objects of rows 7 and 8 | get, patch | Flux's | `run`, `resume` | the reconcile request, an annotation; the waits; the render |
| 11 | `gitrepositories` (`source.toolkit.fluxcd.io`) of those Kustomizations | get, patch | Flux's | `run`, `resume` | the reconcile request; the render reads `spec.ignore` and `spec.include` |
| 12 | `configmaps`, `secrets` named in those Kustomizations' `spec.postBuild.substituteFrom` | get | the Kustomization's | `run`, `resume` | the render's variables |
| 13 | `siteservices` (`strait.tako.id`) named in `spec.routing` | get | the SiteService's | `run`, `resume` | the routing check |
| 14 | `events` | create | the ledger's | `run`, `resume`, `abandon` | the ledger's Events. Optional: without it, a run goes on without Events, and says so once ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 23) |
| 15 | `selfsubjectreviews` (`authentication.k8s.io`) | create | | `run`, `resume`, `abandon` | the user in the ledger's history. Without it, the history names no user, and the run says so once |
| 16 | `sitefailovers` | delete | the ledger's | `prune` | deleting the ledgers of old runs ([Old ledgers](#old-ledgers)) |
| 17 | `leases` (`coordination.k8s.io`) | get, create, update | the ledger's | `run`, `resume`; `get` also `abandon` | the run's Lease, which the process that runs it holds; `tack abandon` and `tack run --take-over` refuse while it is fresh ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 28). Optional: without it, a run holds no Lease and says so once, and those commands judge by the ledger |
| 18 | `pods`, `pods/proxy` | get | the member's | `plan`, `run` | the WAL position of each site: the Cluster's current primary Pod, then a GET of its instance manager's status, port 8000, path `/pg/status`, through the API server ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 13). Optional: without it, the plan says it could not read the position, and goes on |

| Fact | Detail |
|---|---|
| A read-only role | Rows 1 to 4 and the `get` of row 7 are enough for `tack plan` and `tack status`. |
| Row 16 | Only `tack prune` needs it. The chart grants it only with `rbac.prune`. |
| Rows 1, 2 and 15 | Kubernetes grants them to every authenticated user through the default ClusterRoleBindings `system:public-info-viewer`, `system:discovery` and `system:basic-user`. A cluster without those bindings must grant them. |
| `resourceNames` | Rows 3 and 7 to 13 can name their objects. Rows 4 to 6 cannot: run names are generated, and `list` and `create` take no name. |
| Row 18 | Optional. It cannot name its Pods: CloudNativePG generates their names. `get` on `pods/proxy` lets a GET reach any port of any Pod in the namespace through the API server, such as an endpoint that a Pod serves without authentication: grant it only in the members' namespaces. |
| Row 17 | Optional. Without it, a run holds no Lease, says so once, and goes on; `tack abandon` and `tack run --take-over` then judge the run by its ledger, which keeps refusing them longer after a process was killed. |
| Checked | On the e2e's kind clusters, a ServiceAccount with exactly these rows, each named where the table allows, ran `tack plan`, `tack status`, a planned run with the routing check and a Kustomization with `postBuild`, a resume, a run with `--take-over`, and a site-loss run. Without row 3, `tack plan` fails with `namespaces "kube-system" is forbidden`; without the ConfigMap of row 12, the check before release stops with `StepFailed`. Rows 14 and 15 came after that check, with v1.3.0, row 16 with v1.4.0, and row 17 after v1.4.0. The e2e's `TestRoutingFromStrait` now runs `tack plan`, a planned run with the routing check and `tack status` as a ServiceAccount bound to [the chart's roles](#the-charts-roles) and Kubernetes' default bindings only: the run records its Events (row 14) and the ServiceAccount as its history's user (row 15). The e2e has no `postBuild`, so it does not use row 12. Its `TestPrune` runs `tack prune` as that ServiceAccount, with `rbac.prune`, and deletes ledgers through row 16. |

The runbooks' own `kubectl` commands need more, such as `patch` on `sitefailovers/status`; each runbook says so.

### The chart's roles

The chart creates roles that grant exactly the rows above when `rbac.create` is `true`, row 16 only when `rbac.prune` is `true` too. It creates none by default ([RFC#0009](../rfcs/RFC0009.md#the-chart), Requirement 29).

| Role | Kind, namespace | Rows |
|---|---|---|
| `<release>-cluster` | ClusterRole | 1, 2, 3 and 15 |
| `<release>-ledger` | Role, the release's namespace: install the chart in the ledger namespace | 4, 5, 6, 14 and 17; 16 with `rbac.prune` |
| `<release>-objects` | Role, each namespace of `rbac.objects` | 7 to 13, for the objects that entry names; 18 where the entry names `clusters` |

| Value | Default | Meaning |
|---|---|---|
| `rbac.create` | `false` | Create the roles. |
| `rbac.prune` | `false` | Also grant row 16, for `tack prune`. |
| `rbac.subjects` | `[]` | Bind each role to these subjects, such as the operators' group: a ClusterRoleBinding, and a RoleBinding next to each Role. Empty binds nothing. |
| `rbac.objects` | `[]` | Per namespace, the names of the Workload files' objects: `clusters`, `poolers`, `kustomizations`, `gitRepositories`, `configMaps`, `secrets`, `siteServices`. Each list is optional. `poolers` also grants `get` on the Deployment of the same name (row 9). A kind without names gets no rule. The chart fails on an entry without a namespace or without names, and on a namespace listed twice. Each namespace must exist before the install: Helm does not create it. |

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

Each site's chart may name every site's objects, so one values file serves all sites. A role grants the names it lists, also where no such object exists.

### Who may write ledgers

Writing a ledger is privileged. A ledger names the steps, the kube contexts and the Git URL that `tack resume` uses with the operator's credentials. Each search for runs selects ledgers by the label `failover.tako.id/workload`, and decides on what it finds whether a run starts or resumes ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 8 and 18).

| Access | What it lets its holder do |
|---|---|
| `create` on `sitefailovers` | plant a run for an operator to resume |
| `update`, `patch` on `sitefailovers/status` | change a run's state, captured values, holds and follow-ups |
| `update`, `patch` on `sitefailovers` | change a ledger's labels: a run without its workload's label is left out of every search, so a second run starts, or an older run resumes, while it is not done |
| `delete`, `deletecollection` on `sitefailovers` | remove a run's record, with the same effect |

| Rule | Why |
|---|---|
| Grant rows 5 and 6 to the operators only, and row 16 only to the operators who prune, in the ledger namespace at every site. | They are the only writes Tack needs. Tack never needs `update` or `patch` on `sitefailovers`, nor `deletecollection`. |
| Keep the ledger namespace out of every Flux path, and keep `SiteFailover` objects out of Git. | A Kustomization applies what Git holds with kustomize-controller's service account, or the one its `spec.serviceAccountName` names. |
| Run the check below at every site: before the first run, and after each change of who is bound to which role. | A role with a wildcard resource or verb, such as `cluster-admin`, includes every access above. RBAC only adds: neither the chart nor Tack can take it away. |

The check, as a cluster admin, since `--as` needs `impersonate`. First list who is bound, then ask for each subject that is not an operator, such as a GitOps controller's or a CI system's ServiceAccount. Every answer must be `no`:

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

For a group, pass a user name and the group: `--as someone --as-group GROUP`.

A Role with rows 4 to 6, for a RoleBinding to the operators' group:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: tack-ledger-writer
  namespace: tack-system
rules:
- apiGroups: ["failover.tako.id"]
  resources: ["sitefailovers"]
  verbs: ["get", "list", "create"]
- apiGroups: ["failover.tako.id"]
  resources: ["sitefailovers/status"]
  verbs: ["update"]
```

What `tack resume` checks of a ledger, and what it leaves to the operator:

| Part of the ledger | Checked |
|---|---|
| `spec.steps` | Before the confirmation: they must be the plan that the driver composes from the ledger's Workload and request ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 21). At the Git step: each change's file and path must be where the Workload's mapping, or the manifest of its object, puts it ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 15). |
| `spec.workload`: contexts, Clusters, Git URL | Not checked: a planted Workload composes its own plan. The confirmation prints each site's context, API server and recorded cluster, and the Git URL: compare them with the Workload file. |
| `status`: captured values, Git revisions, step states, holds, follow-ups | Not checked. |

For Git, the operator's `git` configuration needs read access for `tack plan`, and push access to the branch for `tack run`. Tack pushes without force and skips client-side hooks; server-side rules still apply ([RFC#0006](../rfcs/RFC0006.md#requirements)).
