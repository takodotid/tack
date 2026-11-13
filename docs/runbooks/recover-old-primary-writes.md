# Find and hand over the old primary's writes after a site loss

A site-loss run promotes the new primary without a hand-over. So the old primary may have taken writes that never reached the new one: the writes after the fork point. The rebuild deletes them. This runbook finds them in a snapshot of the old primary, and hands them to the owners of the data. It never merges them. The same row may have changed in both histories, and only the data's owners can decide which change wins.

Do this before [the rebuild](rebuild-old-primary.md). Tack does none of it, but it records what this runbook needs:

| Record | Where | Written by |
|---|---|---|
| The fork point: the new primary's WAL position right before its promotion, its timeline then, and its new timeline | `tack status RUN -f workload.yaml`, line `Fork:`; `status.fork` | `record-position-<to>` and `record-fork-<to>` |
| The snapshot of the old primary: a CloudNativePG `Backup` and its VolumeSnapshots | the follow-up `Rebuild` of the old site; `status.followUps[].snapshot` | `snapshot-<from>` and `record-snapshot-<from>`, when the Workload sets `spec.sites[].cnpg.snapshot` for that site |
| Where the old primary archived its WAL | the follow-up `Rebuild`; `status.followUps[].archive` | `record-snapshot-<from>` or `record-archive-<from>` |

Tack records the snapshot and the archive once the old site answers again and `tack resume RUN -f workload.yaml` runs the deferred steps. The fence runs first, so the old primary no longer takes writes when the snapshot is taken.

## Before

| Check | How | If not |
|---|---|---|
| Tack recorded the snapshot | `tack status RUN -f workload.yaml` shows `snapshot: Backup NS/NAME` on the `Rebuild` of the old site | **Stop. Do not rebuild.** If the old site answers, take a cold snapshot by hand, in the old Cluster's namespace: a `Backup` with `method: volumeSnapshot`, `target: primary` and `online: false`. If the old site's volumes are gone, there is nothing to recover. Record that with the data's owners, then rebuild. |
| The snapshot survives the rebuild | `kubectl --context OLD -n NS get volumesnapshot SNAPSHOT -o jsonpath='{.metadata.ownerReferences}'` names no `Cluster` | The Cluster's `spec.backup.volumeSnapshot.snapshotOwnerReference` is `cluster`, so deleting the Cluster deletes the snapshot. Remove that owner reference first: `kubectl --context OLD -n NS patch volumesnapshot SNAPSHOT --type=json -p '[{"op":"remove","path":"/metadata/ownerReferences"}]'`. |
| The snapshot's data outlives the object | `kubectl --context OLD get volumesnapshotcontent CONTENT -o jsonpath='{.spec.deletionPolicy}'`. The VolumeSnapshot names its content in `status.boundVolumeSnapshotContentName`. | With `Delete`, deleting the VolumeSnapshot deletes the data. Keep the VolumeSnapshot until the owners are done. |
| The fork point is recorded | `Fork:` names a position and a timeline | If `tack status` says it was not read, read the new primary's timeline history. On the new primary, run `psql -c "select pg_read_file('pg_wal/0000000N.history')"`, where `N` is its timeline in hexadecimal. The file's last line holds the old timeline and the position where the new one starts. |

Access needed: `kubectl` with `create` on `clusters` and `create` on `pods/exec` in the old Cluster's namespace at the old site, and `psql` access to the cluster started below.

## Steps

1. **Start the snapshot as a separate, isolated cluster.**
   - Create it at the old site, in the old Cluster's namespace `NS`, with a name of its own. A VolumeSnapshot restores only in its own namespace.
   - Apply it with `kubectl`, not through Git, so that no Flux Kustomization owns it.
   - Give it no `replica`, no `externalClusters`, no `backup`, no `plugins`, no Pooler and no Strait `SiteService`. It must not route clients, replicate, or archive into the old primary's archive.

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

   The Backup's `status.snapshotBackupStatus.elements` names each snapshot with its type: `PG_DATA`, `PG_WAL`. The snapshot is cold, so the cluster starts from what the volumes hold, with no WAL archive (CloudNativePG v1.30.1, `docs/src/recovery.md:120-185`, `docs/src/appendixes/backup_volumesnapshot.md:130-150`). It replays the old primary's WAL to its end, then starts a timeline of its own.

   Wait until it is healthy:

   ```sh
   kubectl --context OLD -n NS wait cluster OLD_CLUSTER-writes --for=jsonpath='{.status.phase}'='Cluster in healthy state' --timeout=30m
   ```

2. **List what changed after the fork point.** `FORK` is the fork position, and `TL` the timeline it is on. Both come from `Fork:`.

   - PostgreSQL 15 or later: in each database of the isolated cluster, run `create extension pg_walinspect;`, then:
     - PostgreSQL 16 or later: `select distinct reldatabase, relfilenode from pg_get_wal_block_info('FORK', pg_current_wal_lsn()) where relfilenode <> 0;`
     - PostgreSQL 15: `select distinct block_ref from pg_get_wal_records_info('FORK', pg_current_wal_lsn()) where block_ref is not null;`
   - Any version: run `pg_waldump --path=/var/lib/postgresql/data/pgdata/pg_wal --timeline=TL --start=FORK` in the isolated cluster's pod, through `kubectl exec`. Each record that touches a table names it as `rel TABLESPACE/DATABASE/FILENODE`. Once the cluster starts its own timeline, the last segment of timeline `TL` ends in `.partial`. Copy that segment without the suffix into a directory of its own, and read that directory too.

   The isolated cluster's own first writes, made after it started, come at the end of the list. They touch only system catalogs.

   From the same output, keep the transaction IDs after `FORK`, and the time of the first `COMMIT` after it. That time is when the lost writes start.

3. **Name the tables.** In each database the output names, run `select pg_filenode_relation(0, FILENODE)::regclass;` for each file node. To name a database, run `select datname from pg_database where oid = DATABASE;`.

4. **Query the rows.** In each table:
   - Rows inserted or updated after the fork carry one of those transaction IDs: `select * from TABLE where xmin::text::bigint in (XIDS);`.
   - A table with a modification time can also be queried by that time: `where updated_at >= 'FIRST_COMMIT_TIME'`.
   - A row deleted after the fork is gone from the isolated cluster. Find it by comparing primary keys with the new primary.

   Export the rows found, for example with `\copy (select ...) to 'TABLE.csv' csv header`.

5. **Hand the rows over.** Give each table's rows to the owners of that data, with the fork point, the first commit time and the run's name. The owners decide what to apply to the new primary, and how.

6. **Clean up once the owners are done.** Delete the isolated cluster `OLD_CLUSTER-writes`. Keep the VolumeSnapshots and the old WAL archive for as long as the retention rules require. The rebuilt Cluster must never write to that archive ([rebuild-old-primary.md](rebuild-old-primary.md)).
