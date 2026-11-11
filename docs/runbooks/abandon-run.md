# Abandon a run

A run is abandoned when it must not go on, such as after its old primary was promoted again by hand ([no-primary-after-demotion.md](no-primary-after-demotion.md)). `tack abandon` marks it in its ledger. The run stays there as a record.

| Name | Meaning |
|---|---|
| `RUN`, `T`, `T_CTX` | the run to abandon, its `to` site, where its ledger and its Lease are, and that site's kube context |
| `SITE_CTX` | the kube context of a site with a held object |
| `KIND`, `NAME`, `NS` | a held object: its kind, name and namespace |
| `KS`, `FLUX_NS` | the Flux Kustomization that applies it, and its namespace |
| `CHECKOUT`, `BRANCH`, `FILE` | a clone of the Workload's Git repository, its branch, and a file the run changes |

## Steps

1. **No `tack` process runs `RUN`.** Ask every operator and every script that runs `tack`. The process that runs a run holds its Lease, which names the process and its host: `kubectl --context T_CTX -n tack-system get lease RUN -o jsonpath='{.spec.holderIdentity} {.spec.renewTime}{"\n"}'`. No holder means its process ended.
2. **Make Git equal live** in every field the run changes: change live through the workload's own API, as option 2 of [no-primary-after-demotion.md](no-primary-after-demotion.md) does, or commit live's values to Git.
3. **Remove its holds,** below.
4. **Mark it abandoned:** `tack abandon RUN -f workload.yaml --reason TEXT`. It prints the run and the objects that its ledger records as held, asks for the workload's name, and records `status.abandoned`. It does not read the objects: a hold removed in step 3 is still listed. It refuses while a `tack` process may still run `RUN`: its Lease was renewed less than 40 s ago, or, for a run without a Lease, such as one that v1.4.0 runs, the run is `Running` and its ledger was written within its `Running` step's timeout and 5 minutes. The refusal says until when. Pass `--force` only once step 1 is done. `tack resume` then refuses `RUN`, and `tack status` lists it as `Abandoned`. `kubectl get sfo` shows the mark's age in its Abandoned column, with the CRD of v1.4.0 or later at `T`, and keeps the run's last state, such as `Stopped`, in its State column ([operations.md](../operations.md#reading-a-run)). With a CRD older than v1.3.0 at `T`, it fails and says so: apply the CRD of [the README](../../README.md#upgrading-within-v1) there first. Only tack v1.3.0 and later honour the mark: an older `tack resume` resumes the run and drops the mark, so every operator's CLI must be on v1.3.0 first. The mark does not stop a `tack` process: step 1 comes first. Without the mark, never resume `RUN`: its plan no longer fits the sites. While another site answers as primary, a resume stops with `FencedSiteIsPrimary` before its next Failover step after the old primary's demotion or fence, `promote-T` among them. Before each patch, it checks that the run still holds the object, and stops with `StepFailed` when it does not: `... is no longer held by this run`, which says the plan may be stale. Do not hold the object again. A Hold step that has not run yet would still hold its object.
5. **The next run.** Once `RUN` is marked, `tack run` no longer waits for it. An object `RUN` still holds stops the next run with `HeldByAnotherRun`: remove the hold (step 3), or start the next run with `--take-over RUN`, which takes the holds over. Without the mark, `tack run` refuses while `RUN` is not `Succeeded`, with a message such as `run RUN at site T is Stopped: resume it (tack resume RUN) or take it over (--take-over RUN)`; start the next run with `--take-over RUN`. It records the take-over, writes its name into `status.supersededBy` of `RUN`'s ledger when that site answers, and `RUN` can never resume after that.
6. **Keep the ledger.** It is the record of the run. A `Rebuild` follow-up in it keeps a planned run off a member that diverged ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 14).

## Remove a hold left behind

A hold is left behind when no run will release it: its run is abandoned, or its ledger is gone. While held, the object gets nothing from Flux, including later Git changes. Remove a hold only once Git equals live in every field the run changed: otherwise Flux applies Git's old values, and undoes the switch.

`tack status RUN -f workload.yaml` lists the run's objects under `Held objects, as the ledger records them`, with `(held)` for those the run did not release. The ledger does not see a hold removed by hand. An object's annotations show the hold:

```sh
kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations}{"\n"}'
```

Per object:

1. **Git equals live.** The `git-commit` step of `tack status RUN -f workload.yaml` lists each field the run changes: the file, the path in it, and the object and field. For each field, compare the value at the branch head with live, with the field's path as the JSONPath, such as `{.spec.replica.primary}`:

   ```sh
   git -C CHECKOUT fetch
   git -C CHECKOUT show origin/BRANCH:FILE | kubectl --context SITE_CTX create --dry-run=client -f - -o jsonpath='{.spec.replica.primary}{"\n"}'
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.spec.replica.primary}{"\n"}'
   ```

   A removed field prints nothing on both sides. Fix Git, or live through the workload's API, until they match. `flux diff kustomization` cannot check this: it reports a held object as `skipped`.

2. **Remove both annotations:**

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n NS annotate KIND NAME kustomize.toolkit.fluxcd.io/reconcile- failover.tako.id/held-by-
   ```

3. **Reconcile:**

   ```sh
   flux --context SITE_CTX -n FLUX_NS reconcile kustomization KS --with-source
   ```

4. **Check that Flux changed nothing:** the generation is the one printed in step 2, and the Kustomization is `Ready` with reason `ReconciliationSucceeded`:

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n FLUX_NS get kustomization KS -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
   ```

   A new generation means Flux changed the object: compare the run's fields again, as in step 1.

| Fact | Detail |
|---|---|
| A hold without `failover.tako.id/held-by`, or `kustomize.toolkit.fluxcd.io/reconcile: disabled` as a label | Set outside Tack. Find who set it and why before removing it. |
| Permissions | `patch` on the held kind for the annotation, `get` and `patch` on the Kustomization and its GitRepository for the reconcile: rows 8, 10 and 11 of [operations.md](../operations.md#permissions). |
