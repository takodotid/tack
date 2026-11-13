# Rebuild an old primary after a site loss

After a `SiteLoss` run, the old primary's Cluster may hold writes that the new primary lacks. CloudNativePG never rewinds it, so it cannot follow the new primary. The run records the follow-up `Rebuild` ([RFC#0004](../../rfcs/RFC0004.md)).

The rebuild deletes the old Cluster's data. Do it only once the new primary is healthy and nothing needs the old data.

## Before

**First check: Tack recorded a snapshot of the old primary.** `tack status RUN -f workload.yaml` shows `snapshot: Backup NS/NAME` on the old site's `Rebuild`. **Without it, stop:** the old primary's writes after the fork point exist nowhere else. Follow [recover-old-primary-writes.md](recover-old-primary-writes.md) first. It says how to take the snapshot by hand, or how to agree with the data's owners that the writes can go.

| Check | Command |
|---|---|
| The run succeeded and lists the follow-up | `tack status RUN -f workload.yaml` |
| The new primary is healthy | `kubectl --context NEW -n NS get cluster NEW_CLUSTER` shows `Cluster in healthy state` |
| The old site declares the new primary | `kubectl --context OLD -n NS get cluster OLD_CLUSTER -o jsonpath='{.spec.replica.primary}'` |

The snapshot keeps the old data after the rebuild. Keep its VolumeSnapshots until the data's owners are done.

## Steps

1. **Set the old Cluster to clone from the new primary, in Git.** In the old Cluster's manifest, set `bootstrap` as below, and commit:

   ```yaml
   spec:
     bootstrap:
       pg_basebackup:
         source: NEW_CLUSTER
   ```

   A Cluster reads `bootstrap` only when it is created, so this changes nothing live.

   If the Cluster archives WAL to an object store, give the rebuilt Cluster a new destination, such as a new `serverName`. Otherwise CloudNativePG refuses to archive into the old one. Keep the old archive, which the `Rebuild` names. Never delete it, and never point a Cluster's archiving at it again.

2. **Delete the old Cluster.** Its PVCs go with it.

   ```sh
   kubectl --context OLD -n NS delete cluster OLD_CLUSTER
   ```

3. **Let Flux recreate it** from Git, and wait until it is a healthy replica:

   ```sh
   flux --context OLD reconcile kustomization KUSTOMIZATION --with-source
   kubectl --context OLD -n NS wait cluster OLD_CLUSTER --for=jsonpath='{.status.phase}'='Cluster in healthy state' --timeout=20m
   ```

4. **Check with Tack.** `tack plan -f workload.yaml --to OLD_SITE` must show the old site as a healthy `Replica` that declares the new primary. A planned switchover back is then possible.
