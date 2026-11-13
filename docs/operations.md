# Operations

## Reading a run

| Command | Shows |
|---|---|
| `tack status -f workload.yaml` | the workload's runs at every reachable site: site, run, mode, from, to, state, reason, start. The state is `Abandoned` for an abandoned run, and `TakenOver` for a run that another run took over. After the list comes each site that gave no answer, with its error. That site's runs are not listed, and the command exits with code 1. With `-o yaml`, those lines follow the ledgers as YAML comments. With `-o json`, they go to stderr |
| `tack status RUN -f workload.yaml` | one run: state and reason, message, the version of the `tack` that last wrote it, Git base and commit, captured values by name, held objects as the ledger records them, when writes stopped and came back (`Writes down:`), the fork point of a site loss (`Fork:`), each phase and step with its state, start, end, attempts, last result and earlier attempts, follow-ups with the old primary's snapshot and archive, and the history. The history lists each start, stop, wait and success of the run, with the version, the user and the flags of the `tack` that recorded it |
| `tack status [RUN] --context site-b` | the same from one context, without the Workload file. Add `--namespace` when the ledger namespace is not `tack-system` |
| `tack status RUN -f workload.yaml -o yaml`, `-o json` | the whole `SiteFailover` object |
| `tack abandon RUN -f workload.yaml --reason TEXT` | marks a run that must never go on: [runbooks/abandon-run.md](runbooks/abandon-run.md) |
| `tack prune -f workload.yaml --keep N` | lists the workload's runs, and deletes the ledgers of old runs that succeeded: [Old ledgers](#old-ledgers) |
| `kubectl --context site-b -n tack-system get sfo` | the ledgers at one site, with workload, mode, from, to, state, reason, how long ago each was abandoned (empty otherwise), phase, age, and the run that took each over (empty otherwise). The State column of an abandoned or taken-over run keeps its last state, such as `Stopped`. `status.abandoned` and `status.supersededBy` mark such a run, apart from `status.state`. The Abandoned column comes with the CRD of v1.4.0. The TakenOverBy column comes with the CRD of the release after v1.5.0 ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 15). A run's take-over mark may not have landed, because its site gave no answer at the time. Such a run shows an empty TakenOverBy, and `tack status` lists it as `TakenOver` |
| `kubectl --context site-b -n tack-system get sfo -o custom-columns=NAME:.metadata.name,STATE:.status.state,REASON:.status.reason,ABANDONED:.status.abandoned.at,TAKEN-OVER-BY:.status.supersededBy` | the ledgers at one site, with the time each was abandoned and the run that took each over, `<none>` otherwise. Works with a CRD of any v1.3.0 or later release |
| `kubectl --context site-b -n tack-system describe sfo RUN` | also the ledger's Events: each step's start and end, and each change of the run's state. They last as long as the API server keeps Events (1 h by default) |

`-o yaml` and `-o json` are stable output for scripts and tools. Both have the same content: the `SiteFailover` objects, with the Git URL's user and password shown as `***`. `tack plan` prints the object a run would create. `tack status RUN` prints one object. `tack status` without RUN prints one YAML document per run, or one JSON `List` whose `items` are the runs. Minor releases add fields. Within `failover.tako.id/v1`, fields are never removed or renamed ([RFC0003](../rfcs/RFC0003.md#requirements), Requirement 13).

Every `tack` command starts each progress line, such as a step or a retried check, with its time in UTC (`2026-10-27T10:15:00Z`). A terminal's log then lines up with the ledger and with the API servers' audit logs ([RFC#0009](../rfcs/RFC0009.md#progress-and-status), Requirement 27).

A read checks all its sites at once. The lines of its checks come grouped by site, in the order of the Workload's sites. Each line has the time it was printed, so a later site's lines can carry earlier times ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 10).

Before those checks, Tack obtains each site's credentials. It does this one site at a time, in the same order, with no time limit. A credential plugin that prompts on the terminal, such as an OIDC login, then prompts alone, and its wait takes no time from a check. When a site's credentials take more than 2 s, a line `waiting   for the credentials of site S` says so. A site whose credentials cannot be obtained is not counted as lost: the command stops ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 12).

