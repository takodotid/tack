# Metrics

Tack serves no metrics. While planning, it reads two CloudNativePG metrics.

## What Tack reads

Each replica's instance manager serves its metrics on port 9187, path `/metrics`. Tack reads them through the API server's pod proxy, with `pods/proxy` ([Permissions](../permissions.md)) and no database credential.

| Metric | From | Tack uses it for |
|---|---|---|
| `cnpg_pg_replication_lag` | CloudNativePG's default monitoring queries | the replay lag in the plan's `WAL positions`, and a warning in a site-loss plan when the target still has WAL to replay |
| `cnpg_tack_replay_timestamp_seconds` | Tack's opt-in replay-time query, which an operator adds to each Cluster ([Replay time](../workload.md#replay-time)) | when the target's last replayed commit was, in the plan and in the ledger's `status.fork.lastCommit` |

A metric that is missing or unreadable is left out of the plan and never refuses a run. [RFC#0004](../../rfcs/RFC0004.md#requirements), Requirement 13, has the rules.

Recorded downtime and how to measure it: [How long writes stay down](../operations.md#how-long-writes-stay-down).
