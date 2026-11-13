# Fail over after a site loss

`--mode site-loss` promotes a new primary without a hand-over from the old one. Tack counts a site as lost only when the site's API gives no answer to the operator's host ([RFC#0007](../../rfcs/RFC0007.md), Requirement 5). Tack cannot tell a lost site from one that still runs but is cut off from that host.

If the old site still runs, its Cluster stays primary and takes writes until one of these happens:

| Event | When |
|---|---|
| Flux at the old site applies the run's commit | only once that site reaches Git again, and after the run's commit exists |
| Tack fences it | on a `tack resume` that reaches the site, after `--fence` if the site answers as primary or declares itself primary |

Until then there are two primaries. Before the run, confirm the loss and fence the old primary. Tack does neither.

**A cut-off site's own applications keep writing.** During a partition, the applications at the cut-off site still reach their local primary. Strait at that site still routes them to it, since it is the only writable member that site can see. Their writes are lost to the new primary. So fence those applications too, or their Pooler, not only the database. What Tack's fence does, and does not do: [operations.md](../operations.md#what-fencing-does).

## Before

| Check | How |
|---|---|
| The old site is down, not only cut off from this host | Check it from a second vantage point that does not share this host's network path: another site, an out-of-band console, the provider's status page, monitoring. |
| The old primary cannot take writes | Fence it out of band: stop its database nodes, cut its network at the edge, or power it off. Its Kubernetes API is not there to do it. |
| The old site's applications cannot write | Stop the applications, or their Pooler, at that site, out of band: scale them to 0 from a console that still reaches the site, cut the site's network at the edge, or power it off. Cutting off the whole site covers the database and its applications at once. |
| Clients no longer reach the old primary | With Strait: at each site that answers, the route names no member of the old site. Check with `kubectl --context SITE -n NS get siteservice NAME -o jsonpath='{.status.route.members}'`. |
| The new site is a healthy replica | `tack plan -f workload.yaml --to NEW --mode site-loss` shows it as a healthy `Replica`. |

## Steps

1. **Plan:** `tack plan -f workload.yaml --to NEW --mode site-loss`. The plan's notes name the sites it could not read.
   - **Pick the most caught-up site.** The plan's `WAL positions` show how far each replica got, and mark one as `most caught up`. Promote that one. The writes that `NEW` never received are lost (the RPO). The plan refuses a `NEW` that is behind another replica that answers, and names the one to promote.
   - **To promote a site that is behind anyway,** add `--accept-behind` to the plan and to the run. One reason: the most caught-up site cannot serve. The run then records a `Rebuild` for each replica that was ahead. Such a replica holds writes that `NEW` lacks, so it cannot follow `NEW`.
   - A position that could not be read is a warning, not a refusal. The plan says why, such as a missing permission (row 18 of [the permissions table](../operations.md#permissions)).
2. **Run:** `tack run -f workload.yaml --to NEW --mode site-loss`, with `--accept-behind` if the plan needed it. The steps at the lost site are `Deferred`. If Git is down too, the run waits, with exit code 4: [break-glass](../operations.md#break-glass).
   - What the run will lose, and how long writes stay down: [wal-rpo-rto.md](../wal-rpo-rto.md).
3. **When the old site answers again, resume:** `tack resume RUN -f workload.yaml`.
   - If the site answers as primary, or its Cluster still declares itself primary, the run stops with `FencedSiteIsPrimary`. Check the site, then run `tack resume RUN -f workload.yaml --fence OLD`.
   - A run whose old site answers its API at the start, but with its instances down, stops the same way before its first step there. The plan says so.
4. **Rebuild the old primary:** [rebuild-old-primary.md](rebuild-old-primary.md).

## When Tack refuses the lost site

Tack does not count these sites as lost, and the plan refuses:

- a site whose API answers with an error, such as a load balancer that answers 503;
- a site whose kube context needs a credential plugin that fails, or never ends, because of the loss. Tack waits for a plugin with no time limit, so interrupt it with Ctrl-C.

For a 5xx or 429, the plan's message names this section. Tack cannot tell a 503 from a lost site from a 503 from a live API server that restarts ([RFC#0007](../../rfcs/RFC0007.md), Motivation).

Only once the loss is confirmed and the old primary is fenced, as above:

1. Copy the kubeconfig. In the copy, point the lost site's context at an address where nothing listens, such as a free port on `127.0.0.1`.
2. Plan and run with `--kubeconfig COPY`. The site then gives no answer, and counts as lost.
3. Use the copy for every `tack resume` while the site stays lost. With the usual kubeconfig, the search for runs fails on the site's error.
4. Once the site is back and answers its API, delete the copy, and resume with the usual kubeconfig. Tack records the site's cluster on its first read ([RFC#0003](../../rfcs/RFC0003.md), Requirement 19).