The history in `status.history` keeps at most 100 entries: the first, and the newest ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 22).

A run's state is `Running`, `Waiting`, `Stopped` or `Succeeded`. Each step's state is `Pending`, `Running`, `Succeeded`, `Failed` or `Deferred` ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 9 and 11). A run can be left `Running` by a process that ended, such as in a killed terminal. `tack resume` continues it. A run shown as `NotStarted` never recorded its start. Its process ended between creating the ledger and its first write, so none of its steps ran. It cannot resume. Plan again, and start a new run with `--take-over RUN`.

A run that stopped because the Git branch moved, shortened:

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

`tack run` and `tack resume` exit with code 3 when the run stops and needs the operator. They exit with code 4 when the run waits for something outside it. The ledger's reason says why. The table of [RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 10, lists each reason and how the operator goes on.

| Runbook | For |
|---|---|
| [runbooks/stop-reasons.md](runbooks/stop-reasons.md) | each reason: what stopped, what to check, how to go on |
| [runbooks/no-primary-after-demotion.md](runbooks/no-primary-after-demotion.md) | a planned run that stopped after its demotion and before its promotion: no site takes writes |
| [runbooks/abandon-run.md](runbooks/abandon-run.md) | a run that must not go on, and holds left behind |

Most reasons go on with a plain `tack resume RUN -f workload.yaml` once the cause is fixed. Three decisions have their own flag:

| Flag | When | Example |
|---|---|---|
| `--accept-git-head SHA` | The run stopped with `GitChanged`. Or it stopped with `GitDiffersFromLive` and Git is now fixed, or with `FluxChanged` for an expected change ([runbooks/stop-reasons.md](runbooks/stop-reasons.md)). | Review the commits between the base and the new head that the message names, such as with `git log --oneline 6dd30f7..a2c8b96`. Then run `tack resume RUN -f workload.yaml --accept-git-head a2c8b96`. When the `GitChanged` message names one head, only that commit is accepted, or `HEAD` standing for it. `tack status RUN` shows that head. Otherwise `HEAD` is the branch head when `tack resume` starts: `tack resume` prints that commit before it asks, and records it. If the run has not made its commit yet, the accepted head becomes the run's base. If it has, the release checks Git against the accepted head, which must contain the run's commit. |
| `--fence SITE` | The run stopped with `FencedSiteIsPrimary`, and its message offers the flag. | A lost site came back and answers as primary, or declares itself primary. Check the site. Then `tack resume RUN -f workload.yaml --fence a` declares the new primary there. Sometimes no step of the run is left to fence the site, such as when an old primary was promoted again by hand. Then the flag cannot help, and the message says so ([runbooks/stop-reasons.md](runbooks/stop-reasons.md#fencedsiteisprimary)). |
| `--take-over RUN` | Another run of the workload can no longer finish, such as when its ledger is at a lost site. Without the flag, the new run is refused while that run is listed and not `Succeeded`, or the new run stops on that run's holds. | Pass it when starting the new run. `tack run -f workload.yaml --to b --mode site-loss --take-over RUN` takes over the holds of `RUN`, and records the take-over in its own ledger. `RUN` can then never resume. Repeat the flag for each run to take over: `--take-over RUN1 --take-over RUN2`. |
| `--force` | `tack abandon RUN` or `tack run --take-over RUN` was refused with `a tack process may still run the run`. | The process that runs a run holds the run's Lease, and renews it every 10 s ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 28). The refusal names that process. For a run without a Lease, such as one that v1.4.0 runs, it names when the ledger was last written instead. Stop that process, or check that it ended. Then wait until the time the refusal names. Pass `--force` only once a human has checked that no `tack` process runs `RUN`. A step in flight in such a process still changes the workload. |

A hold set outside Tack stops a run with `HeldByAnotherRun`. It stays until the operator removes it, because Tack never removes a hold it did not set ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)).

