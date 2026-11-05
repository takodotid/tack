# Fail over after a site loss

`--mode site-loss` promotes a new primary without a hand-over from the old one. Tack counts a site as lost only when its API gives no answer to the operator's host ([RFC#0007](../../rfcs/RFC0007.md), Requirement 5). It cannot tell a lost site from one that still runs but is cut off from that host.

If the old site still runs, its Cluster stays primary and takes writes until one of these happens:

| Event | When |
|---|---|
| Flux at the old site applies the run's commit | only once that site reaches Git again, and after the run's commit exists |
| Tack fences it | on a `tack resume` that reaches the site, after `--fence` if it answers as primary or declares itself primary |

Until then there are two primaries. Confirm the loss, and fence the old primary, before the run. Tack does neither.

## Before

| Check | How |
|---|---|
| The old site is down, not only cut off from this host | Check it from a second vantage point that does not share this host's network path: another site, an out-of-band console, the provider's status page, monitoring. |
| The old primary cannot take writes | Fence it out of band: stop its database nodes, cut its network at the edge, or power it off. Its Kubernetes API is not there to do it. |
| Clients no longer reach the old primary | With Strait, at each site that answers, the route names no member of the old site: `kubectl --context SITE -n NS get siteservice NAME -o jsonpath='{.status.route.members}'`. |
| The new site is a healthy replica | `tack plan -f workload.yaml --to NEW --mode site-loss` shows it as a healthy `Replica`. |

## Steps

1. **Plan.** `tack plan -f workload.yaml --to NEW --mode site-loss`. Its notes name the sites it could not read.
2. **Run.** `tack run -f workload.yaml --to NEW --mode site-loss`. The steps at the lost site are `Deferred`. With Git down too, the run waits with exit code 4: [break-glass](../operations.md#break-glass).
3. **When the old site answers again,** `tack resume RUN -f workload.yaml`. If it answers as primary, or its Cluster still declares itself primary, the run stops with `FencedSiteIsPrimary`; check it, then `tack resume RUN -f workload.yaml --fence OLD`. A run whose old site answers its API at the start, with its instances down, stops the same way before its first step there; the plan says so.
4. **Rebuild the old primary:** [rebuild-old-primary.md](rebuild-old-primary.md).

## When Tack refuses the lost site

A site whose API answers with an error, such as a load balancer that answers 503, is not counted as lost; neither is a site whose kube context needs a credential plugin that fails because of the loss. The plan refuses.

Only once the loss is confirmed and the old primary fenced, as above:

1. Copy the kubeconfig. In the copy, point the lost site's context at an address where nothing listens, such as a free port on `127.0.0.1`.
2. Plan and run with `--kubeconfig COPY`. The site then gives no answer, and counts as lost.
3. Use the copy for every `tack resume` while the site stays lost: with the usual kubeconfig, the search for runs fails on the site's error.
4. Once the site is back and answers its API, delete the copy and resume with the usual kubeconfig. Tack records the site's cluster on its first read ([RFC#0003](../../rfcs/RFC0003.md), Requirement 19).
