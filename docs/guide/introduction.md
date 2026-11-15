# Introduction

Tack moves the primary of a CloudNativePG database from one Kubernetes site to another. It is for setups where Flux deploys every site from one Git repository. It handles two cases: a planned switchover, and a failover after a site is lost.

The operator starts every run. Tack never decides to fail over by itself.

## Why

Moving the primary by hand fails in two ways:

| What goes wrong | What Tack does |
|---|---|
| Flux applies the old values from Git and undoes the switch. | Stops Flux from applying the objects the run changes, and only those. Commits the new values to Git. Lets Flux apply those objects again once Git matches live. |
| The old primary's demotion token must be carried to the new primary's cluster, between two steps. Promoting without the token can fork the data: the two sites then hold different histories. | Waits for the old primary to publish the token. Records the token in the ledger. Promotes the new primary with it. |

Tack changes the database only through CloudNativePG's own API, and records every step in a ledger, the `SiteFailover` resource. [Concepts](concepts.md) shows how a run works. [RFC#0001](../../rfcs/RFC0001.md) gives the full scope.

## What keeps it safe

| Property | How |
|---|---|
| The operator starts every run | Only `tack run` starts a run. Tack has no timer, watch or health check that starts one. |
| Plan first | `tack plan` reads every site and Git. It prints every step, patch, wait, Git edit and check. It changes nothing. |
| Typed confirmation | `tack run` and `tack resume` print the plan or the ledger, then ask the operator to type the workload's name. Without a terminal they refuse, unless `--yes` is passed for a run that a human approved. |
| Narrow hold | Tack holds only the objects the run changes. The hold is an annotation on the live objects: nothing is annotated in Git. No Kustomization is suspended. |
| Resume from the ledger | The plan is stored in the ledger when the run starts. `tack resume` continues that plan and never plans again. Every step is safe to repeat. |
| Git equals live before release | Tack renders the run's commit the way Flux does. It releases an object only once that render matches live. After release, it checks that Flux changed nothing. |
| Stop and ask | Anything unexpected stops the run, with exit code 3 and a reason in the ledger. For example: Git changed during the run, a token is missing, a lost site came back as primary, or an object is held by someone else. |
| Break-glass | If a site is lost and Git is down too, Tack still switches live. Then it waits at the Git step, with exit code 4. Once Git is back, `tack resume` commits. Nothing is released before that. |
| Explicit contexts | Each site uses the kube context named in the Workload file. Tack never falls back to the current context. |
| AI agents | `tack --help` and `tack --skill` tell agents never to start `tack run` or `tack resume` without a human's explicit instruction for that run, and to run `tack plan` first. |
| No secrets | Git access comes from the operator's own `git` configuration. The ledger holds no secret, and `tack status` prints captured values by name only. |

## What v1 supports

| Area | Support |
|---|---|
| Workload | CloudNativePG in the distributed topology: one `Cluster` per site, the primary named by `.spec.replica.primary`, and an optional read-write `Pooler` per site. The driver is `cnpg`. |
| GitOps | Flux kustomize-controller with a `GitRepository` source. One Git repository serves all sites. |
| Routing check | Optional. Reads the `SiteService` status of [Strait](https://github.com/takodotid/strait) v1.0.0 or later. Strait routes clients to the primary. |
| Platforms | Linux, macOS and Windows, amd64 and arm64. |
| Tested with | CloudNativePG v1.30.1, Flux v2.10.0 and Kubernetes v1.37, in the end-to-end test. |

## Versions

Releases are on the [releases page](https://github.com/takodotid/tack/releases). The API is `failover.tako.id/v1`, for both the `SiteFailover` ledger and the Workload file. A change that would break a user comes as a new API version. `failover.tako.id/v1alpha1` was the API of v0.1.x. It is deprecated but still served: see [Upgrade from v0.1.x](upgrade.md#upgrade-from-v01x).
