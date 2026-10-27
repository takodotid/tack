# Operations

## Reading a run

| Command | Shows |
|---|---|
| `tack status -f workload.yaml` | the workload's runs at every reachable site: site, run, mode, from, to, state, reason, start |
| `tack status RUN -f workload.yaml` | one run: state and reason, message, Git base and commit, captured values by name, held objects, each step with its state and last result, follow-ups |
| `tack status [RUN] --context site-b` | the same from one context, without the Workload file; `--namespace` when the ledger namespace is not `tack-system` |
| `tack status RUN -f workload.yaml -o yaml` | the whole `SiteFailover` object |
| `kubectl --context site-b -n tack-system get sfo` | the ledgers at one site, with workload, mode, from, to, state and phase |

A run's state is `Running`, `Waiting`, `Stopped` or `Succeeded`; each step's state is `Pending`, `Running`, `Succeeded`, `Failed` or `Deferred` ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirements 9 and 11). A run left `Running` by a process that ended, such as a killed terminal, goes on with `tack resume`.

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
  site a: Pooler db/pg-a-rw (held)
  site b: Cluster db/pg-b (held)
  site b: Pooler db/pg-b-rw (held)
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

Most reasons go on with a plain `tack resume RUN -f workload.yaml` once the cause is fixed. Three decisions have their own flag:

| Flag | When | Example |
|---|---|---|
| `--accept-git-head SHA` | The run stopped with `GitChanged`. | Review the commits between the base and the new head named in the message, such as `git log --oneline 6dd30f7..a2c8b96`, then `tack resume RUN -f workload.yaml --accept-git-head a2c8b96`. |
| `--fence SITE` | The run stopped with `FencedSiteIsPrimary`. | A lost site came back and answers as primary. Check it, then `tack resume RUN -f workload.yaml --fence a` declares the new primary there. |
| `--take-over RUN` | Another run of the workload can no longer finish, such as when its ledger is at a lost site. Without the flag, the new run is refused while that run is listed and not `Succeeded`, or stops on its holds. | Pass it when starting the new run: `tack run -f workload.yaml --to b --mode site-loss --take-over RUN` takes over the holds of `RUN`. |

A hold set outside Tack, which stops a run with `HeldByAnotherRun`, stays until the operator removes it: Tack never removes a hold it did not set.

After `--accept-git-head`, the check before release reports differences outside the run's own fields instead of stopping, and the check after release does not stop on a changed generation ([RFC#0005](../rfcs/RFC0005.md#requirements), Requirements 8 and 10).

`tack resume` asks for the workload's name like `tack run`. `--yes` skips that, only for a resume a human approved.

## Break-glass

In a site loss, Git may be down too. Tack then switches live and commits later. The sequence is in [RFC#0007](../rfcs/RFC0007.md#design).

| Step | Command | Result |
|---|---|---|
| 1 | `tack plan -f workload.yaml --to b --mode site-loss` | Works with Git down. The notes say the Git step waits for Git, and fields found by `find` are resolved then. |
| 2 | `tack run -f workload.yaml --to b --mode site-loss` | Holds and promotes at b; the steps at the lost site are `Deferred`. Exit code 4, state `Waiting`, reason `GitUnreachable`. |
| 3 | `tack resume RUN -f workload.yaml` once Git answers | Commits, checks Git against live, releases. The run ends `Succeeded`, with the follow-up `Rebuild` for the lost site. |

While the run waits, the held objects stay held: Flux applies nothing to them, including unrelated Git changes. A planned run never starts with Git unreachable.

When the lost site answers again, `tack resume` runs its deferred steps: hold, then fence. If the site answers as primary, the run stops with `FencedSiteIsPrimary` first and changes nothing there until the operator passes `--fence`. A run that already succeeded with deferred steps runs them on a later `tack resume`. If the site comes back after the commit and no resume runs, Flux at that site applies Git, which already declares the new primary.

## After a site loss

The old primary may hold writes the new one lacks, and cannot follow it. Rebuild it with [docs/runbooks/rebuild-old-primary.md](runbooks/rebuild-old-primary.md). Until it is rebuilt, `tack plan` refuses a planned switchover back to it.

## Permissions

Tack acts with the operator's credentials for each site's kube context. Per site, it needs:

| For | Listed in |
|---|---|
| The ledger: `SiteFailover` objects in the ledger namespace | [RFC#0003](../rfcs/RFC0003.md#security-considerations), Security Considerations |
| Holding and releasing objects, Flux reconciles, rendering and diffing Git | [RFC#0005](../rfcs/RFC0005.md#security-considerations), Security Considerations |
| The Pooler waits: `get` on the `apps/v1` Deployment named after each Pooler | [RFC#0004](../rfcs/RFC0004.md#requirements), Requirement 6 |
| The routing check: `get` on Strait's `SiteService` | [RFC#0008](../rfcs/RFC0008.md#requirements), Requirement 1 |

For Git, the operator's `git` configuration needs read access for `tack plan`, and push access to the branch for `tack run`. Tack pushes without force and skips client-side hooks; server-side rules still apply ([RFC#0006](../rfcs/RFC0006.md#requirements)).
