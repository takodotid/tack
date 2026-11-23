# Stopped and waiting runs

`tack run` and `tack resume` exit with code 3 when a run stops for the operator, and with code 4 when it waits for Git ([exit codes](../reference/cli.md#exit-codes)). The ledger's reason says why ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 10).

## Names

Runbooks write these in capitals. Replace them with the real values.

| Name | Meaning |
|---|---|
| `RUN`, `OTHER` | the run, as `tack status -f workload.yaml` lists it, and another run of the workload |
| `F`, `T` | the run's `from` and `to` sites; `F_CTX`, `T_CTX` their kube contexts |
| `F_CLUSTER`, `T_CLUSTER`, `NS` | their CloudNativePG Clusters and its namespace, from the Workload file |
| `SITE`, `SITE_CTX`, `CLUSTER` | the site the message names, its kube context and its Cluster |
| `KIND`, `NAME` | an object the message names |
| `LEDGER_NS` | the ledger namespace |
| `URL`, `BRANCH` | the Workload's Git URL and branch |
| `BASE`, `HEAD`, `FIX` | the commits the message names, and a commit that fixes Git |

## First

| Check | How |
|---|---|
| No other `tack` process runs `RUN`. | Ask the operators and check every script that runs `tack`. A live process holds a Lease that names it and its host. No holder: the process ended. |
| Where the run stopped | `tack status RUN -f workload.yaml`: state, reason, message, and each step's state and last result. |
| The workload has a primary | After a planned run demoted F and T is not primary yet, go to [no-primary-after-demotion.md](no-primary-after-demotion.md) first. |

A `Running` ledger does not tell a live process from a dead one. Read the Lease:

```sh
kubectl --context T_CTX -n LEDGER_NS get lease RUN -o jsonpath='{.spec.holderIdentity} {.spec.renewTime}{"\n"}'
```

A stopped run keeps its objects held: Flux applies nothing to them, not even later Git changes. A run left `Running` by an ended process goes on with `tack resume RUN -f workload.yaml`.

### Primary check

At each site, for its Cluster:

```sh
kubectl --context SITE_CTX -n NS get cluster CLUSTER -o jsonpath='{.spec.replica.primary} {.status.instancesReportedState.*.isPrimary}{"\n"}'
```

It prints the Cluster the site declares primary, then `true` or `false` for each instance.

## GitUnreachable

- **When:** Git cannot be fetched or pushed at the Git step. State `Waiting`, exit code 4; `git-commit` stays `Pending`. Every fetch failure counts except a missing branch, including a refused credential or a wrong URL ([RFC#0006](../../rfcs/RFC0006.md#requirements), Requirement 3).
- **Check:** `git ls-remote URL BRANCH`, from the same host and user.
- **Way on:** fix Git or the access to it, then `tack resume RUN -f workload.yaml`. During a site loss: [break-glass](../operations.md#break-glass).

## GitChanged

- **When:** any of these. Nothing at a site is released before its check.
  - The branch moved away from the run's base before the run's commit, or the push was rejected because it moved.
  - Flux at a site applied a revision other than the base, the run's commit or the head accepted after it.
  - Before a release, the branch moved past the run's commit or no longer contains it.
- **Check:** the message names the base or the run's commit, and the head. Review `git log --oneline BASE..HEAD`, and `git diff BASE HEAD` for the files of the run's `git-commit` step.
- **Way on:**
  1. The new commits leave the run's fields alone, or set them as live has them: `tack resume RUN -f workload.yaml --accept-git-head HEAD`.
  2. They change a field of the run: fix Git first, then accept the head that fixes it.

  When the message names one head, `HEAD` must be that head; any other commit is refused. A head pushed after the stop needs its own stop and review. After the run's commit, the accepted head must contain that commit. What the flag changes for the rest of the run: [operations.md](../operations.md#stopped-and-waiting-runs).

## GitDiffersFromLive

- **When:** the check before release, `verify-git-SITE`, found that Git, rendered as Flux renders it, differs from live. Nothing at that site is released.
- **Check:** the message names each path. For the run's own fields, it gives Git's value and live's.
- **Way on:** commit a fix to Git, then `tack resume RUN -f workload.yaml --accept-git-head FIX`. The fix moves the branch, so a plain `tack resume` stops with `GitChanged` and names the head to accept.

## TokenMissing

| Message | When | Way on |
|---|---|---|
| `Cluster NS/F_CLUSTER has no value at {.status.demotionToken}` | `capture-token` found no token at F, though `wait-demoted-F` saw one. CloudNativePG clears it once F is no longer a replica, such as after a manual promotion of F. | Run the [primary check](#primary-check) at F. When F runs as primary again, abandon the run: [abandon-run.md](abandon-run.md). |
| `step promote-T: Cluster NS/F_CLUSTER now has another value at {.status.demotionToken}, not the value step capture-token captured as demotionToken`, or `no value` | F was demoted again after the capture, such as by hand. T would promote with the token of a replaced demotion. | Abandon the run ([abandon-run.md](abandon-run.md)). Do not copy F's new token into the ledger. |
| `the ledger has no value for demotionToken` | With `capture-token` `Succeeded`, the value was lost after the capture, such as by an edit of the ledger. `capture-token` never runs again. | Restore the value from F, as below. |

Restore the token only when F has stayed demoted since the run demoted it and T has not promoted. Read it from F only: T can carry the token of an earlier demotion, and CloudNativePG holds a Cluster that uses a used token as unrecoverable ([RFC#0004](../../rfcs/RFC0004.md#motivation), Motivation).

1. **Check F.** The [primary check](#primary-check) at F prints `T_CLUSTER` and no `true`, and `status.demotionToken` is set.
2. **Check T.** It declares `F_CLUSTER`, has no `lastPromotionToken`, and reports no `isPrimary`:

   ```sh
   kubectl --context T_CTX -n NS get cluster T_CLUSTER -o jsonpath='{.spec.replica.primary} {.status.lastPromotionToken} {.status.instancesReportedState.*.isPrimary}{"\n"}'
   ```

3. **Restore and resume.** The patch needs `patch` on `sitefailovers/status`, beyond Tack's own permissions ([Permissions](../permissions.md)):

   ```sh
   TOKEN=$(kubectl --context F_CTX -n NS get cluster F_CLUSTER -o jsonpath='{.status.demotionToken}')
   test -n "$TOKEN" && kubectl --context T_CTX -n LEDGER_NS patch sitefailover RUN --subresource=status --type=merge \
     -p "{\"status\":{\"captured\":{\"demotionToken\":\"$TOKEN\"}}}"
   unset TOKEN
   tack resume RUN -f workload.yaml
   ```

## FencedSiteIsPrimary

A site answers as primary, or declares itself primary, where going on would leave two primaries. The run stops before the step, which keeps its state. Each `tack resume` reads the site again and stops while it is primary.

First find which sites take writes: the [primary check](#primary-check) at each site, and with Strait `kubectl --context SITE_CTX -n NS get siteservice NAME -o jsonpath='{.status.route.members}'`. A site that was primary on its own, such as a lost site, may hold writes the new primary lacks. The rebuild drops them.

| The message | When | Way on |
|---|---|---|
| `step S: site SITE answers as primary (...); it should be fenced. Check it, then resume with --fence SITE`, or `declares itself primary` | The read before the site's steps, for a site whose steps can wait for it, such as a lost site that is back. | `tack resume RUN -f workload.yaml --fence SITE`. Tack holds the site's objects, then declares the new primary there. Then [rebuild-old-primary.md](rebuild-old-primary.md). |
| `step S: site SITE ... Promoting T gives two primaries, and no step of this run is left to fence SITE, so --fence cannot help` | Before `promote-T`, after F's demotion or fence: the site was made primary again outside the run, such as F by hand ([no-primary-after-demotion.md](no-primary-after-demotion.md), option 2). | The plan no longer fits the sites. Abandon the run ([abandon-run.md](abandon-run.md)), then plan a new run if the switch is still needed. |
| `step S: site SITE ..., and so does T, which this run promoted: two primaries. No step of this run is left to fence SITE, so --fence cannot help` | The same, after `promote-T`. Both sites may take writes. | Pick the site that keeps the writes, and declare its Cluster primary at the other site by hand. Then abandon the run ([abandon-run.md](abandon-run.md)) and [rebuild](rebuild-old-primary.md) the other site. |
| `step S: site SITE ..., and this run fences it only after it promotes T`, or `..., and so does T, which this run promoted, until this run fences SITE` | A third site is primary, and the run declares T primary there only after the promotion. | Find out why SITE is primary. To go on anyway: `tack resume RUN -f workload.yaml --fence SITE`. Otherwise abandon the run ([abandon-run.md](abandon-run.md)). |

- **No step left to fence SITE, before `promote-T`:** demoting F by hand does not help, because the resume stops with `TokenMissing`.
- **No step left to fence SITE, after `promote-T`:** declaring the Cluster primary at the other site stops writes there. The writes that site took are lost.
- **A third site that the run fences after the promotion:** with `--fence`, SITE and T are both primary until the run declares T at SITE. The writes SITE takes meanwhile are lost, and SITE needs a [rebuild](rebuild-old-primary.md).

## HeldByAnotherRun

- **When:** another run or a person holds an object of the run. A hold by hand is `kustomize.toolkit.fluxcd.io/reconcile: disabled` without `failover.tako.id/held-by`, or set as a label. Tack finds it at Hold, before a patch, at release, or again after release.
- **Another run:** `tack status OTHER -f workload.yaml`. If the other run took this run over, this run cannot resume: follow the other run. Otherwise one of the two must end:
  - Finish the other run, then start a new run with `--take-over RUN` if the switch is still needed.
  - Or abandon the other run ([abandon-run.md](abandon-run.md)), then start a new run with `--take-over OTHER`.
- **By hand:** find out who set the hold and why. Tack never removes a hold it did not set. Remove it once Git equals live in that object ([abandon-run.md](abandon-run.md#remove-a-hold-left-behind)), then resume.

## FluxChanged

- **When:** the check after release, `verify-noop-SITE`, found that an object's `metadata.generation` changed after its release: Flux applied something to it.
- **Check:**
  1. The [primary check](#primary-check) at every site shows the new primary declared, and `true` only at T.
  2. Find what changed: the commits since the run's commit for the object's file, and `kubectl --context SITE_CTX -n NS get KIND NAME --show-managed-fields -o yaml`.
- **Way on:** for an expected change, such as a later commit that changed the object: `tack resume RUN -f workload.yaml --accept-git-head HEAD`. A plain `tack resume` stops again, since the generation stays changed. When a site declares the old primary again, Git undid the switch there: restore the run's values in Git at once, and treat any site that ran as primary since then as after a site loss ([rebuild-old-primary.md](rebuild-old-primary.md)).

## SiteMismatch

- **When:** a site's context reaches a different cluster than the one the run recorded: the UID of its `kube-system` namespace differs. Nothing changed at that site.
- **Way on:** point the context at the site's cluster in the kubeconfig, then `tack resume RUN -f workload.yaml`. If the site's cluster was rebuilt, the run cannot use it: start a new run with `--take-over RUN`.

## StepFailed

Any other error. Fix the cause the message names, then `tack resume RUN -f workload.yaml`. The step runs again.

| Message | Cause | Way on |
|---|---|---|
| `site SITE is unreachable: no answer to 3 checks` | The site does not answer, and the step cannot wait for it: the site is `to`, or `from` in a planned run. | Resume once the site answers. After a demotion: [no-primary-after-demotion.md](no-primary-after-demotion.md). |
| `site SITE: ... answered its check with an error, so it is not counted as lost` | The check failed with an error, not with no answer, such as a refused credential. | Fix the kubeconfig or the credential, then resume. |
| `... is forbidden: User ... cannot ...` | A missing permission. | Grant it ([Permissions](../permissions.md)), then resume. |
| A `Wait` that timed out, such as `wait-demoted-F`, `wait-promoted-T` or `wait-pooler-T` | The workload did not reach the state in time. `wait-demoted-F` may never see a token: CloudNativePG skips it when F's shutdown outlasts `switchoverDelay` ([RFC#0004](../../rfcs/RFC0004.md#limitations), Limitations). | Check the object the step names, then resume. Without a token from F: [no-primary-after-demotion.md](no-primary-after-demotion.md). |
| `step S: the read before the first Failover step: driver cnpg no longer allows the run: ...` | No Failover step ran yet, and the sites are no longer what the plan needs, such as a primary switched by hand while the run was stopped in Hold. | Resume when the sites are back as planned. Otherwise abandon the run ([abandon-run.md](abandon-run.md)) and plan again. |
| `step git-commit: ...: the run would write ... The ledger's Git step does not match its Workload` | The Git step names another file, path or document than the Workload maps, or than the object's manifest ([RFC#0006](../../rfcs/RFC0006.md#requirements), Requirement 15). Nothing was committed. | Find out who wrote the ledger ([Who may write ledgers](../permissions.md#who-may-write-ledgers)). Abandon the run: [abandon-run.md](abandon-run.md). |
| `verify-noop-SITE`, with a Kustomization not `Ready` | Flux did not end a reconcile with `ReconciliationSucceeded` within `timeouts.flux`. | Fix the Kustomization, then resume. |
| `verify-noop-SITE`: `the Kustomization NS/NAME at site SITE is suspended (spec.suspend)` | A released object's Kustomization is suspended: Flux applies nothing to it. | Find out who suspended it, and why. Run `flux --context SITE_CTX resume kustomization NAME -n NS`, then resume the run. |
| `routing-SITE` | Strait did not route to T's member alone within `timeouts.routing`. Every release is done. | Check the `SiteService` at that site, then resume. |
| `... is no longer held by this run, so Flux may apply Git over the change` | Someone removed a hold during the run. | Find out who, and why. If the workload was also changed by hand, abandon the run ([abandon-run.md](abandon-run.md)). Otherwise hold the object again as the message says, then resume. |

- A `git-commit` mismatch comes from a ledger written outside Tack, or a manifest moved in Git after the plan.
- A change by hand to the workload is, for example, F promoted again.
