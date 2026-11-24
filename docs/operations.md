# Operations

## Reading a run

| Command | Shows |
|---|---|
| `tack status -f workload.yaml` | runs at every reachable site: site, run, mode, from, to, state, reason, start. A site with no answer follows with its error, and the command exits with code 1 |
| `tack status RUN -f workload.yaml` | one run: state, reason, message, Git base and commit, captured values, held objects, `Writes down:`, `Fork:`, each phase and step, follow-ups, and the history |
| `tack status [RUN] --context site-b` | the same from one context, without the Workload file. Add `--namespace` when the ledger namespace is not `tack-system` |
| `tack status RUN -f workload.yaml -o yaml`, `-o json` | the whole `SiteFailover` object |
| `tack abandon RUN -f workload.yaml --reason TEXT` | marks a run that must never go on: [runbooks/abandon-run.md](runbooks/abandon-run.md) |
| `tack prune -f workload.yaml --keep N` | lists the workload's runs, and deletes the ledgers of old runs that succeeded: [Old ledgers](old-ledgers.md) |
| `kubectl --context site-b -n tack-system get sfo` | the ledgers at one site: workload, mode, from, to, state, reason, abandoned (how long ago), phase, age, taken over by |
| `get sfo` with custom columns, under this table | the same time and run columns, `<none>` otherwise. Works with a CRD of any v1.3.0 or later release |
| `kubectl --context site-b -n tack-system describe sfo RUN` | also the ledger's Events: each step's start and end, and each change of the run's state. They last as long as the API server keeps Events (1 h by default) |

The custom columns:

```sh
kubectl --context site-b -n tack-system get sfo -o custom-columns=NAME:.metadata.name,STATE:.status.state,REASON:.status.reason,ABANDONED:.status.abandoned.at,TAKEN-OVER-BY:.status.supersededBy
```

- The runs of a site that gave no answer are not listed. With `-o yaml` its error lines follow the ledgers as YAML comments, with `-o json` they go to stderr.
- `tack status RUN` also prints the `tack` version that last wrote the run, and each step's attempts and last result. `Writes down:` is when writes stopped and came back ([How long writes stay down](#how-long-writes-stay-down)). `Fork:` is the fork point of a site loss. Each history entry has the version, user and flags of the `tack` that recorded it.

A run's state:

| State | Where | Meaning |
|---|---|---|
| `Running`, `Waiting`, `Stopped`, `Succeeded` | the ledger's `status.state`, and the State column of `get sfo` | A process that ended, such as in a killed terminal, can leave a run `Running`: `tack resume` continues it. |
| `Abandoned` | `tack status`, when `status.abandoned` is set | The run never goes on. `get sfo` keeps its last state, such as `Stopped`, and fills the Abandoned column (CRD of v1.4.0). |
| `TakenOver` | `tack status`, when another run took it over (`status.supersededBy`) | `get sfo` keeps its last state and fills the TakenOverBy column ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 15). If the mark did not land, TakenOverBy stays empty and `tack status` still lists `TakenOver`. |
| `NotStarted` | `tack status`, when `status.state` is empty | The process ended between creating the ledger and its first write: no step ran. It cannot resume. Plan again, and start a new run with `--take-over RUN`. |

The TakenOverBy mark does not land when the run's site gave no answer.

A step's state is `Pending`, `Running`, `Succeeded`, `Failed` or `Deferred` ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 9 and 11).

- `-o yaml` and `-o json` are stable output for scripts. Both hold the `SiteFailover` objects, with the Git URL's user and password shown as `***`. `tack status` without RUN prints one YAML document per run, or one JSON `List` whose `items` are the runs. `tack plan` prints the object a run would create. Minor releases add fields; within `failover.tako.id/v1`, fields are never removed or renamed ([RFC0003](../rfcs/RFC0003.md#requirements), Requirement 13).
- Every progress line starts with its time in UTC (`2026-10-27T10:15:00Z`), so a terminal's log lines up with the ledger and the API servers' audit logs ([RFC#0009](../rfcs/RFC0009.md#progress-and-status), Requirement 27).
- A read checks all its sites at once. Its lines come grouped by site, in the order of the Workload's sites, so a later site's lines can carry earlier times ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 10).
- Before those checks, Tack obtains each site's credentials, one site at a time, with no time limit. A credential plugin that prompts on the terminal, such as an OIDC login, prompts alone, and its wait takes no time from a check. `waiting   for the credentials of site S` appears after 2 s. A site whose credentials cannot be obtained is not counted as lost: the command stops ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 12).
- `status.history` keeps at most 100 entries: the first, and the newest ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 22).

