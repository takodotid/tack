# The ledger

Each run is one `SiteFailover` object, its ledger, at the new primary's site. The ledger holds the whole plan, and the result of every step. A run resumes from its ledger, never from memory.

| | |
|---|---|
| API | `failover.tako.id/v1`. `v1alpha1`, the API of v0.1.x, is still served, deprecated, with the same schema. |
| Kind | `SiteFailover`, namespaced, short name `sfo` |
| Where | the new primary's site, in the Workload file's `spec.ledger.namespace`, `tack-system` by default |
| Name | `<workload>-<UTC yyyymmdd-hhmmss>`, from the plan's time, with `-2` to `-9` appended when the name is taken |
| Label | `failover.tako.id/workload: <workload>` |
| `spec` | the mode, `from` and `to`, a snapshot of the Workload file, every step, and the cluster of each site that answered the plan. Written at creation, and never changed after. |
| `status` | the run's state and reason, each phase and step with its times and earlier attempts, captured values such as the demotion token, held objects, Git revisions, follow-ups, the fork point after a site loss, when writes stopped and came back, and the run's history |
| The CRD | `failover.tako.id_sitefailovers.yaml`, in each release and in the chart: [Install](../guide/install.md#the-crd-at-every-site) |

[RFC#0003](../../rfcs/RFC0003.md#design) lists every field of `spec` and `status`.

## Reading it

`tack status RUN -f workload.yaml` shows a run step by step, and `-o yaml` or `-o json` prints the object ([Reading a run](../operations.md#reading-a-run)). With kubectl:

```sh
kubectl --context site-b -n tack-system get sfo
```

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

Writing a ledger is privileged, because `tack resume` acts on what a ledger holds: check [who may write ledgers](../operations.md#who-may-write-ledgers). `tack prune` deletes the ledgers of old runs that succeeded ([Old ledgers](../operations.md#old-ledgers)). A CRD older than the CLI drops the fields it does not know, so [upgrade](../guide/upgrade.md) the CRD first.
