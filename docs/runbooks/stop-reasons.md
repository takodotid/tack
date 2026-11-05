# Stopped and waiting runs

`tack run` and `tack resume` exit with code 3 when a run stops for the operator, and 4 when it waits for Git. The ledger's reason says why: [RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 10. This runbook gives the way on for each reason.

| Name | Meaning |
|---|---|
| `RUN`, `OTHER` | the run, as `tack status -f workload.yaml` lists it, and another run of the workload |
| `F`, `T` | the run's `from` and `to` sites; `F_CTX`, `T_CTX` their kube contexts |
| `F_CLUSTER`, `T_CLUSTER`, `NS` | their CloudNativePG Clusters and its namespace, from the Workload file |
| `SITE`, `SITE_CTX`, `CLUSTER` | the site the message names, its kube context and its Cluster |
| `KIND`, `NAME` | an object the message names |
| `LEDGER_NS` | the ledger namespace, `tack-system` by default |
| `URL`, `BRANCH` | the Workload's Git URL and branch |
| `BASE`, `HEAD`, `FIX` | the commits the message names, and a commit that fixes Git |

## First

| Check | How |
|---|---|
| No other `tack` process runs `RUN`. | Ask every operator and every script that runs `tack`. A `Running` ledger does not tell a live process from a dead one. |
| Where the run stopped | `tack status RUN -f workload.yaml`: state, reason, message, and each step's state and last result. |
| The workload has a primary | When a planned run demoted F and T does not run as primary yet: [no-primary-after-demotion.md](no-primary-after-demotion.md) first. |

While a run is stopped, its objects stay held: Flux applies nothing to them, including later Git changes. A run left `Running` by a process that ended goes on with `tack resume RUN -f workload.yaml`.

## GitUnreachable