After `--accept-git-head`, the check before release reports differences outside the run's own fields, instead of stopping on them. The check after release does not stop on a changed generation ([RFC#0005](../rfcs/RFC0005.md#requirements), Requirements 8 and 10).

Once its ledger exists, `tack run` searches for other runs again. If another run of the workload started at the same time, the run ends before any step and changes nothing. It is marked abandoned: `tack status` lists it as `Abandoned`, and it never resumes. Plan again once the other run is done.

`tack resume` refuses a run that another run took over. It also refuses a run when another run of the workload started after it, because the older run's plan may no longer match the sites. Resume or check the newer run instead. `tack resume` refuses before it asks for the workload's name, with exit code 1. It searches again after the confirmation.

`tack resume` asks for the workload's name, like `tack run`. `--yes` skips that. Use it only for a resume that a human approved.

## How long writes stay down

`tack status RUN` prints `Writes down:` from `status.writes`:

| Mode | Writes stopped | Writes came back |
|---|---|---|
| `Planned` | the start of `pooler-<from>`, or of `demote-<from>` without a Pooler | the end of `wait-pooler-<to>`, or of `wait-promoted-<to>` without a Pooler |
| `SiteLoss` | unknown, because the site was lost before the run. Tack records the run's start, marked as an estimate. Writes had stopped by then at the latest | as above |

The window ends when the new primary takes writes at its site. Clients that reach it through Strait also wait for Strait's route. To measure what clients see, and to set a budget, use [measure-failover-time.md](runbooks/measure-failover-time.md) in staging.

**Demotion time.** In a planned switchover, writes stay down from the moment the old Pooler scales to 0 until the new primary is ready. The longest part is often the old primary's shutdown. CloudNativePG lets it last up to the old Cluster's `spec.switchoverDelay`, 3600 seconds by default (v1.30.1, `api/v1/cluster_types.go:375-381`, `pkg/management/postgres/instance.go:695-720`). Tack waits for the demotion up to `timeouts.demotion`, 10 minutes by default.

| Case | What happens |
|---|---|
| The shutdown ends within both limits | The demotion token appears, and the run goes on. |
| The shutdown outlasts `timeouts.demotion` | The run stops with `StepFailed` at `wait-demoted-<from>`, while CloudNativePG keeps waiting. Writes stay down. Resume the run: the wait starts again, and goes on once the token appears. |
| The shutdown outlasts `switchoverDelay` | CloudNativePG shuts down at once and publishes no token. The run stops at `wait-demoted-<from>`: [no-primary-after-demotion.md](runbooks/no-primary-after-demotion.md). |

Keep `timeouts.demotion` shorter than `switchoverDelay`, so Tack reports a slow shutdown before CloudNativePG gives up on it. While the new primary replays up to the token, Tack stops the wait early if replay stands still for 2 minutes short of the token ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 18). The patch that promotes the new primary stays in place. It promotes once it catches up, and a resume waits again.

## What fencing does

