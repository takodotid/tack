# Rebuild an old primary after a site loss

After a `SiteLoss` run, the old primary's Cluster may hold writes the new primary lacks. CloudNativePG never rewinds it, so it cannot follow the new primary. The run records the follow-up `Rebuild` ([RFC#0004](../../rfcs/RFC0004.md)).

The rebuild deletes the old Cluster's data. Do it only when the new primary is healthy and nothing needs the old data.

## Before

Without a snapshot, stop. `tack status RUN -f workload.yaml` must show `snapshot: Backup NS/NAME` on the old site's `Rebuild`. The old primary's writes after the fork point exist nowhere else: follow [recover-old-primary-writes.md](recover-old-primary-writes.md) first. Keep the snapshot's VolumeSnapshots until the data's owners are done.

| Check | Command |
|---|---|
| The run succeeded and lists the follow-up | `tack status RUN -f workload.yaml` |
| The new primary is healthy | `kubectl --context NEW -n NS get cluster NEW_CLUSTER` shows `Cluster in healthy state` |
| The old site declares the new primary | `kubectl --context OLD -n NS get cluster OLD_CLUSTER -o jsonpath='{.spec.replica.primary}'` |

## Steps

1. Set the old Cluster to clone from the new primary, in Git. In the old Cluster's manifest, set `bootstrap` as below, and commit. A Cluster reads `bootstrap` only when it is created, so this changes nothing live.

   ```yaml
   spec:
     bootstrap:
       pg_basebackup:
         source: NEW_CLUSTER
   ```

   If the Cluster archives WAL to an object store, give the rebuilt Cluster a new destination, such as a new `serverName`: CloudNativePG refuses to archive into the old one. Keep the old archive the `Rebuild` names; never delete it or point a Cluster's archiving at it again.

2. Delete the old Cluster. Its PVCs go with it.

   ```sh
   kubectl --context OLD -n NS delete cluster OLD_CLUSTER
   ```

3. Let Flux recreate it from Git and wait until it is a healthy replica:

   ```sh
   flux --context OLD reconcile kustomization KUSTOMIZATION --with-source
   kubectl --context OLD -n NS wait cluster OLD_CLUSTER --for=jsonpath='{.status.phase}'='Cluster in healthy state' --timeout=20m
   ```

4. Check with Tack: `tack plan -f workload.yaml --to OLD_SITE` must show the old site as a healthy `Replica` that declares the new primary. A planned switchover back is then possible.
