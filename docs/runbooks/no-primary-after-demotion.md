# No primary after demotion

A planned run from site F to site T stopped, or its process ended, after it demoted F and before T runs as primary. No site takes writes. This runbook brings writes back.

| Name | Meaning |
|---|---|
| `RUN` | the run, as `tack status -f workload.yaml` lists it |
| `F`, `T` | the run's `from` and `to` sites; `F_CTX`, `T_CTX` their kube contexts |
| `F_CLUSTER`, `T_CLUSTER`, `F_POOLER`, `NS` | the CloudNativePG Clusters, F's read-write Pooler and their namespace, from the Workload file |

## State

| Where | What it shows |
|---|---|
| `tack status RUN -f workload.yaml` | `Stopped` with any reason, or `Running` with no `tack` process left. The last step that started is one of `demote-F`, `wait-demoted-F`, `capture-token`, `promote-T`, `wait-promoted-T`. |
| Site F | Its Cluster declares `T_CLUSTER` primary, and no instance reports `isPrimary`. Its read-write Pooler runs no pod. `status.demotionToken` is set once the demotion ended. |
| Site T | Its Cluster still declares `F_CLUSTER`, and no instance reports `isPrimary`, until `promote-T` runs. |
| Holds | Every object of the run is held at both sites: Flux applies nothing to them. |
| Git | Unchanged: it still declares `F_CLUSTER` primary. The run commits after the promotion. |

## First

| Check | How | If not |
|---|---|---|
| No `tack` process runs `RUN`. | Ask every operator and every script that runs `tack`. The ledger cannot tell a live process from a dead one ([RFC#0003](../../rfcs/RFC0003.md#limitations), Limitations). | Let it end. |
| F is demoted. | `kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'` prints `T_CLUSTER` and `false` for each instance. | F is still primary, and this runbook does not apply: resume. |
| T has not promoted. | `kubectl --context T_CTX -n NS get cluster T_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.lastPromotionToken} {.status.instancesReportedState.*.isPrimary}{"\n"}'` prints `F_CLUSTER`, nothing, `false`. | T has promoted, or is promoting: take option 1. Never promote F by hand then: two primaries. |
| The run's reason. | `tack status RUN -f workload.yaml`. Fix its cause first: [stop-reasons.md](stop-reasons.md). | |

## Options

| Option | When | Writes lost | Then |
|---|---|---|---|
| 1. Resume | T answers, or once it does. F answers too, or `capture-token` is `Succeeded`. | None: T promotes with F's demotion token, after replaying up to it. | `tack resume RUN -f workload.yaml`. With F down, T promotes and its Pooler starts, then the run stops with `StepFailed` at `reconcile-git-F` until F answers; resume then. |
| 2. Promote F again, by hand | Both sites answer, T has not promoted, and the switch must not go on now. | None: F holds every write, and T never took one. | Below. Then abandon the run: [abandon-run.md](abandon-run.md). Once F is primary again, a resume stops before `promote-T` with `FencedSiteIsPrimary`, also with `--fence F`. |
| 3. A site-loss run back to F | T does not answer. The ledger is at T, so `tack resume` cannot find the run. Writes must come back before T does. | None at F. If T may have promoted, such as when the run's last output was `start promote-T`, writes T took after that are lost. | `tack run -f workload.yaml --to F --mode site-loss --take-over RUN`, after [site-loss.md](site-loss.md). T's steps wait for it; `tack resume` of the new run fences T once it answers. The new run records the follow-up `Rebuild` for T, even when T never promoted, since Tack cannot tell: rebuild T before a planned run back to it: [rebuild-old-primary.md](rebuild-old-primary.md). |
| 4. A site-loss run to T | There is no token: F does not answer and `capture-token` is not `Succeeded`, or F never published one. The switch must go on. | Writes F made that T did not replay. | `tack run -f workload.yaml --to T --mode site-loss --take-over RUN`, after [site-loss.md](site-loss.md). F needs a rebuild. |

## Option 2: promote F again

1. **Declare F primary.** CloudNativePG promotes a Cluster that declares itself primary at once, without a token ([RFC#0004](../../rfcs/RFC0004.md#motivation), Motivation).

   ```sh
   kubectl --context F_CTX -n NS patch cluster F_CLUSTER --type merge -p '{"spec":{"replica":{"primary":"F_CLUSTER"}}}'
   kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.status.instancesReportedState.*.isPrimary} {.status.phase}{"\n"}'
   ```

   Repeat the second command until one instance prints `true` and the phase is `Cluster in healthy state`.

2. **Start F's Pooler again,** at its value in Git, `N`:

   ```sh
   kubectl --context F_CTX -n NS patch pooler F_POOLER --type merge -p '{"spec":{"instances":N}}'
   ```

3. **Release the holds** once Git equals live, then abandon the run: [abandon-run.md](abandon-run.md).

T needs no change but its holds: it still declares `F_CLUSTER`, and the run patches nothing at T before the promotion.
