# Getting started

A run needs the CLI and the CRD at every site ([Install](install.md)) and a [Workload file](../workload.md) that names the sites, their kube contexts and the Git repository.

## A planned switchover

A planned switchover from site a to site b:

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

After a site loss, add `--mode site-loss` to `plan` and `run`, after the checks of [Fail over after a site loss](../runbooks/site-loss.md).

## What a plan shows

An excerpt of `tack plan`:

```text
Plan for workload pg-main: Planned, site a to site b
Ledger: SiteFailover tack-system/pg-main-20261027-101500 at site b
...
   9. demote-a  site a
      demote Cluster pg-a: declare pg-b primary and drop any old token
        patch Cluster db/pg-a (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":null}}}
  11. capture-token  site a
      record the demotion token of Cluster pg-a in the ledger
  12. promote-b  site b
      promote Cluster pg-b with the demotion token, in one patch
        patch Cluster db/pg-b (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":"$(demotionToken)"}}}
...
Nothing was changed. To run this plan: tack run -f workload.yaml --to b --from a --mode planned
```

## Try it on kind

To try `tack` against the end-to-end environment, see [Contributing](../contributing.md#the-end-to-end-test).
