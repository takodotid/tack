# Abandon a run

Abandon a run when it must not go on. One case: its old primary was promoted again by hand ([no-primary-after-demotion.md](no-primary-after-demotion.md)). `tack abandon` marks the run in its ledger. The run stays there as a record.

| Name | Meaning |
|---|---|
| `RUN`, `T`, `T_CTX` | the run to abandon; its `to` site, which holds its ledger and its Lease; and that site's kube context |
| `SITE_CTX` | the kube context of a site with a held object |
| `KIND`, `NAME`, `NS` | a held object: its kind, name and namespace |
| `KS`, `FLUX_NS` | the Flux Kustomization that applies it, and its namespace |
| `CHECKOUT`, `BRANCH`, `FILE` | a clone of the Workload's Git repository, its branch, and a file the run changes |

## Steps

1. **Make sure no `tack` process runs `RUN`.** Ask every operator, and check every script that runs `tack`. The process that runs a run holds its Lease. The Lease names the process and its host: `kubectl --context T_CTX -n tack-system get lease RUN -o jsonpath='{.spec.holderIdentity} {.spec.renewTime}{"\n"}'`. No holder means the process ended.
2. **Make Git equal live** in every field the run changes. Either change live through the workload's own API, as option 2 of [no-primary-after-demotion.md](no-primary-after-demotion.md) does, or commit live's values to Git.
3. **Remove the run's holds,** as below.
4. **Mark the run abandoned:** `tack abandon RUN -f workload.yaml --reason TEXT`.
   - **Before:** only tack v1.3.0 and later honour the mark. An older `tack resume` resumes the run and drops the mark. So every operator's CLI must be on v1.3.0 first.
   - **What it does:** it prints the run and the objects that its ledger records as held. It asks for the workload's name. Then it records `status.abandoned`.
   - It does not read the objects. So a hold removed in step 3 is still listed.
   - **When it refuses:** while a `tack` process may still run `RUN`. That is, the run's Lease was renewed less than 40 s ago. For a run without a Lease, such as one that v1.4.0 runs: the run is `Running`, and its ledger was written within its `Running` step's timeout and 5 minutes. The refusal says until when. Pass `--force` only once step 1 is done.
   - **With a CRD older than v1.3.0 at `T`,** it fails and says so. Apply the CRD of [Upgrade](../guide/upgrade.md#upgrade-within-v1) there first.
   - **After the mark:** `tack resume` refuses `RUN`, and `tack status` lists it as `Abandoned`. `kubectl get sfo` shows the mark's age in its Abandoned column, with the CRD of v1.4.0 or later at `T`. Its State column keeps the run's last state, such as `Stopped` ([operations.md](../operations.md#reading-a-run)).
   - The mark does not stop a `tack` process. That is why step 1 comes first.
   - **Without the mark, never resume `RUN`:** its plan no longer fits the sites. If `RUN` is resumed anyway:
     - While another site answers as primary, a resume stops with `FencedSiteIsPrimary`. It stops before its next Failover step after the old primary's demotion or fence, `promote-T` among them.
     - Before each patch, a resume checks that the run still holds the object. When it does not, it stops with `StepFailed`: `... is no longer held by this run`, which says the plan may be stale. Do not hold the object again. A Hold step that has not run yet would still hold its object.
5. **Start the next run.** Once `RUN` is marked, `tack run` no longer waits for it.
   - An object that `RUN` still holds stops the next run with `HeldByAnotherRun`. Either remove the hold (step 3), or start the next run with `--take-over RUN`, which takes the holds over.
   - Without the mark, `tack run` refuses while `RUN` is not `Succeeded`, with a message such as `run RUN at site T is Stopped: resume it (tack resume RUN) or take it over (--take-over RUN)`. Start the next run with `--take-over RUN`.
   - A take-over is recorded. The new run writes its name into `status.supersededBy` of `RUN`'s ledger when that site answers. After that, `RUN` can never resume.
6. **Keep the ledger.** It is the record of the run. A `Rebuild` follow-up in it keeps a planned run off a member that diverged ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 14).

## Remove a hold left behind

A hold is left behind when no run will release it: its run is abandoned, or its ledger is gone. While an object is held, Flux applies nothing to it, not even later Git changes. Remove a hold only once Git equals live in every field the run changed. Otherwise Flux applies Git's old values, and undoes the switch.

`tack status RUN -f workload.yaml` lists the run's objects under `Held objects, as the ledger records them`. It marks with `(held)` those the run did not release. The ledger does not see a hold removed by hand. The object's annotations show the hold:

```sh
kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations}{"\n"}'
```

Per object:

1. **Check that Git equals live.** The `git-commit` step of `tack status RUN -f workload.yaml` lists each field the run changes: the file, the path in it, and the object and field. For each field, compare the value at the branch head with live. Use the field's path as the JSONPath, such as `{.spec.replica.primary}`:

   ```sh
   git -C CHECKOUT fetch
   git -C CHECKOUT show origin/BRANCH:FILE | kubectl --context SITE_CTX create --dry-run=client -f - -o jsonpath='{.spec.replica.primary}{"\n"}'
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.spec.replica.primary}{"\n"}'
   ```

   A removed field prints nothing on both sides. Fix Git, or fix live through the workload's API, until they match. `flux diff kustomization` cannot check this: it reports a held object as `skipped`.

2. **Note the generation, then remove both annotations:**

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n NS annotate KIND NAME kustomize.toolkit.fluxcd.io/reconcile- failover.tako.id/held-by-
   ```

3. **Reconcile:**

   ```sh
   flux --context SITE_CTX -n FLUX_NS reconcile kustomization KS --with-source
   ```

4. **Check that Flux changed nothing.** The generation must be the one printed in step 2. The Kustomization must be `Ready` with reason `ReconciliationSucceeded`:

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n FLUX_NS get kustomization KS -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
   ```

   A new generation means Flux changed the object. Compare the run's fields again, as in step 1.

| Fact | Detail |
|---|---|
| A hold without `failover.tako.id/held-by`, or `kustomize.toolkit.fluxcd.io/reconcile: disabled` set as a label | It was set outside Tack. Find out who set it and why before removing it. |
| Permissions | `patch` on the held kind, for the annotation. `get` and `patch` on the Kustomization and its GitRepository, for the reconcile. These are rows 8, 10 and 11 of [operations.md](../operations.md#permissions). |
