# Metrics

Tack is a command, so it serves no metrics of its own. It reads two metrics of CloudNativePG while it plans, and records how long each run kept writes down.

## What Tack reads

Each replica's instance manager serves its metrics on port 9187, path `/metrics`. Tack reads them through the API server's pod proxy, with the same permission as the WAL position (row 18 of [Permissions](../operations.md#permissions)), and no database credential.

| Metric | From | Tack uses it for |
|---|---|---|
| `cnpg_pg_replication_lag` | CloudNativePG's default monitoring queries | the replay lag in the plan's `WAL positions`, and a warning in a site-loss plan when the target still has WAL to replay |
| `cnpg_tack_replay_timestamp_seconds` | Tack's opt-in replay-time query, which an operator adds to each Cluster ([Replay time](../workload.md#replay-time)) | when the target's last replayed commit was, in the plan and in the ledger's `status.fork.lastCommit` |

A metric that is missing, or that cannot be read, leaves the plan as it is without it, and never refuses a run. [RFC#0004](../../rfcs/RFC0004.md#requirements), Requirement 13, has the rules.

## What Tack records

| Record | Where |
|---|---|
| When writes stopped and came back | `status.writes`, and the line `Writes down:` of `tack status RUN` ([How long writes stay down](../operations.md#how-long-writes-stay-down)) |
| When each phase and step started and ended, and each step's earlier attempts | `status.phases`, `status.steps`, and `tack status RUN` |
| Where a site loss's new history leaves the old one | `status.fork` |

To measure what clients see, in staging, use `hack/measure/writes.sh` with [Measure failover time](../runbooks/measure-failover-time.md). [Data loss and downtime](../wal-rpo-rto.md) sets the budget.
