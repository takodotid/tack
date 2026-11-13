# Concepts

A run moves the primary in four phases, and records each step in a ledger before it starts and after it ends.

## The four phases

A planned run from site a to site b:

| Phase | Tack does |
|---|---|
| Hold | Adds the annotation `kustomize.toolkit.fluxcd.io/reconcile: disabled` to the live objects the run changes, at each site, and to no others. Then waits for a fresh Flux reconcile at each site. From then on, Flux stops applying those objects. |
| Failover | Changes those objects through CloudNativePG's API, in order, and waits for each result. A planned run does this: stop the old Pooler, demote the old primary, capture its demotion token into the ledger, promote the new primary with it, start the new Pooler. |
| Promote to GitOps | Writes one commit that makes Git match live. The commit goes on top of the revision that live matched at Hold. Then waits until Flux at each site has applied it. |
| Release | At each site, renders the commit the way Flux does, and compares it with live. Removes the annotations only if the two match. Then checks that Flux changed nothing. If [Strait](https://github.com/takodotid/strait) is configured, waits until it routes to the new primary. |

## Two modes

| Mode | When | What happens |
|---|---|---|
| `--mode planned` (default) | Both sites answer. | The old primary demotes and hands its token to the new one. No write is lost. |
| `--mode site-loss` | The old site is lost. Confirm the loss first: [Fail over after a site loss](../runbooks/site-loss.md). | The new site promotes without a token. The old primary must be rebuilt before it can follow the new one again: [Rebuild an old primary](../runbooks/rebuild-old-primary.md). What each mode can lose, and for how long writes stop: [Data loss and downtime](../wal-rpo-rto.md). |

## The ledger

The ledger is a `SiteFailover` object at the new primary's site. Its namespace is `tack-system` by default. Its `spec` holds the whole plan and never changes. Its `status` holds every step's result, the demotion token, the held objects and the Git commit. Read it with `tack status`, or with `kubectl --context site-b -n tack-system get sitefailovers` (short name `sfo`). [The ledger](../reference/ledger.md) has the rest.

## A run's states

A run is in one of four states, recorded in the ledger:

| State | Reached when | Exit code | The way on |
|---|---|---|---|
| `Running` | `tack run` starts it, or `tack resume` continues it | | |
| `Succeeded` | each step ran or is deferred | 0 | none: it is done |
| `Stopped` | something unexpected happened | 3 | `tack resume`, with the decision's flag ([Stopped and waiting runs](../runbooks/stop-reasons.md)) |
| `Waiting` | Git does not answer | 4 | `tack resume`, once Git is back |

## Words

| Word | Meaning |
|---|---|
| Site | One Kubernetes cluster with one member of the workload, reached through its own kube context. |
| Primary | The site whose database takes writes. The others are replicas. |
| Hold | The annotation that stops Flux from applying one object. Tack holds only what the run changes. |
| Demotion token | What CloudNativePG publishes when the old primary demotes. The new primary promotes with it, so the two histories stay one. |
| Fence | To make a member declare another member primary, so that it stops being primary. It does not stop clients at that site from writing ([What fencing does](../operations.md#what-fencing-does)). |
| Break-glass | A site-loss run while Git is down: Tack switches live, then waits to commit ([Break-glass](../operations.md#break-glass)). |
| Follow-up | Work a run leaves for the operator, recorded in its ledger, such as `Rebuild` for an old primary after a site loss. |
| Workload file | The operator's file that names the sites, their contexts, the Git repository and where each field lives in it ([the Workload file](../workload.md)). |