A run that stopped because the Git branch moved, shortened:

```text
$ tack status -f workload.yaml
SITE  RUN                      MODE     FROM  TO  STATE    REASON      STARTED
b     pg-main-20261027-101500  Planned  a     b   Stopped  GitChanged  2026-10-27 10:15:00Z

$ tack status pg-main-20261027-101500 -f workload.yaml
Run tack-system/pg-main-20261027-101500: workload pg-main, Planned, site a to site b
State: Stopped (GitChanged)
Message: step git-commit: branch main of ssh://***@git.example.com/gitops.git is at a2c8b96..., but the run's base is 6dd30f7...: Git changed during the run
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
        started 2026-10-27 10:16:40Z, finished 2026-10-27 10:16:42Z, attempts 1
  19. [Pending] reconcile-git-a  site a
...
History:
  2026-10-27 10:15:00Z  Running  tack v1.3.0, user operator@example.com
  2026-10-27 10:16:42Z  Stopped (GitChanged)  tack v1.3.0, user operator@example.com
```

The switch is done live and the objects are still held: Flux applies nothing to them until the run releases them.

## Stopped and waiting runs

`tack run` and `tack resume` exit with code 3 when the run stops for the operator, and with code 4 when it waits for something outside it. The ledger's reason says why. [Stop reasons](runbooks/stop-reasons.md) says how to go on from each.

| Runbook | For |
|---|---|
| [runbooks/stop-reasons.md](runbooks/stop-reasons.md) | each reason: what stopped, what to check, how to go on |
| [runbooks/no-primary-after-demotion.md](runbooks/no-primary-after-demotion.md) | a planned run that stopped after its demotion and before its promotion: no site takes writes |
| [runbooks/abandon-run.md](runbooks/abandon-run.md) | a run that must not go on, and holds left behind |

Most reasons go on with a plain `tack resume RUN -f workload.yaml` once the cause is fixed. Four decisions have their own flag ([reference/cli.md](reference/cli.md) lists every flag):

