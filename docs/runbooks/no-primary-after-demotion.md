# No primary after demotion

A planned run from site F to site T demoted F, then stopped or ended before T runs as primary. No site takes writes.

Names: [stop-reasons.md](stop-reasons.md#names), and `F_POOLER`, F's read-write Pooler.

## First

Every object of the run is held at both sites, so Flux applies nothing to them. Git still declares `F_CLUSTER` primary: the run commits only after the promotion.

| Check | How | If not |
|---|---|---|
| No `tack` process runs `RUN`. | [First](stop-reasons.md#first) | Let the process end. |
| The run stopped at the demotion. | `tack status RUN -f workload.yaml` shows `Stopped` with any reason, or `Running` with no `tack` process left. Fix the reason first: [stop-reasons.md](stop-reasons.md). | |
| F is demoted. | The [primary check](stop-reasons.md#primary-check) at F prints `T_CLUSTER`, and `false` for each instance. F's read-write Pooler runs no pod. | F is still primary, and this runbook does not apply. Resume the run. |
| T has not promoted. | `kubectl --context T_CTX -n NS get cluster T_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.lastPromotionToken} {.status.instancesReportedState.*.isPrimary}{"\n"}'` prints `F_CLUSTER`, nothing, `false`. | T has promoted or is promoting: take option 1. Never promote F by hand then: that gives two primaries. |

The last step that started is one of `demote-F`, `wait-demoted-F`, `capture-token`, `promote-T`, `wait-promoted-T`.

## Options

| Option | When | Writes lost |
|---|---|---|
| 1. Resume | T answers, or once it does; and F answers, or `capture-token` is `Succeeded`. | None. T replays up to F's demotion token, then promotes with it. |
| 2. Promote F again, by hand | Both sites answer, T has not promoted, and the switch must not go on now. | None. F holds every write, and T never took one. |
| 3. A site-loss run back to F | T does not answer, so `tack resume` cannot find the run (its ledger is at T). Writes must come back before T does. | None at F. If T may have promoted (for example the run's last output was `start promote-T`), the writes T took after that are lost. |
| 4. A site-loss run to T | There is no token: F does not answer and `capture-token` is not `Succeeded`, or F never published a token. The switch must go on. | The writes F made that T did not replay. |

1. Resume: `tack resume RUN -f workload.yaml`. If F is down, T promotes and its Pooler starts. The run then stops with `StepFailed` at `reconcile-git-F` until F answers: resume again then.
2. Promote F again: follow the steps below, then abandon the run ([abandon-run.md](abandon-run.md)). Once F is primary again, a resume stops at once with `FencedSiteIsPrimary`, also with `--fence F`.
3. Back to F: do the checks of [site-loss.md](site-loss.md), then `tack run -f workload.yaml --to F --mode site-loss --take-over RUN`. T's steps wait for T, and once T answers, `tack resume` of the new run fences it. The new run records the follow-up `Rebuild` for T, even when T never promoted, since Tack cannot tell. Rebuild T before any planned run back to it ([rebuild-old-primary.md](rebuild-old-primary.md)).
4. To T: do the checks of [site-loss.md](site-loss.md), then `tack run -f workload.yaml --to T --mode site-loss --take-over RUN`. F needs a rebuild.

## Option 2: promote F again

1. Declare F primary. CloudNativePG promotes a Cluster that declares itself primary at once, without a token ([RFC#0004](../../rfcs/RFC0004.md#motivation), Motivation).

   ```sh
   kubectl --context F_CTX -n NS patch cluster F_CLUSTER --type merge -p '{"spec":{"replica":{"primary":"F_CLUSTER"}}}'
   kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.status.instancesReportedState.*.isPrimary} {.status.phase}{"\n"}'
   ```

   Repeat the second command until one instance prints `true` and the phase is `Cluster in healthy state`.

2. Start F's Pooler again, with its instance count from Git, `N`:

   ```sh
   kubectl --context F_CTX -n NS patch pooler F_POOLER --type merge -p '{"spec":{"instances":N}}'
   ```

3. Release the holds once Git equals live, then abandon the run: [abandon-run.md](abandon-run.md).

T needs no change except its holds: it still declares `F_CLUSTER`.
