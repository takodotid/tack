# Introduction

Tack moves the primary of a CloudNativePG database from one Kubernetes site to another. It is for setups where Flux deploys every site from one Git repository. It handles a planned switchover and a failover after a site is lost. The operator starts every run; Tack never decides to fail over by itself.

## Why

Moving the primary by hand goes wrong in two ways:

| What goes wrong | What Tack does |
|---|---|
| Flux applies the old values from Git and undoes the switch. | Stops Flux from applying the objects the run changes, and only those. Commits the new values to Git. Lets Flux apply those objects again once Git matches live. |
| The old primary's demotion token must reach the new primary's cluster. Promoting without it can fork the data into two histories. | Waits for the old primary to publish the token. Records the token in the ledger. Promotes the new primary with it. |

Tack changes the database only through CloudNativePG's API and records every step in a ledger, the `SiteFailover` resource.

## What keeps it safe

| Property | How |
|---|---|
| Plan first | `tack plan` reads every site and Git, prints every step, and changes nothing. |
| Typed confirmation | `tack run` and `tack resume` ask the operator to type the workload's name. Without a terminal they refuse, unless `--yes` is passed. Rules for AI agents: [The tack command](../reference/cli.md#rules-for-ai-agents). |
| Git equals live before release | Tack releases an object only once the run's commit, rendered the way Flux does, matches live. After release, it checks that Flux changed nothing. |
| Stop and ask | Anything unexpected stops the run, with exit code 3 and a reason in the ledger. A lost site with Git down is [break-glass](../operations.md#break-glass). |
| Explicit contexts | Each site uses the kube context the Workload file names. Tack never falls back to the current context. |

## What v1 supports

| Area | Support |
|---|---|
| Workload | CloudNativePG in the distributed topology: one `Cluster` per site, the primary named by `.spec.replica.primary`, and an optional read-write `Pooler` per site. The driver is `cnpg`. |
| GitOps | Flux kustomize-controller with a `GitRepository` source. One Git repository serves all sites. |
| Routing check | Optional. Reads the `SiteService` status of [Strait](https://github.com/takodotid/strait) v1.0.0 or later. Strait routes clients to the primary. |
| Platforms | Linux, macOS and Windows, amd64 and arm64. |
| Tested with | CloudNativePG v1.30.1, Flux v2.10.1 and Kubernetes v1.37, in the end-to-end test. |

## Versions

The API is `failover.tako.id/v1`, for the `SiteFailover` ledger and the Workload file. A breaking change comes as a new API version. `v1alpha1` (v0.1.x) is deprecated but still served: [Upgrade from v0.1.x](upgrade.md#upgrade-from-v01x).