To fence a member means to make it declare another member primary, so that it stops being primary ([RFC0001](../rfcs/RFC0001.md#glossary)). For CloudNativePG, Tack's fence is one patch of the old Cluster: `spec.replica.primary` set to the new primary's Cluster, and `spec.replica.promotionToken` removed (`fence-<from>`, [RFC0004](../rfcs/RFC0004.md#requirements), Requirement 7).

| Tack's fence | Does |
|---|---|
| Changes the old Cluster's spec | yes, once its site's Kubernetes API answers. CloudNativePG then restarts the old primary as a replica |
| Runs while the site is lost | no. The step waits for the site (`Deferred`), and runs on a `tack resume` once the site answers |
| Stops the old primary while its API is unreachable | no |
| Stops clients at the old site from writing to it | no. Applications at that site, and its Pooler until `pooler-<from>` runs, still reach it |
| Cuts the site's network, or stops its nodes | no |
| Rewinds or rebuilds the old primary | no: [rebuild-old-primary.md](runbooks/rebuild-old-primary.md) |

Flux at the old site fences it too, once that site reaches Git and applies the run's commit. That commit declares the new primary. Until one of these happens, the old primary takes the writes that reach it. Before a site-loss run, the operator fences by other means, outside Tack: the database, and the site's applications or their Pooler ([site-loss.md](runbooks/site-loss.md)).

## Interrupting a run

The first Ctrl-C, SIGTERM or SIGHUP stops the run. The step in flight ends, and the ledger records the stop. That can take up to 30 s when the ledger's site is slow. Then Tack records the stop's Event, which can take up to 5 s more.

A second signal ends `tack` at once, without that record. The ledger then shows the step `Running`, and `tack resume` runs it again. If the ledger already holds the stop, only the Event is lost.

SIGHUP comes when the terminal or SSH session ends. On a remote host, run `tack` in `tmux` or `screen`, so a dropped connection does not stop the run.

A process that ends this way releases the run's Lease. A process ended by a second signal, or killed, leaves the Lease to expire 30 s after its last renewal. Until 10 s after that, `tack abandon` and `tack run --take-over` refuse the run.

## Break-glass

In a site loss, Git may be down too. Tack then switches live and commits later. The sequence is in [RFC#0007](../rfcs/RFC0007.md#design).

| Step | Command | Result |
|---|---|---|
| 1 | `tack plan -f workload.yaml --to b --mode site-loss` | Works with Git down. The notes say that the Git step waits for Git, and that fields found by `find` are resolved then. |
| 2 | `tack run -f workload.yaml --to b --mode site-loss` | Holds and promotes at b. The steps at the lost site are `Deferred`. Exit code 4, state `Waiting`, reason `GitUnreachable`. |
| 3 | `tack resume RUN -f workload.yaml` once Git answers | Commits, checks Git against live, releases. The run ends `Succeeded`, with the follow-up `Rebuild` for the lost site. |

While the run waits, the held objects stay held. Flux applies nothing to them, not even unrelated Git changes. A planned run never starts with Git unreachable.

A git command that gets no answer ends within 2 minutes, and counts as Git unreachable. Over HTTP, a stalled transfer ends after 30 s. Over SSH, a connection that does not connect ends after 15 s, unless `GIT_SSH_COMMAND`, `GIT_SSH` or `core.sshCommand` names the operator's own ssh command ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 3).

When the lost site answers again, `tack resume` runs its deferred steps: hold, then fence. The site may answer as primary, or declare itself primary. Then the run first stops with `FencedSiteIsPrimary`, and changes nothing there until the operator passes `--fence`. The steps stay as they were: `Deferred` for a lost site.

`--fence` applies only to the resume it is passed to. Each `tack run` and `tack resume` reads the site again before its first step there, and stops again while the site answers as primary. Each also checks before every Failover step that comes after the old primary's demotion or fence, the promotion included, in every mode. It reads every site except the new primary, and stops while one that answers is primary ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 9).

A run that already succeeded with deferred steps runs them on a later `tack resume`. If the site comes back after the commit and no resume runs, Flux at that site applies Git, which already declares the new primary. A later resume holds and releases the site at the run's commit. If Flux applied a later commit there, the resume stops with `GitChanged` until the operator accepts that head.

## After a site loss

The old primary may hold writes that the new one lacks, and it cannot follow the new one. Rebuild it with [docs/runbooks/rebuild-old-primary.md](runbooks/rebuild-old-primary.md). Until it is rebuilt, `tack plan` refuses a planned switchover back to it.

## Old ledgers

Each search for runs reads every ledger of the workload at each site that answers, in pages of 100 ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 26). `tack plan` and `tack status -f` search once, `tack resume` three times, `tack run` four times. The cost grows with the number of runs.

| Measured for #35 row 47 | Size |
|---|---|
| A ledger as a planned run on 2 sites leaves it | 21 KB of JSON |
| The same on 10 sites | 86 KB |
| A ledger with a full history: 100 entries of 1024 bytes | 138 KB on 2 sites, 204 KB on 10 |
| A search at one site with 1,000 ledgers of 13.5 KB | 13.5 MB, in 10 answers of 1.35 MB |
| The list of `tack status -f` once its search is done, for 10,000 runs | 28 ms |

`tack prune -f workload.yaml --keep N` deletes the ledgers of old runs. Use it when their number slows the searches. It lists every run of the workload, newest first, each with `delete` or the reason it is kept. Then it asks for the workload's name, searches again, and deletes one ledger at a time, oldest first. It needs row 16 of the [permissions](#permissions). To delete a run with a follow-up `Rebuild`, it also needs row 7's `get`. The rules ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 27):

| Rule | Why |
|---|---|
| Every site of the workload answers, and every run of the workload at every site is finished: `Succeeded` with no `Deferred` step, `Abandoned` or `TakenOver`. Otherwise prune deletes nothing. | Deleting a newer run lets an older unfinished run resume on a plan that no longer fits the sites ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 18). |
| Delete only a `Succeeded` run with no `Deferred` step. Keep `Abandoned` and `TakenOver` runs. | They record holds that may be left in the cluster, and the take-over that ended them ([runbooks/abandon-run.md](runbooks/abandon-run.md), step 6). Delete one by hand, below. |
| Keep the `--keep` newest runs that succeeded, at least 1. Abandoned and taken-over runs do not count among them: they are kept anyway. A run is deleted only once `--keep` of those runs were certainly created after it. At its own site, that means in a later second. At another site, it means 10 s or more later. Otherwise prune keeps the run with `may be one of the N newest`, and names the run that the clocks cannot order it against. | The newest run that succeeded records the last switch. Each site's API server sets its ledgers' creation time, to the second, by its own clock. |
| Keep a run whose spec takes over a run that is kept, not abandoned, and not marked `status.supersededBy`. | The other run counts as taken over only through this run's spec. Without this run, the other run would block every new run. |
| Keep a run that a newer `tack` last wrote (`status.writtenBy`). A `tack` whose version names no release, such as `dev`, keeps every run that a release wrote. | A newer release may need what it recorded. |
| Keep a run with a follow-up `Rebuild` until the member of its site, as the Workload file names it, was created again 10 s or more after the run's ledger was created (`metadata.creationTimestamp`). Prune reads the member only for such a run, and keeps the run when it cannot read it. | `tack plan` refuses a planned switchover to that member while such a run says it diverged ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 14). Both times come from API servers: the member's from its own site, and the ledger's from the ledger's site. The 10 s cover the difference between their clocks. The run's start comes from the operator's host, whose clock may be behind. |
| Delete each ledger by name, and only while it is still the object that was read. Stop at the first error. | A run that started, or a ledger that was written, while the operator was confirming is not deleted because of a stale list. |

