# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

Primary: the operators of a database that runs at more than one Kubernetes site, such as platform engineers and database reliability engineers. They know Kubernetes, Flux and CloudNativePG well. They run `tack` from a terminal, often under pressure: during a planned switchover, or after a site is lost.

Secondary: the people who review a run before it starts or read it afterwards, such as a second operator on call; and AI agents, which `tack --help` and `tack --skill` tell to plan first and never start a run on their own.

## Product Purpose

Tack moves the primary of a CloudNativePG database from one Kubernetes site to another, through a planned switchover or a failover after a site is lost. It acts only through the workload's own API, keeps Flux and Git in step, and records every step in a ledger. Success: the primary is where the operator asked, no write is lost in a planned switchover, Git equals live, Flux has nothing to undo, and the ledger says what happened.

Two surfaces:

- **The `tack` CLI** (`cmd/`, `internal/`): plans, runs, resumes and reads runs. Its output marks meaning with colour and symbols on a terminal only.
- **The docs site** (`docs/`, tack.tako.id), VitePress, in the chart table identity of [DESIGN.md](DESIGN.md).

## Positioning

The operator starts every run, and Tack never decides to fail over by itself. It holds Flux narrowly, on only the objects a run changes, and never releases them before Git matches live. A run that stops resumes from its ledger, never from memory. Anything unexpected stops the run and asks. Strait, its sibling, routes clients to the primary; Tack reads Strait's status and never writes it.

## Operating Context

- One Git repository, applied by Flux, serves every site. Each site's database is one CloudNativePG `Cluster`.
- `tack` runs on an operator's machine, with one kube context per site and the operator's own `git` credentials. It stores no credential.
- The ledger is a `SiteFailover` object at the new primary's site.
- A site loss may take Git with it: Tack then switches live, and commits once Git is back.

## Capabilities and Constraints

- Workload support is a driver: `cnpg`, for CloudNativePG in the distributed topology.
- GitOps support is Flux kustomize-controller with a `GitRepository` source.
- Design decisions are RFCs in `rfcs/`. The code conforms to the Active RFCs.

## Brand Commitments

- Names: **Tack**, the `tack` command, the `SiteFailover` ledger. The sibling project is **Strait**.
- Voice: plain, everyday English. The answer first, short sentences, tables for facts, steps as numbered lists. No em-dashes, nothing personal: "the operator", never a person.
- Examples use the sites `a` and `b`, the workload `pg-main`, and documentation hosts such as `git.example.com`.
- Licensed under the Apache License 2.0.

## Evidence on Hand

- The end-to-end test on kind, with CloudNativePG v1.30.1, Flux v2.10.0 and Kubernetes v1.37, in CI on every pull request.
- The failover-time budget in `docs/wal-rpo-rto.md` has no measured numbers yet: they wait for a staging run of `docs/runbooks/measure-failover-time.md`.
- No testimonials, benchmarks or adoption figures exist. Never invent them.

## Product Principles

1. **The operator decides.** Never imply that Tack acts on its own.
2. **Plan before apply.** Every change is shown before it is made.
3. **The ledger is the truth.** Say what was recorded, not what was hoped.
4. **Stop and ask.** On anything unexpected, stop with a reason and the way on.
5. **Honest about limits.** What Tack cannot do, or has not measured, is stated plainly.