| Flag | When | How |
|---|---|---|
| `--accept-git-head SHA` | `GitChanged`; `GitDiffersFromLive` once Git is fixed; `FluxChanged` for an expected change ([runbooks/stop-reasons.md](runbooks/stop-reasons.md)) | Review the commits from the base to the new head, such as `git log --oneline 6dd30f7..a2c8b96`. Then `tack resume RUN -f workload.yaml --accept-git-head a2c8b96`. |
| `--fence SITE` | `FencedSiteIsPrimary`, when its message offers the flag | Check the lost site that came back as primary. Then `tack resume RUN -f workload.yaml --fence a` declares the new primary there. |
| `--take-over RUN` | Another run of the workload can no longer finish, such as when its ledger is at a lost site. | `tack run -f workload.yaml --to b --mode site-loss --take-over RUN` takes over the holds of `RUN` and records it in the new ledger. `RUN` can then never resume. |
| `--force` | `tack abandon RUN` or `tack run --take-over RUN` was refused with `a tack process may still run the run` | Stop the process the refusal names, or check it ended, and wait ([Interrupting a run](#interrupting-a-run)). Use `--force` only after a human checked no `tack` process runs `RUN`. |

- `--accept-git-head`: if the `GitChanged` message names one head, only that commit is accepted, or `HEAD` standing for it, and `tack status RUN` shows it. Otherwise `HEAD` is the branch head when `tack resume` starts: it prints that commit before it asks, and records it. Before the run's commit, the accepted head becomes the run's base. After it, the release checks Git against the accepted head, which must contain the run's commit.
- `--fence`: if no step is left to fence the site, the message says so ([runbooks/stop-reasons.md](runbooks/stop-reasons.md#fencedsiteisprimary)).
- `--take-over`: without it, the new run is refused while the other run is listed and not `Succeeded`, or stops on its holds. Repeat the flag for each run.
- `--force`: the refusal names the process and the time to wait. For a run without a Lease, such as a v1.4.0 run, it names when the ledger was last written. A step in flight in a `tack` process still changes the workload, so check that no process runs `RUN` first.
- A hold set outside Tack stops a run with `HeldByAnotherRun`. It stays until the operator removes it ([runbooks/abandon-run.md](runbooks/abandon-run.md#remove-a-hold-left-behind)).
- After `--accept-git-head`, the check before release reports differences outside the run's own fields instead of stopping on them, and the check after release does not stop on a changed generation ([RFC#0005](../rfcs/RFC0005.md#requirements), Requirements 8 and 10).
- Once its ledger exists, `tack run` searches for other runs again. If another run of the workload started at the same time, the run ends before any step, changes nothing, and is marked abandoned. Plan again once the other run is done.
- `tack resume` refuses a run that another run took over, and a run that another run of the workload started after: the older plan may no longer match the sites. Resume or check the newer run instead. It refuses with exit code 1, before it asks for the workload's name, and searches again after the confirmation. `--yes` skips the confirmation: use it only for a resume that a human approved.

## How long writes stay down

`tack status RUN` prints `Writes down:` from `status.writes`:

| Mode | Writes stopped | Writes came back |
|---|---|---|
| `Planned` | the start of `pooler-<from>`, or of `demote-<from>` without a Pooler | the end of `wait-pooler-<to>`, or of `wait-promoted-<to>` without a Pooler |
| `SiteLoss` | unknown, as the site was lost before the run. Tack records the run's start, marked as an estimate: writes had stopped by then at the latest | as above |

The window ends when the new primary takes writes at its site. Clients that reach it through Strait also wait for Strait's route. To measure what clients see, and to set a budget, use [measure-failover-time.md](runbooks/measure-failover-time.md) in staging.

In a planned switchover, writes stay down from the moment the old Pooler scales to 0 until the new primary is ready. The longest part is often the old primary's shutdown. CloudNativePG lets it last up to the old Cluster's `spec.switchoverDelay`, 3600 seconds by default (v1.30.1). Tack waits for the demotion up to `timeouts.demotion`, 10 minutes by default.

| Case | What happens |
|---|---|
| The shutdown ends within both limits | The demotion token appears, and the run goes on. |
| The shutdown outlasts `timeouts.demotion` | The run stops with `StepFailed` at `wait-demoted-<from>`, while CloudNativePG keeps waiting. Writes stay down. Resume the run: the wait starts again, and goes on once the token appears. |
| The shutdown outlasts `switchoverDelay` | CloudNativePG shuts down at once and publishes no token. The run stops at `wait-demoted-<from>`: [no-primary-after-demotion.md](runbooks/no-primary-after-demotion.md). |

Keep `timeouts.demotion` shorter than `switchoverDelay`, so Tack reports a slow shutdown before CloudNativePG gives up on it. While the new primary replays up to the token, Tack stops the wait early if replay stands still for 2 minutes short of the token ([RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 18). The patch that promotes the new primary stays in place: it promotes once it catches up, and a resume waits again.

## What fencing does

To fence a member means to make it declare another member primary, so that it stops being primary ([RFC0001](../rfcs/RFC0001.md#glossary)). For CloudNativePG, Tack's fence is one patch of the old Cluster: `spec.replica.primary` set to the new primary's Cluster, and `spec.replica.promotionToken` removed (`fence-<from>`, [RFC0004](../rfcs/RFC0004.md#requirements), Requirement 7).

| Tack's fence | Does |
|---|---|
| Changes the old Cluster's spec | yes, once its site's Kubernetes API answers. CloudNativePG then restarts the old primary as a replica |
| Runs while the site is lost | no. The step waits (`Deferred`), and runs on a `tack resume` once the site answers |
| Stops the old primary while its API is unreachable | no |
| Stops clients at the old site from writing to it | no. Applications at that site, and its Pooler until `pooler-<from>` runs, still reach it |
| Cuts the site's network, or stops its nodes | no |
| Rewinds or rebuilds the old primary | no: [rebuild-old-primary.md](runbooks/rebuild-old-primary.md) |

Flux at the old site fences it too, once that site reaches Git and applies the run's commit, which declares the new primary. Until one of these happens, the old primary takes the writes that reach it. Before a site-loss run, the operator fences by other means, outside Tack: the database, and the site's applications or their Pooler ([site-loss.md](runbooks/site-loss.md)).

## Interrupting a run

The first Ctrl-C, SIGTERM or SIGHUP stops the run. The step in flight ends, and the ledger records the stop. That can take up to 30 s when the ledger's site is slow. Recording the stop's Event can take up to 5 s more.

A second signal ends `tack` at once, without that record. The ledger then shows the step `Running`, and `tack resume` runs it again. If the ledger already holds the stop, only the Event is lost.

SIGHUP comes when the terminal or SSH session ends. On a remote host, run `tack` in `tmux` or `screen`.

The process that runs a run holds its Lease and renews it every 10 s ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 28). A process that ends on the first signal releases it. A process ended by a second signal, or killed, leaves it to expire 30 s after its last renewal. Until 10 s after that, `tack abandon` and `tack run --take-over` refuse the run.

## Break-glass

In a site loss, Git may be down too. Tack then switches live and commits later ([RFC#0007](../rfcs/RFC0007.md#design)).

| Step | Command | Result |
|---|---|---|
| 1 | `tack plan -f workload.yaml --to b --mode site-loss` | Works with Git down. The notes say that the Git step waits for Git, and that fields found by `find` are resolved then. |
| 2 | `tack run -f workload.yaml --to b --mode site-loss` | Holds and promotes at b. The steps at the lost site are `Deferred`. Exit code 4, state `Waiting`, reason `GitUnreachable`. |
| 3 | `tack resume RUN -f workload.yaml` once Git answers | Commits, checks Git against live, releases. The run ends `Succeeded`, with the follow-up `Rebuild` for the lost site. |

- While the run waits, the held objects stay held: Flux applies nothing to them, not even unrelated Git changes. A planned run never starts with Git unreachable.
- A git command with no answer ends within 2 minutes and counts as Git unreachable. Over HTTP a stalled transfer ends after 30 s, over SSH a connection that does not connect after 15 s, unless `GIT_SSH_COMMAND`, `GIT_SSH` or `core.sshCommand` names the operator's own ssh command ([RFC#0006](../rfcs/RFC0006.md#requirements), Requirement 3).
- When the lost site answers again, `tack resume` runs its deferred steps: hold, then fence. If the site answers as primary, or declares itself primary, the run first stops with `FencedSiteIsPrimary` and changes nothing there until the operator passes `--fence`. The steps stay `Deferred`.
- `--fence` applies only to the resume it is passed to. Each `tack run` and `tack resume` reads the site again before its first step there, and stops while it answers as primary. In every mode, each also checks before every Failover step after the old primary's demotion or fence, the promotion included: it reads every site except the new primary, and stops while one that answers is primary ([RFC#0007](../rfcs/RFC0007.md#requirements), Requirement 9).
- A run that already succeeded with deferred steps runs them on a later `tack resume`. If the site comes back after the commit and no resume runs, Flux at that site applies Git, which already declares the new primary. A later resume holds and releases the site at the run's commit. If Flux applied a later commit there, the resume stops with `GitChanged` until the operator accepts that head.

## After a site loss

The old primary may hold writes that the new one lacks, and it cannot follow the new one. Rebuild it with [runbooks/rebuild-old-primary.md](runbooks/rebuild-old-primary.md). Until it is rebuilt, `tack plan` refuses a planned switchover back to it.
