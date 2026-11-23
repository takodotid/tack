# Abandon a run

Abandon a run when it must not go on, such as when its old primary was promoted again by hand ([no-primary-after-demotion.md](no-primary-after-demotion.md)). `tack abandon` marks the run in its ledger, which stays as a record.

Names: [stop-reasons.md](stop-reasons.md#names), and these:

| Name | Meaning |
|---|---|
| `KS`, `FLUX_NS` | the Flux Kustomization that applies a held object, and its namespace |
| `CHECKOUT`, `BRANCH`, `FILE` | a clone of the Workload's Git repository, its branch, and a file the run changes |

## Steps

1. Make sure no `tack` process runs `RUN`: [First](stop-reasons.md#first).
2. Make Git equal live in every field the run changes: change live through the workload's own API, as option 2 of [no-primary-after-demotion.md](no-primary-after-demotion.md) does, or commit live's values to Git.
3. Remove the run's holds, as below.
4. Mark the run abandoned: `tack abandon RUN -f workload.yaml --reason TEXT`.
   - It prints the run and the objects its ledger records as held, asks for the workload's name, then records `status.abandoned`. It does not read the objects, so a hold removed in step 3 is still listed.
   - It refuses while the run's Lease is fresh ([operations.md](../operations.md#interrupting-a-run)), and says until when. Pass `--force` only after step 1.
   - It needs tack and the CRD at `T` of v1.3.0 or later. An older `tack resume` resumes the run and drops the mark: [Upgrade](../guide/upgrade.md#upgrade-within-v1).
   - After the mark, `tack resume` refuses `RUN`, and `tack status` lists it as `Abandoned` ([operations.md](../operations.md#reading-a-run)). The mark does not stop a `tack` process, hence step 1.
5. Start the next run. Once `RUN` is marked, `tack run` no longer waits for it.
   - An object `RUN` still holds stops the next run with `HeldByAnotherRun`. Remove the hold (step 3), or pass `--take-over RUN`, which takes the holds over.
   - Without the mark, `tack run` refuses while `RUN` is not `Succeeded`: `run RUN at site T is Stopped: resume it (tack resume RUN) or take it over (--take-over RUN)`. Pass `--take-over RUN`.
   - A take-over writes the new run's name into `status.supersededBy` of `RUN`'s ledger when that site answers. After that, `RUN` can never resume.
6. Keep the ledger. A `Rebuild` follow-up in it keeps a planned run off a member that diverged ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 14).

Without the mark, never resume `RUN`: its plan no longer fits the sites. A resume stops with `FencedSiteIsPrimary` while another site answers as primary, or with `StepFailed` (`... is no longer held by this run`) when a hold is gone ([stop-reasons.md](stop-reasons.md#fencedsiteisprimary)). Do not hold the object again: a Hold step that has not run yet would still hold its object.

## Remove a hold left behind

A hold is left behind when no run will release it: its run is abandoned, or its ledger is gone. Remove it only once Git equals live in every field the run changed. Otherwise Flux applies Git's old values and undoes the switch.

`tack status RUN -f workload.yaml` lists the run's objects under `Held objects, as the ledger records them`, and marks with `(held)` those the run did not release. The ledger does not see a hold removed by hand; the object's annotations show it:

```sh
kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.annotations}{"\n"}'
```

A hold without `failover.tako.id/held-by`, or with `kustomize.toolkit.fluxcd.io/reconcile: disabled` set as a label, was set outside Tack. Find out who set it and why before removing it.

Per object:

1. Check that Git equals live. The `git-commit` step of `tack status RUN -f workload.yaml` lists each field the run changes: file, path, object and field. Compare each field's value at the branch head with live, using the path as the JSONPath, such as `{.spec.replica.primary}`:

   ```sh
   git -C CHECKOUT fetch
   git -C CHECKOUT show origin/BRANCH:FILE | kubectl --context SITE_CTX create --dry-run=client -f - -o jsonpath='{.spec.replica.primary}{"\n"}'
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.spec.replica.primary}{"\n"}'
   ```

   A removed field prints nothing on both sides. Fix Git, or fix live through the workload's API, until they match. `flux diff kustomization` cannot check this: it reports a held object as `skipped`.

2. Note the generation, then remove both annotations:

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n NS annotate KIND NAME kustomize.toolkit.fluxcd.io/reconcile- failover.tako.id/held-by-
   ```

3. Reconcile:

   ```sh
   flux --context SITE_CTX -n FLUX_NS reconcile kustomization KS --with-source
   ```

4. Check that Flux changed nothing. The generation must match step 2. The Kustomization must be `Ready` with reason `ReconciliationSucceeded`:

   ```sh
   kubectl --context SITE_CTX -n NS get KIND NAME -o jsonpath='{.metadata.generation}{"\n"}'
   kubectl --context SITE_CTX -n FLUX_NS get kustomization KS -o jsonpath='{.status.conditions[?(@.type=="Ready")].reason}{"\n"}'
   ```

   A new generation means Flux changed the object. Compare the run's fields again, as in step 1.

Permissions: `patch` on the held kind, and `get` and `patch` on the Kustomization and its GitRepository ([Permissions](../permissions.md)).