| | |
|---|---|
| When | Git cannot be fetched or pushed at the Git step. State `Waiting`, exit code 4; `git-commit` stays `Pending`. Every fetch failure but a missing branch counts, a refused credential or a wrong URL included ([RFC#0006](../../rfcs/RFC0006.md#requirements), Requirement 3). |
| Check | From the same host and user: `git ls-remote URL BRANCH`. |
| Way on | Fix Git or the access to it, then `tack resume RUN -f workload.yaml`. During a site loss: [break-glass](../operations.md#break-glass). |

## GitChanged

| | |
|---|---|
| When | The branch moved from the run's base before its commit, or the push was rejected because it moved. Flux at a site applied a revision other than the run's base, its commit or the head accepted after it. The branch moved past the run's commit, or no longer contains it, before a release. Nothing is released at a site before its check. |
| Check | The message names the base or the run's commit, and the head. Review `git log --oneline BASE..HEAD`, and `git diff BASE HEAD` for the files of the run's `git-commit` step. |
| Way on | When the new commits leave the run's fields alone, or set them as live has them: `tack resume RUN -f workload.yaml --accept-git-head HEAD`. When the message names one head, `HEAD` stands for it, and any other commit is refused: a head pushed after the stop needs its own stop and review. When they change a field of the run: fix Git first, then accept the head that fixes it. After the run's commit, the accepted head must contain that commit. What the flag changes for the rest of the run: [operations.md](../operations.md#stopped-and-waiting-runs). |

## GitDiffersFromLive

| | |
|---|---|
| When | The check before release, `verify-git-SITE`: Git, rendered as Flux renders it, differs from live. Nothing at that site is released. |
| Check | The message names each path. For the run's own fields it gives Git's value and live's. |
| Way on | Commit a fix to Git, then `tack resume RUN -f workload.yaml --accept-git-head FIX`, with `FIX` the commit that fixes it. The fix moves the branch, so a plain `tack resume` stops with `GitChanged`, naming the head to accept. |

## TokenMissing

| Message | When | Way on |
|---|---|---|
| `Cluster NS/F_CLUSTER has no value at {.status.demotionToken}` | `capture-token` found no token at F, though `wait-demoted-F` saw one before it. CloudNativePG clears the token once the Cluster is no longer a replica, such as after F was promoted again by hand. | Check F: `kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'`. When F runs as primary again, the run must not go on: [abandon-run.md](abandon-run.md). |
| `step promote-T: Cluster NS/F_CLUSTER now has another value at {.status.demotionToken}, not the value step capture-token captured as demotionToken`, or `no value` | The read before the promotion: F was demoted again after the run captured its token, such as by hand after a promotion by hand. T would promote with the token of a demotion that a later one replaced. | The run must not go on: [abandon-run.md](abandon-run.md). Do not copy F's new token into the ledger. |
| `the ledger has no value for demotionToken` | A step needs a value that the ledger's `status.captured` lacks. When `capture-token` is `Succeeded`, the value was lost after the capture, such as by an edit of the ledger, and `capture-token` never runs again: every resume stops the same way. | Restore the value from F, below, then resume. |

To restore the token, all of these must hold. F has stayed demoted since the run demoted it, so its token is still the one T must promote with. T has not promoted.

```sh
# F declares T_CLUSTER and reports no isPrimary; its token is set.
kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'
# T declares F_CLUSTER, has no lastPromotionToken, and reports no isPrimary.
kubectl --context T_CTX -n NS get cluster T_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.lastPromotionToken} {.status.instancesReportedState.*.isPrimary}{"\n"}'

TOKEN=$(kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.status.demotionToken}')
test -n "$TOKEN" && kubectl --context T_CTX -n LEDGER_NS patch sitefailover RUN --subresource=status --type=merge \
  -p "{\"status\":{\"captured\":{\"demotionToken\":\"$TOKEN\"}}}"
unset TOKEN
tack resume RUN -f workload.yaml
```

| Fact | Detail |
|---|---|
| Read the token from F only | T can carry a token of its own from an earlier demotion, which an earlier run already used. A used token makes CloudNativePG hold the Cluster as unrecoverable ([RFC#0004](../../rfcs/RFC0004.md#motivation), Motivation). |
| Permission | The patch needs `patch` on `sitefailovers/status`, beyond Tack's own rows ([operations.md](../operations.md#permissions)). |

## FencedSiteIsPrimary

A site answers as primary, or declares itself primary, where going on would leave two primaries. The run stops before the step, which keeps its state; the stop changes nothing. Each `tack resume` reads the site again, and stops again while it is primary.

| The message | When | Way on |
|---|---|---|
| `step S: site SITE answers as primary (...); it should be fenced. Check it, then resume with --fence SITE`, or `declares itself primary` | The read before the site's steps: a site whose steps can wait for it, such as a lost site back after a site-loss run. | `tack resume RUN -f workload.yaml --fence SITE`: Tack holds the site's objects, then declares the new primary there. Then [rebuild-old-primary.md](rebuild-old-primary.md). |
| `step S: site SITE ... Promoting T gives two primaries, and no step of this run is left to fence SITE, so --fence cannot help` | The read of the other sites, before a Failover step after F's demotion or fence, up to `promote-T`. The step that fences the site is done, and the site was made primary again outside the run, such as F promoted again by hand after the run demoted or fenced it ([no-primary-after-demotion.md](no-primary-after-demotion.md), option 2). | The run's plan no longer fits the sites: abandon it ([abandon-run.md](abandon-run.md)), then plan a new run if the switch is still needed. A demotion of F by hand does not help: the resume then stops with `TokenMissing` before `promote-T`, since F's token is no longer the one the run captured. |
| `step S: site SITE ..., and so does T, which this run promoted: two primaries. No step of this run is left to fence SITE, so --fence cannot help` | The same read after `promote-T`: the site was made primary again outside the run after the run fenced it. Both may take writes. | Pick the site that keeps the writes; the other needs a rebuild ([rebuild-old-primary.md](rebuild-old-primary.md)), and writes it took are lost. Stop writes at the other site first: declare the kept site's Cluster primary there, by hand. Then abandon the run ([abandon-run.md](abandon-run.md)). |
| `step S: site SITE ..., and this run fences it only after it promotes T`, or `..., and so does T, which this run promoted, until this run fences SITE` | The read of the other sites, before or after `promote-T`. A third site is primary, and the run declares T primary there only after the promotion. | Find why SITE is primary. To go on anyway: `tack resume RUN -f workload.yaml --fence SITE`. SITE and T are then both primary until the run declares T there; writes SITE takes meanwhile are lost to T, and SITE needs a rebuild ([rebuild-old-primary.md](rebuild-old-primary.md)), which the ledger does not record. Else abandon the run ([abandon-run.md](abandon-run.md)). |

| Check | How |
|---|---|
| Which sites take writes | At each site, `kubectl --context SITE_CTX -n NS get cluster CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'`. With Strait: `kubectl --context SITE_CTX -n NS get siteservice NAME -o jsonpath='{.status.route.members}'`. |
| Writes at risk | A site that was primary on its own, such as a lost site, may hold writes the new primary lacks; the rebuild drops them. |

## HeldByAnotherRun

| | |
|---|---|
| When | An object of the run is held by another run, or by hand: `kustomize.toolkit.fluxcd.io/reconcile: disabled` without `failover.tako.id/held-by`, or as a label. Found at Hold, before a patch, at release, or again after release. |
| Another run | `tack status OTHER -f workload.yaml`. If the other run took this run over, this run cannot resume: follow the other run. Otherwise one of the two must end: finish the other run and start a new run with `--take-over RUN` if the switch is still needed, or abandon the other run ([abandon-run.md](abandon-run.md)) and start a new run with `--take-over OTHER`. |
| By hand | Find who set the hold and why. Tack never removes a hold it did not set. Remove it only once Git equals live in that object: [abandon-run.md](abandon-run.md#remove-a-hold-left-behind). Then resume. |

## FluxChanged

| | |
|---|---|
| When | The check after release, `verify-noop-SITE`: an object's `metadata.generation` changed after its release, so Flux applied something to it. |
| Check | First, that the switch holds: at every site, `kubectl --context SITE_CTX -n NS get cluster CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'` shows the new primary declared, and `true` only at T. Then what changed: the commits since the run's commit for the object's file, and `kubectl --context SITE_CTX -n NS get KIND NAME --show-managed-fields -o yaml`. |
| Way on | When the change is expected, such as a commit pushed after the run's that changed the object: `tack resume RUN -f workload.yaml --accept-git-head HEAD`. A plain `tack resume` stops again: the generation stays changed. When a site declares the old primary again, Git undid the switch there: restore the run's values in Git at once, and treat any site that ran as primary since as after a site loss: [rebuild-old-primary.md](rebuild-old-primary.md). |

## SiteMismatch

| | |
|---|---|
| When | A site's context reaches another cluster than the run recorded: the UID of its `kube-system` namespace differs. Nothing changed at that site. |
| Way on | Point the context at the site's cluster in the kubeconfig, then `tack resume RUN -f workload.yaml`. If the site's cluster was rebuilt, the run cannot use it: start a new run with `--take-over RUN`. |

## StepFailed

Any other error. Fix the cause the message names, then `tack resume RUN -f workload.yaml`: the step runs again.

| Message | Cause | Way on |
|---|---|---|
| `site SITE is unreachable: no answer to 3 checks` | The site does not answer, and the step cannot wait for it: the site is `to`, or `from` in a planned run. | Once it answers, resume. After a demotion: [no-primary-after-demotion.md](no-primary-after-demotion.md). |
| `site SITE: ... answered its check with an error, so it is not counted as lost` | The site's check failed with anything but no answer, such as a refused credential. | Fix the kubeconfig or the credential, then resume. |
| `... is forbidden: User ... cannot ...` | A missing permission. | Grant it ([operations.md](../operations.md#permissions)), then resume. |
| A `Wait` that timed out, such as `wait-demoted-F`, `wait-promoted-T` or `wait-pooler-T` | The workload did not reach the state in time. When `wait-demoted-F` times out, F may never publish a token: CloudNativePG skips it when F's shutdown outlasts `switchoverDelay` ([RFC#0004](../../rfcs/RFC0004.md#limitations), Limitations). | Check the object the step names. Once it gets there, resume: the wait passes. Without a token from F: [no-primary-after-demotion.md](no-primary-after-demotion.md). |
| `verify-noop-SITE`, with a Kustomization not `Ready` | Flux did not end a reconcile with `ReconciliationSucceeded` within `timeouts.flux`. | Fix the Kustomization, then resume. |
| `routing-SITE` | Strait did not route to T's member alone within `timeouts.routing`. Every release is done. | Check the `SiteService` at that site, then resume. |
| `... is no longer held by this run, so Flux may apply Git over the change` | Someone removed a hold during the run. | Find who, and why. When the workload was changed by hand too, such as F promoted again, the plan may be stale: abandon the run ([abandon-run.md](abandon-run.md)). Only when nothing else changed, hold it again as the message says, then resume. A resume after F was promoted again stops before `promote-T` with `FencedSiteIsPrimary` instead. |
