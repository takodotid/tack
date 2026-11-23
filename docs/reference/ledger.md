# The ledger

Each run is one `SiteFailover` object, its ledger, at the new primary's site. It holds the whole plan and the result of every step. A run resumes from its ledger.

| Field | Value |
|---|---|
| API | `failover.tako.id/v1` |
| Kind | `SiteFailover`, namespaced, short name `sfo` |
| Where | the new primary's site, in `spec.ledger.namespace` of the Workload file (default `tack-system`) |
| Name | `<workload>-<UTC yyyymmdd-hhmmss>`, from the plan's time, with `-2` to `-9` appended when the name is taken |
| Label | `failover.tako.id/workload: <workload>` |
| `spec` | the mode, `from` and `to`, a snapshot of the Workload file, every step, and the cluster of each site that answered the plan. Written at creation, never changed. |
| `status` | the run's state and reason, each phase and step with its times and earlier attempts, captured values such as the demotion token, held objects, Git revisions, follow-ups, the fork point after a site loss, when writes stopped and came back, and the run's history |
| The CRD | `failover.tako.id_sitefailovers.yaml`, in each release and in the chart: [Install](../guide/install.md#the-crd-at-every-site) |

[RFC#0003](../../rfcs/RFC0003.md#design) lists every field of `spec` and `status`.

## Reading it

[Reading a run](../operations.md#reading-a-run) shows `tack status` and `kubectl get sfo`. The kubectl columns come from:

| Column | From |
|---|---|
| Workload | `spec.workload.name` |
| Mode | `spec.mode` |
| From, To | `spec.from`, `spec.to` |
| State | `status.state`: `Running`, `Waiting`, `Stopped` or `Succeeded` |
| Reason | `status.reason`: why a run stopped or waits ([Stopped and waiting runs](../runbooks/stop-reasons.md)) |
| Abandoned | `status.abandoned.at`, when `tack abandon` marked the run ([Abandon a run](../runbooks/abandon-run.md)) |
| Phase | `status.phase` |
| Age | `metadata.creationTimestamp` |
| TakenOverBy | `status.supersededBy`: the run that took this one over |

## Keeping it

Writers: [Who may write ledgers](../permissions.md#who-may-write-ledgers). Deletion: [Old ledgers](../old-ledgers.md). A CRD older than the CLI drops fields it does not know: [Upgrade](../guide/upgrade.md).
