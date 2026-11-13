# WAL archiving, data loss (RPO) and downtime (RTO)

This guide says what each mode of a run can lose, how long writes stay down, and what the operator reads before choosing `--to`.

- **RPO** is how much recent data a run can lose: the writes that the new primary never got.
- **RTO** is how long writes stay down: from the moment clients can no longer write to the moment they can write again.

## What each mode costs

| Mode | RPO | RTO starts | RTO ends |
|---|---|---|---|
| `Planned` | **0.** The old primary demotes and publishes a token. The new one promotes only after it has replayed up to that token ([RFC0004](../rfcs/RFC0004.md#requirements), Requirement 6). | when the old Pooler goes to 0 (`pooler-<from>`). Without a Pooler, when the demotion starts | when the new Pooler is ready (`wait-pooler-<to>`). Without a Pooler, when the new primary is healthy. Then Strait changes its route |
| `SiteLoss` | **The WAL that the new primary never received, plus any writes that the old primary took before it was fenced.** Replication between sites is asynchronous. The plan shows how far each replica got. | when the site was lost. Tack cannot see that moment, so it records the run's start, marked as an estimate | as for `Planned` |

In a site loss, RTO starts with people. Someone must notice the loss, confirm it, cut the old site off, and decide to fail over. Tack starts only when the operator runs it.

## Before you choose `--to` in a site loss

1. **Run `tack plan -f workload.yaml --to SITE --mode site-loss`.** Its `WAL positions` table shows how far each answering site's replica received and replayed WAL. It marks one site `most caught up`. Promote that one. The plan refuses a target that another replica is ahead of, unless `--accept-behind` is passed ([site-loss.md](runbooks/site-loss.md)).
2. **Read the notes.** The plan states what the run will lose: the writes after the target's position. It warns when the target delays replay (`minApplyDelay`), when its replay is paused, and when a position could not be read.
3. **Check the archive.** A replica that streams nothing may still restore WAL from the old primary's archive. If the archive holds more than any replica, wait until that WAL is replayed before promoting. Watch the position in the plan move.

## WAL archiving

Give each site's Cluster its own archive from day one: [workload.md](workload.md#wal-archives). After a site loss:

| Archive | What happens to it |
|---|---|
| The old primary's | kept. Tack records where it is on the `Rebuild` follow-up. The rebuilt Cluster must write to a new destination ([rebuild-old-primary.md](runbooks/rebuild-old-primary.md)). |
| The new primary's | checked. After the promotion, `verify-archiving-<to>` waits until the new primary archives WAL of its new timeline. If that does not happen in time, the run warns and records `CheckArchiving`. Until archiving works, there is no point-in-time recovery to any moment after the promotion. |

## The old primary's writes after a site loss

The writes that the old primary took after the fork point are not in the new primary. The fork point is where the two histories split. Tack never merges those writes. It records the fork point and, with `cnpg.snapshot`, a snapshot of the old primary. Then [recover-old-primary-writes.md](runbooks/recover-old-primary-writes.md) finds those writes and hands them to the people who own the data.

## The failover-time budget

Each row is one part of the time that writes stay down. The "Bounded by" column names the timeouts and settings that cap that part. Fill in the measured column in staging with [measure-failover-time.md](runbooks/measure-failover-time.md). Numbers from kind or CI do not count.

### Planned switchover

| Part | Steps | Bounded by | Measured in staging |
|---|---|---|---|
| The old Pooler stops | `pooler-<from>`, `wait-pooler-<from>` | `timeouts.promotion` | |
| The old primary demotes | `demote-<from>`, `wait-demoted-<from>` | `timeouts.demotion`; CloudNativePG's `switchoverDelay` ([operations.md](operations.md#how-long-writes-stay-down)) | |
| The token moves | `capture-token`, the read before the promotion | a few API calls | |
| The new primary promotes | `promote-<to>`, `wait-promoted-<to>` | `timeouts.promotion`; stops after 2 minutes if replay stands still | |
| The new Pooler starts | `pooler-<to>`, `wait-pooler-<to>` | `timeouts.promotion` | |
| Tack's window | `tack status`: `Writes down:` | the sum of the above | |
| Strait routes to the new primary | outside Tack | Strait's share: [takodotid/strait#73](https://github.com/takodotid/strait/issues/73) | |
| Clients reconnect | outside Tack | the application's own retry | |

### Failover after a site loss

| Part | Steps | Bounded by | Measured in staging |
|---|---|---|---|
| The loss is noticed and confirmed, and the old site is cut off | outside Tack | people and monitoring ([site-loss.md](runbooks/site-loss.md)) | |
| Plan and confirmation | `tack plan`, `tack run` | each lost site's checks; reading the sites | |
| Hold | the Hold steps at the sites that answer | `timeouts.flux` per site | |
| The new primary promotes | `record-position-<to>`, `promote-<to>`, `wait-promoted-<to>` | `timeouts.promotion` | |
| The new Pooler starts | `pooler-<to>`, `wait-pooler-<to>` | `timeouts.promotion` | |
| Tack's window | `tack status`: `Writes down:`, from the run's start | the sum of the rows above it, from Hold on | |
| Strait routes to the new primary | outside Tack | Strait's share: [takodotid/strait#73](https://github.com/takodotid/strait/issues/73) | |
| Clients reconnect | outside Tack | the application's own retry | |
