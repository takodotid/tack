# Find and hand over the old primary's writes after a site loss

A site-loss run promotes the new primary without a hand-over, so the old primary may hold writes after the fork point that the new one lacks. The rebuild deletes them. This runbook finds them in a snapshot of the old primary and hands them to the data's owners. Tack never merges them: only the owners can decide which change wins.

Do this before [the rebuild](rebuild-old-primary.md). Tack records what it needs:

| Record | Where | Written by |
|---|---|---|
| The fork point: the new primary's WAL position before its promotion, its timeline then and now; with the [replay-time query](../workload.md#replay-time), also the commit time of the last transaction it replayed. | `tack status RUN -f workload.yaml`, line `Fork:`; `status.fork` | `record-position-<to>` and `record-fork-<to>` |
| The snapshot of the old primary: a CloudNativePG `Backup` and its VolumeSnapshots | the follow-up `Rebuild` of the old site; `status.followUps[].snapshot` | `snapshot-<from>` and `record-snapshot-<from>`, when the Workload sets `cnpg.snapshot` for that site |
| Where the old primary archived its WAL | the follow-up `Rebuild`; `status.followUps[].archive` | `record-snapshot-<from>` or `record-archive-<from>` |

Tack records the snapshot and archive when the old site answers and `tack resume RUN -f workload.yaml` runs the deferred steps. The fence runs first, so the old primary takes no writes during the snapshot.

## Before

Access needed: `kubectl` with `create` on `clusters` and `pods/exec` in the old Cluster's namespace at the old site, and `psql` access to the cluster started below.

| Check | How |
|---|---|
| Tack recorded the snapshot | `tack status RUN -f workload.yaml` shows `snapshot: Backup NS/NAME` on the old site's `Rebuild`. **Without it, do not rebuild.** |
| The snapshot survives the rebuild | The VolumeSnapshot's `metadata.ownerReferences` names no `Cluster`. Command 1 under this table. |
| The data outlives the snapshot object | The VolumeSnapshotContent's `spec.deletionPolicy`. The VolumeSnapshot names its content in `status.boundVolumeSnapshotContentName`. Command 2 under this table. |
| The fork point is recorded | `Fork:` names a position and a timeline |

The two commands:

```sh
# 1. The snapshot's owners
kubectl --context OLD -n NS get volumesnapshot SNAPSHOT -o jsonpath='{.metadata.ownerReferences}'
# 2. The deletion policy of its content
kubectl --context OLD get volumesnapshotcontent CONTENT -o jsonpath='{.spec.deletionPolicy}'
```

If a check fails:

- **No snapshot, old site answers:** take a cold snapshot by hand in the old Cluster's namespace: a `Backup` with `method: volumeSnapshot`, `target: primary`, `online: false`.
- **No snapshot, old site's volumes gone:** nothing to recover. Tell the data's owners, then rebuild.
- **An owner reference to a `Cluster`:** the Cluster's `spec.backup.volumeSnapshot.snapshotOwnerReference` is `cluster`, so deleting the Cluster deletes the snapshot. Remove the reference first: `kubectl --context OLD -n NS patch volumesnapshot SNAPSHOT --type=json -p '[{"op":"remove","path":"/metadata/ownerReferences"}]'`.
- **Deletion policy `Delete`:** deleting the VolumeSnapshot deletes the data. Keep the VolumeSnapshot until the owners are done.
- **The fork point was not read:** run `psql -c "select pg_read_file('pg_wal/0000000N.history')"` on the new primary, `N` being its timeline in hexadecimal. The last line holds the old timeline and the position where the new one starts.

## Steps

1. **Start the snapshot as a separate, isolated cluster.**
   - Create it at the old site, in the old Cluster's namespace `NS` (a VolumeSnapshot restores only there), under a new name.
   - Apply it with `kubectl`, not Git, so no Flux Kustomization owns it.
   - Give it no `replica`, `externalClusters`, `backup`, `plugins`, Pooler or Strait `SiteService`: it must not route clients, replicate, or archive into the old primary's archive.

   ```yaml
   apiVersion: postgresql.cnpg.io/v1
   kind: Cluster
   metadata:
     name: OLD_CLUSTER-writes
     namespace: NS
   spec:
     instances: 1
     imageName: IMAGE          # the old Cluster's PostgreSQL image
     storage: {size: SIZE}     # at least the old Cluster's
     walStorage: {size: SIZE}  # only if the old Cluster had walStorage
     postgresql:
       parameters:
         wal_keep_size: 64GB   # keep the old WAL readable after the recovery
     bootstrap:
       recovery:
         volumeSnapshots:
           storage: {name: PGDATA_SNAPSHOT, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}
           walStorage: {name: PGWAL_SNAPSHOT, kind: VolumeSnapshot, apiGroup: snapshot.storage.k8s.io}  # only with walStorage
   ```

   The Backup's `status.snapshotBackupStatus.elements` names each snapshot with its type, `PG_DATA` or `PG_WAL`. The cold snapshot starts from the volumes with no WAL archive (CloudNativePG v1.30.1), replays the old primary's WAL to its end, then starts its own timeline.

   Wait for it:

   ```sh
   kubectl --context OLD -n NS wait cluster OLD_CLUSTER-writes --for=jsonpath='{.status.phase}'='Cluster in healthy state' --timeout=30m
   ```

2. **List what changed after the fork point.** `FORK` is the fork position and `TL` its timeline, both from `Fork:`.

   In PostgreSQL 15 or later, run `create extension pg_walinspect;` in each database of the isolated cluster, then:

   | Version | Query |
   |---|---|
   | 16 or later | `select distinct reldatabase, relfilenode from pg_get_wal_block_info('FORK', pg_current_wal_lsn()) where relfilenode <> 0;` |
   | 15 | `select distinct block_ref from pg_get_wal_records_info('FORK', pg_current_wal_lsn()) where block_ref is not null;` |

   In any version, run `pg_waldump --path=/var/lib/postgresql/data/pgdata/pg_wal --timeline=TL --start=FORK` in the isolated cluster's pod with `kubectl exec`. Each record that touches a table names it as `rel TABLESPACE/DATABASE/FILENODE`. Once the cluster starts its own timeline, the last segment of `TL` ends in `.partial`: copy it without the suffix into a directory of its own and read that too.

   The isolated cluster's own first writes end the list and touch only system catalogs.

   From the same output, keep the transaction IDs after `FORK` and the time of the first `COMMIT` after it, when the lost writes start.

3. **Name the tables.** In each database the output names, run `select pg_filenode_relation(0, FILENODE)::regclass;` per file node. Name a database with `select datname from pg_database where oid = DATABASE;`.

4. **Query the rows.** In each table:
   - Rows inserted or updated after the fork carry one of those transaction IDs: `select * from TABLE where xmin::text::bigint in (XIDS);`.
   - A table with a modification time can be queried by it: `where updated_at >= 'FIRST_COMMIT_TIME'`.
   - Rows deleted after the fork are gone from the isolated cluster: compare primary keys with the new primary.

   Export the rows, for example with `\copy (select ...) to 'TABLE.csv' csv header`.

5. **Hand the rows over.** Give each table's rows to its data's owners, with the fork point, the first commit time and the run's name. The owners decide what to apply to the new primary, and how.

6. **Clean up once the owners are done.** Delete the isolated cluster `OLD_CLUSTER-writes`. Keep the VolumeSnapshots and the old WAL archive as long as the retention rules require, and never write to that archive again ([rebuild-old-primary.md](rebuild-old-primary.md)).
