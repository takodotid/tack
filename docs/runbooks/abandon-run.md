# Abandon a run

A run is abandoned when it must not go on, such as after its old primary was promoted again by hand ([no-primary-after-demotion.md](no-primary-after-demotion.md)). Tack has no command for it. The run stays in its ledger, and the next run takes it over.

| Name | Meaning |
|---|---|
| `RUN`, `T` | the run to abandon, and its `to` site, where its ledger is |
| `SITE_CTX` | the kube context of a site with a held object |
| `KIND`, `NAME`, `NS` | a held object: its kind, name and namespace |
| `KS`, `FLUX_NS` | the Flux Kustomization that applies it, and its namespace |
| `CHECKOUT`, `BRANCH`, `FILE` | a clone of the Workload's Git repository, its branch, and a file the run changes |

## Steps

1. **No `tack` process runs `RUN`.** Ask every operator and every script that runs `tack`.
2. **Make Git equal live** in every field the run changes: change live through the workload's own API, as option 2 of [no-primary-after-demotion.md](no-primary-after-demotion.md) does, or commit live's values to Git.
3. **Remove its holds,** below.
4. **Never resume `RUN`.** Its plan no longer fits the sites. Before each patch, a resume checks that the run still holds the object, and stops with `StepFailed` when it does not: `... is no longer held by this run`. Do not hold the object again as that message says. A Hold step that has not run yet would still hold its object.
5. **The next run names it.** `tack run` refuses while `RUN` is not `Succeeded`, with a message such as `run RUN at site T is Stopped: resume it (tack resume RUN) or take it over (--take-over RUN)`. Start the next run with `--take-over RUN`. It records the take-over, writes its name into `status.supersededBy` of `RUN`'s ledger when that site answers, and `RUN` can never resume after that.
6. **Keep the ledger.** It is the record of the run. A `Rebuild` follow-up in it keeps a planned run off a member that diverged ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 14).

## Remove a hold left behind

A hold is left behind when no run will release it: its run is abandoned, or its ledger is gone. While held, the object gets nothing from Flux, including later Git changes. Remove a hold only once Git equals live in every field the run changed: otherwise Flux applies Git's old values, and undoes the switch.

`tack status RUN -f workload.yaml` lists the run's objects under `Held objects`, with `(held)` for those still held. An object's annotations show the hold:

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
