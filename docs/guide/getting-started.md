# Getting started

You need the CLI and the CRD at every site ([Install](install.md)), and a Workload file that names the sites, their kube contexts and the Git repository ([the Workload file](../workload.md)).

## A planned switchover

With a [Workload file](../workload.md) for sites a and b, a planned switchover from a to b:

```sh
# Read every site and Git, print every step, change nothing.
tack plan -f workload.yaml --to b

# Print the plan again, ask for the workload's name, then run it.
tack run -f workload.yaml --to b

# List the workload's runs at every site, or show one run step by step.
tack status -f workload.yaml
tack status pg-main-20261027-101500 -f workload.yaml

# Continue a run that stopped (exit code 3) or waits (exit code 4).
tack resume pg-main-20261027-101500 -f workload.yaml

# Delete the ledgers of old runs that succeeded, keeping the 20 newest of them.
tack prune -f workload.yaml --keep 20
```

After a site loss, add `--mode site-loss` to `plan` and `run`, after the checks of [Fail over after a site loss](../runbooks/site-loss.md). [Running Tack](../operations.md) covers stopped runs and break-glass.

## What a plan shows

An excerpt of `tack plan`:

```text
Plan for workload pg-main: Planned, site a to site b
Ledger: SiteFailover tack-system/pg-main-20261027-101500 at site b

Sites:
  a  Primary  declares a  healthy  Cluster db/pg-a: phase "Cluster in healthy state", declares pg-a primary, pod pg-a-1; Pooler pg-a-pooler-rw has 2 instances
  b  Replica  declares a  healthy  Cluster db/pg-b: phase "Cluster in healthy state", declares pg-a primary, pod pg-b-1; Pooler pg-b-pooler-rw has 0 instances
...
Failover
...
   9. demote-a  site a
      demote Cluster pg-a: declare pg-b primary and drop any old token
        patch Cluster db/pg-a (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":null}}}
  10. wait-demoted-a  site a
      wait until Cluster pg-a has demoted and published its demotion token
        wait up to 15m0s on Cluster db/pg-a until:
          {.status.demotionToken} is not empty
          {.status.switchReplicaClusterStatus.inProgress} NotEquals "true"
  11. capture-token  site a
      record the demotion token of Cluster pg-a in the ledger
        record {.status.demotionToken} of Cluster db/pg-a in the ledger as $(demotionToken)
  12. promote-b  site b
      promote Cluster pg-b with the demotion token, in one patch
        patch Cluster db/pg-b (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":"$(demotionToken)"}}}
...
PromoteToGitOps
  18. git-commit  Git
      commit the live state to ssh://***@git.example.com/gitops.git branch main
        clusters/site-a/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site a, Cluster db/pg-a spec.replica.primary)
        clusters/site-a/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site a, Cluster db/pg-a spec.replica.promotionToken)
        clusters/site-a/db/pooler.yaml, document 0: spec.instances = 0  (site a, Pooler db/pg-a-pooler-rw spec.instances)
        clusters/site-b/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site b, Cluster db/pg-b spec.replica.primary)
        clusters/site-b/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site b, Cluster db/pg-b spec.replica.promotionToken)
        clusters/site-b/db/pooler-instances.yaml: spec.instances = 2  (site b, Pooler db/pg-b-pooler-rw spec.instances)
        commit message: tack: pg-main primary a to b (planned)
...

Nothing was changed. To run this plan: tack run -f workload.yaml --to b --from a --mode planned
```

[The tack command](../reference/cli.md) lists every command, flag and exit code.

## Try it on kind

The end-to-end test builds two kind clusters with Flux, CloudNativePG, Strait and a local Git server, then switches the primary between them. [Contributing](../contributing.md#the-end-to-end-test) says what it needs and how to keep the environment to try `tack` against it.
