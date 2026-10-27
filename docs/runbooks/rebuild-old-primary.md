# Rebuild an old primary after a site loss

After a `SiteLoss` run, the old primary's Cluster may hold writes the new primary lacks. CloudNativePG never rewinds it, so it cannot follow the new primary. Its run records the follow-up `Rebuild` ([RFC#0004](../../rfcs/RFC0004.md)).

The rebuild deletes the old Cluster's data. Do it only once the new primary is healthy and nothing needs the old data.

## Before

| Check | Command |
|---|---|
| The run succeeded and lists the follow-up | `tack status RUN -f workload.yaml` |
| The new primary is healthy | `kubectl --context NEW -n NS get cluster NEW_CLUSTER` shows `Cluster in healthy state` |
| The old site declares the new primary | `kubectl --context OLD -n NS get cluster OLD_CLUSTER -o jsonpath='{.spec.replica.primary}'` |

If the old data holds writes that must be kept, export them from the old Cluster first. Tack does not do this.

## Steps

1. **Git.** In the old Cluster's manifest, set `bootstrap` to clone from the new primary, and commit:

   ```yaml
   spec:
     bootstrap:
       pg_basebackup:
         source: NEW_CLUSTER
   ```

   `bootstrap` is read only when a Cluster is created, so this changes nothing live. If the Cluster archives WAL to an object store, give the rebuilt Cluster a new archive path, or CloudNativePG refuses to archive into the old one.

2. **Delete the old Cluster.** Its PVCs go with it.

   ```sh
   kubectl --context OLD -n NS delete cluster OLD_CLUSTER
   ```

3. **Let Flux recreate it** from Git, and wait until it is a healthy replica:

   ```sh
   flux --context OLD reconcile kustomization KUSTOMIZATION --with-source
   kubectl --context OLD -n NS wait cluster OLD_CLUSTER --for=jsonpath='{.status.phase}'='Cluster in healthy state' --timeout=20m
   ```

4. **Check with Tack.** `tack plan -f workload.yaml --to OLD_SITE` shows the old site as a healthy `Replica` that declares the new primary. A planned switchover back is then possible.