To delete an abandoned or taken-over run by hand, once its record no longer matters, first check each row below. `RUN` is the run, `T_CTX` its site's context, and `tack-system` the ledger namespace:

| Check | How |
|---|---|
| No object still names `RUN` as its holder. | `tack status RUN -f workload.yaml` lists each object the run held. For each one, `kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations.failover\.tako\.id/held-by}{"\n"}'` must print something other than `RUN`. Otherwise remove the hold first ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)). |
| It has no follow-up `Rebuild`, or that site's member was created again 10 s or more after the run's ledger was created. | `tack status RUN -f workload.yaml` lists its follow-ups. `kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.metadata.creationTimestamp}{"\n"}'` prints when the ledger was created. `kubectl --context SITE_CTX -n NS get cluster MEMBER -o jsonpath='{.metadata.creationTimestamp}{"\n"}'` prints when the site's member was created, as the Workload file names it. |
| Its spec takes over no run that is still listed, neither abandoned nor marked `status.supersededBy`. | Run `kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.spec.takeOver} {.spec.takeOvers}{"\n"}'`. Then look up each run it names with `tack status`. Deleting `RUN` would make such a run block every new run. |

```sh
kubectl --context T_CTX -n tack-system delete sfo RUN
```

## Permissions

Tack acts with the operator's credentials for each site's kube context. Every site needs the same rows, because any site can be `from` or `to`.

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
| 18 | `pods`, `pods/proxy` | get | the member's | `plan`, `run`, `resume` | the WAL position of each site: the Cluster's current primary Pod, then a GET of its instance manager's status, port 8000, path `/pg/status`, through the API server; for a replica, a GET of its metrics too, port 9187, path `/metrics`, for the replay lag ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 13). Optional: without it, the plan says it could not read the position, and goes on; a site-loss run records that it could not read the fork point, and goes on |
| 19 | `backups` (`postgresql.cnpg.io`) | get, create | the member's | `run`, `resume` | the old primary's snapshot after a site loss, when the Workload sets `cnpg.snapshot` ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 15). Only then |
| 20 | `objectstores` (`barmancloud.cnpg.io`) named by the Clusters' barman-cloud plugin | get | the member's | `plan`, `run` | where a Cluster archives WAL through the barman-cloud plugin, to check that each site's destination is its own ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 16). Optional: without it, the plan warns that it could not check |

