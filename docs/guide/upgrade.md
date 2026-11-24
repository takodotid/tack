# Upgrade

Upgrade the CRD before the CLI, and every operator's CLI together.

## Upgrade within v1

1. Finish or stop every run. `tack status -f workload.yaml` must list each run as `Succeeded`, or as `Stopped` or `Waiting` with no `tack` process running it.
2. Upgrade the CRD at every site, the way it was installed: the `helm upgrade --install` of [Install](install.md#the-crd-at-every-site), or `kubectl apply --server-side` of the new CRD. Do it before any operator runs the new CLI against the site. A CRD older than the CLI silently drops the fields it does not know: `tack run` refuses to create a ledger there, and status writes lose those fields.
3. Upgrade every operator's CLI together, and every script that runs `tack`. A CLI drops the status fields it does not know when it writes a ledger. From v1.3.0, each write records the CLI's version in `status.writtenBy`. A CLI of an older release then refuses to resume or abandon that run, and names the version to use ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 24). A build whose version names no release refuses too ([`--version`](../reference/cli.md#flags)). A CLI older than v1.3.0 does not check.

Then read the section of each release skipped over, newest first.

### v1.6.x to v1.7.0

- **CRD:** new optional fields `spec.acceptBehind`, `status.fork` (with `status.fork.lastCommit`), `status.writes`, `status.steps[].earlier`, `wait.stall` of a step, `status.followUps[].snapshot` and `.archive`. In the Workload snapshot: `cnpg.snapshot`, `timeouts.snapshot` and `timeouts.archiving`. New step types: `Create` and `Record`.
- A v1.6.x CRD drops `spec.acceptBehind` (`tack run --accept-behind` refuses to create a ledger there) and `status.fork.lastCommit` (the ledger loses the replay time).
- Grant `get` on `pods` and `pods/proxy` in each member's namespace ([Permissions](../permissions.md); the chart's objects Role has it where `rbac.objects` names `clusters`). The plan reads each site's WAL position and each replica's replay lag with it. Without it, the plan only warns, and a site-loss run records no fork point.
- To have a site-loss plan report the new primary's last replayed commit time, add the opt-in replay-time query to each Cluster ([Replay time](../workload.md#replay-time)).
- A site-loss plan refuses a target that is behind another replica that answers, unless `--accept-behind` is passed ([RFC#0004](../../rfcs/RFC0004.md#requirements), Requirement 14).
- A site-loss run records the fork point and the old primary's WAL archive. With `cnpg.snapshot`, it also snapshots the old primary, and needs `get` and `create` on `backups` ([Permissions](../permissions.md); the chart's objects Role has it where `rbac.objects` names `clusters`).
- A planned switchover is refused when two sites archive WAL to the same destination. A site-loss plan warns instead ([WAL archives](../workload.md#wal-archives)). The check needs `get` on the barman-cloud `objectstores` the Clusters name. With the chart, list them in the new `rbac.objects[].objectStores`. Without the grant, the plan warns that it could not check.
- After each promotion, the run waits up to `timeouts.archiving` for the new primary to archive WAL. If that time runs out, it warns and records the follow-up `CheckArchiving`.
- In a planned run, the wait for the promotion stops once replay has stood still for 2 minutes short of the token ([How long writes stay down](../operations.md#how-long-writes-stay-down)).
- A ledger written by v1.6.x resumes with the steps it planned.

### v1.5.x to v1.6.0

- **CRD:** no new field. Only the TakenOverBy column of `kubectl get sfo` is missing from a v1.5.x CRD.
- Tack obtains each site's credentials before its checks and before each step. A site whose credentials cannot be obtained is never counted as lost ([Reading a run](../operations.md#reading-a-run), [RFC#0007](../../rfcs/RFC0007.md#requirements), Requirement 12).
- A planned switchover is refused when a site's Cluster does not name the new primary in `spec.externalClusters`. A site-loss plan warns, and the run stops at that site's step ([RFC#0004](../../rfcs/RFC0004.md#requirements), Requirement 12).
- `tack prune` deletes a run whose follow-up `Rebuild` is done, and needs `get` on the workload's objects ([Permissions](../permissions.md)). Without it, prune keeps that run. It keeps more than `--keep` runs when the clocks of two sites cannot order them.
- In a ledger written by v1.5.x, `tack prune` deletes a run with a `Rebuild` follow-up once that site's member was created again, 10 s or more after the run's ledger.

### v1.4.x to v1.5.0

- **CRD:** unchanged.
- Grant `get`, `create` and `update` on `leases` in the ledger namespace ([Permissions](../permissions.md); the chart's ledger Role has it with `rbac.create`). Without it, a run holds no Lease and says so once.
- `tack abandon` and `tack run --take-over` refuse a run that a process may still run. `--force` goes on.
- Quote a `kubeContext` that YAML reads as other text, such as `on`: v1.5.0 refuses it unquoted.
- A v1.4.x ledger has no Lease, so `tack abandon` and `tack run --take-over` judge it by the ledger: they refuse a `Running` run whose ledger was written within its `Running` step's timeout plus 5 minutes, unless `--force` is passed.

### v1.3.x to v1.4.0

- **CRD:** no new field. `kubectl get sfo` gains the Abandoned column after Reason.
- `tack prune` needs `delete` on `sitefailovers` in the ledger namespace ([Permissions](../permissions.md)). The chart grants it only with `rbac.prune`; grant it only to operators who prune.
- `tack resume` and `tack abandon` of v1.3.x refuse a run once v1.4.0 has written its ledger, and name v1.4.0.

### v1.3.0 to v1.3.1

- `tack status -f` exits with code 1 when a site gives no answer, and lists only the runs it read ([Reading a run](../operations.md#reading-a-run)).

### v1.2.x to v1.3.0

- **CRD:** new optional fields `spec.takeOvers`, `status.abandoned`, `status.git.changedRevision`, `status.history`, `status.historyDropped` and `status.writtenBy`. The column Reason of `kubectl get sfo` is new.
- Grant `create` on `events` in the ledger namespace. Without it, a run goes on without Events ([Permissions](../permissions.md)).
- In each Workload file, quote every site name that YAML reads as a boolean or a number, such as `n`, `on` or `010`. v1.3.0 refuses it unquoted ([Workload file](../workload.md#specsites)). A run planned before the upgrade recorded the decoded name, such as `false`; its flags, such as `--fence`, take that name.

### v1.0.x or v1.1.x to v1.2.x

- **CRD:** new optional fields `spec.takeOver`, `spec.fingerprints`, `status.fingerprints`, `status.supersededBy` and `status.git.releaseRevision`.
- Grant `get` on the namespace `kube-system` at every site. The plan reads its UID to identify the site's cluster and fails with `forbidden` without it.
- Tack refuses an `http` or `https` Git URL that holds a user, such as `https://user@git.example.com/gitops.git`. Move the user into git's configuration (`credential.<url>.username`). `ssh://git@git.example.com/gitops.git` and `git@git.example.com:gitops.git` still work.
- A ledger written by v1.0.x or v1.1.x records each site's cluster in `status.fingerprints` on its first read, then checks it.

## Upgrade from v0.1.x

1. At every site, upgrade the CRD the way it was installed: the `helm upgrade --install` of [Install](install.md#the-crd-at-every-site), or `kubectl apply --server-side` of the new CRD. Do it before running tack v1.0.0 there: against a v0.1.x CRD, v1.0.0 stops and says to upgrade it.
2. Install tack v1.0.0 or later.
3. In each Workload file, set `apiVersion: failover.tako.id/v1`. Tack still reads `failover.tako.id/v1alpha1`, with a warning.

| After the upgrade | Behaviour |
|---|---|
| Ledgers written by v0.1.x | `tack status` reads them and `tack resume` continues them. The API server serves them as `v1` and stores each as `v1` on its next write. |
| tack v0.1.x | Still reads and writes ledgers, through `v1alpha1`, until every operator has upgraded. |
| `kubectl` | Warns on each `v1alpha1` request. Until its discovery cache refreshes, `kubectl get sfo` may still ask for `v1alpha1`. |
| Downgrading the CRD to v0.1.x | Refused by the API server: `v1` is now in the CRD's `status.storedVersions`. |
