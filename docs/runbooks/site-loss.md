# Fail over after a site loss

`--mode site-loss` promotes a new primary without a hand-over from the old one. Tack counts a site as lost only when its API gives no answer to the operator's host ([RFC#0007](../../rfcs/RFC0007.md), Requirement 5). It cannot tell a lost site from one that runs but is cut off from that host.

If the old site still runs, its Cluster stays primary and takes writes, so there are two primaries, until:

| Event | When |
|---|---|
| Flux at the old site applies the run's commit | only once that site reaches Git again, and after the run's commit exists |
| Tack fences it | on a `tack resume` that reaches the site, after `--fence` if the site answers as primary or declares itself primary ([stop-reasons.md](stop-reasons.md#fencedsiteisprimary)) |

## Before

| Check | How |
|---|---|
| The old site is down, not only cut off from this host | Check from a second vantage point with another network path: another site, an out-of-band console, the provider's status page, monitoring. |
| The old primary cannot take writes | Fence it out of band: stop its database nodes, cut its network at the edge, or power it off. What Tack's fence does: [operations.md](../operations.md#what-fencing-does). |
| The old site's applications cannot write | Stop the applications or their Pooler out of band, or cut the site's network at the edge or power it off. |
| Clients no longer reach the old primary | With Strait, at each site that answers, the SiteService's `status.route.members` names no member of the old site. The command is under this table. |
| The new site is a healthy replica | `tack plan -f workload.yaml --to NEW --mode site-loss` shows it as a healthy `Replica`. |

The route of a SiteService at one site:

```sh
kubectl --context SITE -n NS get siteservice NAME -o jsonpath='{.status.route.members}'
```

- The old site's Kubernetes API is not there to fence its primary.
- A cut-off site's applications still reach their local primary, and Strait there still routes them to it, so their writes are lost to the new primary.
- To stop the applications or their Pooler, scale them to 0 from a console that still reaches the site. Cutting off the whole site covers the database and its applications.

## Steps

1. Plan: `tack plan -f workload.yaml --to NEW --mode site-loss`. The plan's notes name the sites it could not read. A position that could not be read is only a warning, with the reason, such as a missing permission ([Permissions](../permissions.md)).
   - Pick the most caught-up site. The plan's `WAL positions` mark one as `most caught up`. The plan refuses a `NEW` that is behind another replica that answers, and names the one to promote. What `NEW` never received is lost: [wal-rpo-rto.md](../wal-rpo-rto.md).
   - To promote a site that is behind anyway (for example, the most caught-up site cannot serve), add `--accept-behind` to the plan and the run. The run then records a `Rebuild` for each replica that was ahead, since it holds writes that `NEW` lacks and cannot follow `NEW`.
2. Run: `tack run -f workload.yaml --to NEW --mode site-loss`, with `--accept-behind` if the plan needed it. The steps at the lost site are `Deferred`. If Git is down too, the run waits, with exit code 4: [break-glass](../operations.md#break-glass).
3. When the old site answers again, resume: `tack resume RUN -f workload.yaml`. If the site answers as primary, or its Cluster still declares itself primary, the run stops with `FencedSiteIsPrimary`: [stop-reasons.md](stop-reasons.md#fencedsiteisprimary). The plan says so when the old site's API answers but its instances are down: the run stops the same way before its first step there.
4. Rebuild the old primary: [rebuild-old-primary.md](rebuild-old-primary.md).

## When Tack refuses the lost site

The plan refuses a site whose API answers with an error, such as a load balancer that answers 503, and a site whose kube context needs a credential plugin that fails or never ends because of the loss. Interrupt a plugin that never ends with Ctrl-C; Tack waits for it with no time limit. For a 5xx or 429, the plan's message names this section. Tack cannot tell such a 503 from one of a live API server that restarts ([RFC#0007](../../rfcs/RFC0007.md), Motivation).

Once the loss is confirmed and the old primary is fenced, as above:

1. Copy the kubeconfig. In the copy, point the lost site's context at an address where nothing listens, such as a free port on `127.0.0.1`.
2. Plan and run with `--kubeconfig COPY`. The site then gives no answer and counts as lost.
3. Use the copy for every `tack resume` while the site stays lost. With the usual kubeconfig, the search for runs fails on the site's error.
4. Once the site answers its API again, delete the copy and resume with the usual kubeconfig. Tack records the site's cluster on its first read ([RFC#0003](../../rfcs/RFC0003.md), Requirement 19).