| Fact | Detail |
|---|---|
| A read-only role | Rows 1 to 4 and the `get` of row 7 are enough for `tack plan` and `tack status`. |
| Row 16 | Only `tack prune` needs it. The chart grants it only with `rbac.prune`. |
| Rows 1, 2 and 15 | Kubernetes grants them to every authenticated user through the default ClusterRoleBindings `system:public-info-viewer`, `system:discovery` and `system:basic-user`. A cluster without those bindings must grant them. |
| `resourceNames` | Rows 3, 7 to 13 and 20 can name their objects. Rows 4 to 6 cannot: run names are generated, and `list` and `create` take no name. |
| Row 19 | Only for a Workload with `cnpg.snapshot`. It cannot name its Backups: their names include the run's name. |
| Row 18 | Optional. It cannot name its Pods: CloudNativePG generates their names. `get` on `pods/proxy` lets a GET reach any port of any Pod in the namespace, through the API server. That includes an endpoint that a Pod serves without authentication. Grant it only in the members' namespaces. |
| Row 17 | Optional. Without it, a run holds no Lease, says so once, and goes on. `tack abandon` and `tack run --take-over` then judge the run by its ledger. After a process was killed, the ledger makes them keep refusing for longer. |
| Checked | On the e2e's kind clusters, a ServiceAccount with exactly these rows, each named where the table allows, ran `tack plan`, `tack status`, a planned run with the routing check and a Kustomization with `postBuild`, a resume, a run with `--take-over`, and a site-loss run. Without row 3, `tack plan` fails with `namespaces "kube-system" is forbidden`. Without the ConfigMap of row 12, the check before release stops with `StepFailed`. Rows 14 and 15 came after that check, with v1.3.0. Row 16 came with v1.4.0, and row 17 after v1.4.0. The e2e's `TestRoutingFromStrait` now runs `tack plan`, a planned run with the routing check, and `tack status`. It runs them as a ServiceAccount bound only to [the chart's roles](#the-charts-roles) and Kubernetes' default bindings. The run records its Events (row 14), and the ServiceAccount as its history's user (row 15). The e2e has no `postBuild`, so it does not use row 12. Its `TestPrune` runs `tack prune` as that ServiceAccount, with `rbac.prune`, and deletes ledgers through row 16. |

The runbooks' own `kubectl` commands need more access, such as `patch` on `sitefailovers/status`. Each runbook says what it needs.

### The chart's roles

When `rbac.create` is `true`, the chart creates roles that grant exactly the rows above. It grants row 16 only when `rbac.prune` is `true` too. By default it creates no roles ([RFC#0009](../rfcs/RFC0009.md#the-chart), Requirement 29).

| Role | Kind, namespace | Rows |
|---|---|---|
| `<release>-cluster` | ClusterRole | 1, 2, 3 and 15 |
| `<release>-ledger` | Role, the release's namespace: install the chart in the ledger namespace | 4, 5, 6, 14 and 17; 16 with `rbac.prune` |
| `<release>-objects` | Role, each namespace of `rbac.objects` | 7 to 13 and 20, for the objects that entry names; 18 and 19 where the entry names `clusters` |

| Value | Default | Meaning |
|---|---|---|
| `rbac.create` | `false` | Create the roles. |
| `rbac.prune` | `false` | Also grant row 16, for `tack prune`. |
| `rbac.subjects` | `[]` | Bind each role to these subjects, such as the operators' group: a ClusterRoleBinding, and a RoleBinding next to each Role. Empty binds nothing. |
| `rbac.objects` | `[]` | Per namespace, the names of the Workload files' objects: `clusters`, `poolers`, `kustomizations`, `gitRepositories`, `configMaps`, `secrets`, `siteServices`, and `objectStores`, the barman-cloud ObjectStores that the Clusters archive to. Each list is optional. `poolers` also grants `get` on the Deployment of the same name (row 9). A kind without names gets no rule. The chart fails on an entry without a namespace or without names, and on a namespace listed twice. Each namespace must exist before the install: Helm does not create it. |

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

Each site's chart may name every site's objects, so one values file serves all sites. A role grants the names it lists, even where no such object exists.

### Who may write ledgers

Writing a ledger is privileged. A ledger names the steps, the kube contexts and the Git URL that `tack resume` uses with the operator's credentials. Each search for runs selects ledgers by the label `failover.tako.id/workload`. What it finds decides whether a run starts or resumes ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 8 and 18).

| Access | What it lets its holder do |
|---|---|
| `create` on `sitefailovers` | plant a run for an operator to resume |
| `update`, `patch` on `sitefailovers/status` | change a run's state, captured values, holds and follow-ups |
| `update`, `patch` on `sitefailovers` | change a ledger's labels. A run without its workload's label is left out of every search. So a second run starts, or an older run resumes, while that run is not done |
| `delete`, `deletecollection` on `sitefailovers` | remove a run's record, with the same effect |

| Rule | Why |
|---|---|
| Grant rows 5 and 6 to the operators only, and row 16 only to the operators who prune, in the ledger namespace at every site. | They are the only writes Tack needs. Tack never needs `update` or `patch` on `sitefailovers`, nor `deletecollection`. |
| Keep the ledger namespace out of every Flux path, and keep `SiteFailover` objects out of Git. | A Kustomization applies what Git holds with kustomize-controller's service account, or the one its `spec.serviceAccountName` names. |
| Run the check below at every site: before the first run, and after each change of who is bound to which role. | A role with a wildcard resource or verb, such as `cluster-admin`, includes every access above. RBAC only adds access: neither the chart nor Tack can take it away. |

Run the check as a cluster admin, since `--as` needs `impersonate`. First list who is bound. Then ask for each subject that is not an operator, such as the ServiceAccount of a GitOps controller or of a CI system. Every answer must be `no`:

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

What `tack resume` checks in a ledger, and what it leaves to the operator:

| Part of the ledger | Checked |
|---|---|
| `spec.steps` | Before the confirmation: they must be the plan that the driver composes from the ledger's Workload and request ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 21). At the Git step: each change's file and path must be where the Workload's mapping, or the manifest of its object, puts it ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 15). |
| `spec.workload`: contexts, Clusters, Git URL | Not checked, because a planted Workload composes its own plan. The confirmation prints each site's context, API server and recorded cluster, and the Git URL: compare them with the Workload file. |
| `status`: captured values, Git revisions, step states, holds, follow-ups | Not checked. |

For Git, the operator's `git` configuration needs read access for `tack plan`, and push access to the branch for `tack run`. Tack pushes without force and skips client-side hooks. Server-side rules still apply ([RFC#0006](../rfcs/RFC0006.md#requirements)).
