# Old ledgers

Each search for runs reads every ledger of the workload at each answering site, in pages of 100 ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 26). `tack plan` and `tack status -f` search once, `tack resume` three times, `tack run` four times. The cost grows with the number of runs.

| Measured | Size |
|---|---|
| Ledger of a planned run, 2 sites / 10 sites | 21 KB / 86 KB of JSON |
| Ledger with a full history (100 entries of 1024 bytes), 2 sites / 10 sites | 138 KB / 204 KB |
| Search at one site, 1,000 ledgers of 13.5 KB | 13.5 MB, in 10 answers of 1.35 MB |
| List of `tack status -f` after its search, 10,000 runs | 28 ms |

## Prune

`tack prune -f workload.yaml --keep N` deletes the ledgers of old runs when their number slows the searches. It lists every run of the workload, newest first, each with `delete` or the reason it is kept. It then asks for the workload's name, searches again, and deletes one ledger at a time, oldest first. It needs `delete` on sitefailovers, and `get` on clusters to delete a run with a follow-up `Rebuild` ([Permissions](permissions.md)). The rules ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 27):

| Rule | Why |
|---|---|
| Every site answers, and every run of the workload at every site is finished: `Succeeded` with no `Deferred` step, `Abandoned` or `TakenOver`. Otherwise prune deletes nothing. | A deleted newer run lets an older unfinished run resume on a plan that no longer fits the sites ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 18). |
| Delete only a `Succeeded` run with no `Deferred` step. Keep `Abandoned` and `TakenOver` runs. | They record holds that may remain, and the take-over that ended them ([runbooks/abandon-run.md](runbooks/abandon-run.md), step 6). [Delete one by hand](#delete-a-run-by-hand). |
| Keep the `--keep` newest runs that succeeded (at least 1; abandoned and taken-over runs do not count). | The newest successful run records the last switch. |
| Delete a run only once `--keep` of them were certainly created after it: at its own site in a later second, at another site 10 s or more later. | Each site's API server sets creation time, to the second, by its own clock. |
| Otherwise keep the run as `may be one of the N newest`, and name the run the clocks cannot order it against. | Prune cannot tell which of the two runs is newer. |
| Keep a run whose spec takes over a run that is kept, not abandoned, and not marked `status.supersededBy`. | Without it, that run would block every new run. |
| Keep a run that a newer `tack` last wrote (`status.writtenBy`). A `tack` with no release version, such as `dev`, keeps every run a release wrote. | A newer release may need what it recorded. |
| Keep a run with a follow-up `Rebuild` until its site's member, as the Workload file names it, was created again 10 s or more after the run's ledger (`metadata.creationTimestamp`). | `tack plan` refuses a switchover to that member while such a run says it diverged ([RFC#0003](../rfcs/RFC0003.md#requirements), Requirement 14). The 10 s cover the clock difference between two sites' API servers. |
| Read the member only for such a run, and keep the run when it cannot be read. | The row above needs the member's creation time. |
| Delete each ledger by name, only while it is still the object that was read. Stop at the first error. | A run or ledger written during the confirmation is not deleted from a stale list. |

## Delete a run by hand

For an abandoned or taken-over run whose record no longer matters, check each row. `RUN` is the run, `T_CTX` its site's context, `tack-system` the ledger namespace:

| Check | How |
|---|---|
| No object names `RUN` as its holder. | `tack status RUN -f workload.yaml` lists each object the run held. For each, the holder (below) must be something other than `RUN`. Otherwise [remove the hold first](runbooks/abandon-run.md#remove-a-hold-left-behind). |
| No follow-up `Rebuild`, or that site's member was created again 10 s or more after the run's ledger. | `tack status RUN -f workload.yaml` lists the follow-ups. Compare the ledger's creation time with the member's (below). |
| Its spec takes over no listed run that is neither abandoned nor marked `status.supersededBy`. | Look up each run the spec names (below) with `tack status`. Deleting `RUN` would make such a run block every new run. |

```sh
# holder of an object the run held
kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations.failover\.tako\.id/held-by}{"\n"}'
# creation time of the ledger, then of the member
kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.metadata.creationTimestamp}{"\n"}'
kubectl --context SITE_CTX -n NS get cluster MEMBER -o jsonpath='{.metadata.creationTimestamp}{"\n"}'
# runs the spec takes over
kubectl --context T_CTX -n tack-system get sfo RUN -o jsonpath='{.spec.takeOver} {.spec.takeOvers}{"\n"}'
```

```sh
kubectl --context T_CTX -n tack-system delete sfo RUN
```
