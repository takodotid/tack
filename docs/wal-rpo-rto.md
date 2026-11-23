# WAL archiving, data loss (RPO) and downtime (RTO)

- RPO: the recent writes a run can lose, those the new primary never got.
- RTO: how long writes stay down, from the last moment clients could write to the first moment they can again.

## What each mode costs

| Mode | RPO | RTO starts | RTO ends |
|---|---|---|---|
| `Planned` | **0.** The new primary promotes only after it has replayed up to the old primary's demotion token ([RFC0004](../rfcs/RFC0004.md#requirements), Requirement 6). | when the old Pooler goes to 0 (`pooler-<from>`); without a Pooler, when the demotion starts | when the new Pooler is ready (`wait-pooler-<to>`); without a Pooler, when the new primary is healthy. Then Strait changes its route |
| `SiteLoss` | **The WAL the new primary never received, plus writes the old primary took before it was fenced.** Replication between sites is asynchronous. | when the site was lost. Tack cannot see it, so it records the run's start, marked as an estimate | as for `Planned` |

In a site loss, RTO starts with people: noticing the loss, confirming it, cutting the old site off, deciding to fail over. Tack starts when the operator runs it.

## Before choosing `--to` in a site loss

1. Run `tack plan -f workload.yaml --to SITE --mode site-loss`. Its `WAL positions` table shows how far each answering site's replica received and replayed WAL, and marks one `most caught up`. Promote that one: the plan refuses another target unless `--accept-behind` is passed ([site-loss.md](runbooks/site-loss.md)).
2. Read the notes. The plan states what the run will lose: the writes after the target's position. It warns when the target delays replay (`minApplyDelay`), when its replay is paused or lags, and when a position could not be read.
   - The replay lag is WAL the target received and has not replayed. The promotion replays it first, so lag adds to the time writes stay down.
   - A lag of 0 does not give the age of the target's data. For that, add the opt-in replay-time query: [workload.md](workload.md#replay-time).
3. Check the archive. A replica that streams nothing may still restore WAL from the old primary's archive. If the archive holds more than any replica, wait until that WAL is replayed before promoting; the plan's position moves as it is. Archive rules: [workload.md](workload.md#wal-archives).

## The old primary's writes after a site loss

Writes the old primary took after the fork point, where the histories split, are not in the new primary. Tack records the fork point and, with `cnpg.snapshot`, a snapshot of the old primary. [recover-old-primary-writes.md](runbooks/recover-old-primary-writes.md) finds those writes and hands them to their owners.

## The failover-time budget

Each row is one part of the time writes stay down. Fill the measured column in staging with [measure-failover-time.md](runbooks/measure-failover-time.md).

| Part | Mode | Steps | Bounded by | Measured in staging |
|---|---|---|---|---|
| The loss is noticed and confirmed, and the old site is cut off | `SiteLoss` | outside Tack | people and monitoring ([site-loss.md](runbooks/site-loss.md)) | |
| Plan and confirmation | `SiteLoss` | `tack plan`, `tack run` | each lost site's checks; reading the sites | |
| Hold | `SiteLoss` | the Hold steps at the sites that answer | `timeouts.flux` per site | |
| The old Pooler stops | `Planned` | `pooler-<from>`, `wait-pooler-<from>` | `timeouts.promotion` | |
| The old primary demotes | `Planned` | `demote-<from>`, `wait-demoted-<from>` | `timeouts.demotion`; `switchoverDelay` ([operations.md](operations.md#how-long-writes-stay-down)) | |
| The token moves | `Planned` | `capture-token`, the read before the promotion | a few API calls | |
| The new primary promotes | both | `promote-<to>`, `wait-promoted-<to>`; in a site loss also `record-position-<to>` | `timeouts.promotion`; a promotion that replays nothing for 2 minutes stops ([operations.md](operations.md#how-long-writes-stay-down)) | |
| The new Pooler starts | both | `pooler-<to>`, `wait-pooler-<to>` | `timeouts.promotion` | |
| Tack's window | both | `tack status`: `Writes down:`. In a site loss it counts from the run's start | the sum of the rows above it; in a site loss, from Hold on | |
| Strait routes to the new primary | both | outside Tack | Strait's share ([takodotid/strait#73](https://github.com/takodotid/strait/issues/73)) | |
| Clients reconnect | both | outside Tack | the application's own retry | |
