# Tack

Tack switches a stateful workload's primary from one site to another: a planned switchover, or a failover after a site is lost. It acts through the workload's own declarative API, keeps GitOps in step, and records every step in a ledger, the `SiteFailover` resource.

The operator starts every run. Tack never decides to fail over by itself.

v1 supports CloudNativePG in its distributed topology, deployed by Flux.

## Quick start: the end-to-end test

The end-to-end test builds two kind clusters with Flux, CloudNativePG and a local Git server, then switches the primary between them.

| Needs | Version |
|---|---|
| Linux with Docker | Docker 29 or later |
| Go | as in `go.mod` |
| kind | v0.33.0 |
| kubectl | v1.37 |
| flux CLI | v2.10.0 |

```sh
make e2e
```

| Scenario | What it checks |
|---|---|
| Planned switchover | the token moves, no write is lost, Git equals live, Flux changes nothing after release |
| Interrupted run | a run killed mid-step resumes from its ledger |
| Git changed during a run | the run stops; it goes on once the operator accepts the new head |
| Site loss with Git down | Tack switches live and waits for Git (break-glass) |
| A site coming back | a lost site that answers as primary stops the run until the operator fences it |
| Rebuild and switch back | a planned run to the failed-over site is refused until it is rebuilt; then it becomes primary again |
| Routing from Strait | with Strait's `SiteService` CRD installed, a run waits until the route names the new primary |

`make e2e-up` keeps the environment for a look around; `make e2e-down` removes it. Everything it creates is named `tack-e2e*`.

## Build

| Command | Result |
|---|---|
| `make build` | `bin/tack` |
| `make dist` | static binaries for Linux and macOS, with man pages and the CRD, in `dist/` |
| `make test` | unit and golden tests |
| `make verify` | generated files are current, code is formatted and vetted |

## Use

1. Install the CRD `config/crd/failover.tako.id_sitefailovers.yaml` and create the namespace `tack-system` at every site.
2. Describe the workload in a Workload file: its sites, their kube contexts, the Git repository and where each field lives in it. See [RFC#0009](rfcs/RFC0009.md) and [RFC#0006](rfcs/RFC0006.md).
3. Plan, then run:

```sh
tack plan -f workload.yaml --to b
tack run  -f workload.yaml --to b
tack status -f workload.yaml
```

`tack --skill` prints a guide for AI agents. `man tack` documents every command.

## Design

The design lives in [rfcs/](rfcs/). Runbooks are in [docs/runbooks/](docs/runbooks/).

## License

Apache-2.0. See [LICENSE](LICENSE).
