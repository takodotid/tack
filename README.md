# Tack

<p><b>Move a database primary between sites, on your command.</b><br>Tack moves the primary of a CloudNativePG database from one Kubernetes site to another: a planned switchover, or a failover after a site is lost. It keeps Flux and Git in step, and records every step in a ledger.</p>

<p>
    <a href="https://github.com/takodotid/tack/actions/workflows/ci.yaml?query=branch%3Amain"><img src="https://github.com/takodotid/tack/actions/workflows/ci.yaml/badge.svg?branch=main" alt="ci"></a>
    <a href="https://github.com/takodotid/tack/releases"><img src="https://img.shields.io/github/v/release/takodotid/tack?label=release&color=b0127a" alt="release"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-16233b" alt="license"></a>
    <a href="https://tack.tako.id"><img src="https://img.shields.io/badge/docs-tack.tako.id-b0127a" alt="docs"></a>
</p>

<p>
    <a href="https://tack.tako.id/guide/install">Install</a> ·
    <a href="https://tack.tako.id/guide/getting-started">Getting started</a> ·
    <a href="https://tack.tako.id/guide/concepts">Concepts</a> ·
    <a href="https://tack.tako.id/operations">Operations</a> ·
    <a href="https://tack.tako.id/reference/cli">Reference</a>
</p>

## What it does

- **The operator starts every run.** Tack never fails over by itself. `tack plan` reads every site and Git, prints every step, and changes nothing. `tack run` asks you to type the workload's name.
- **Flux is held narrowly.** Tack annotates only the live objects the run changes, so Flux stops applying them. It commits the new values to Git, and lets Flux apply those objects again once Git matches live.
- **The demotion token is carried across.** In a planned switchover the old primary demotes, Tack records its token, and the new primary promotes with it. No write is lost.
- **Every step is in a ledger.** A `SiteFailover` object records each step before it starts and after it ends. A run that stops resumes from its ledger, never from memory.
- **Site loss, even with Git down.** Tack switches live, records where the histories fork, and commits once Git is back. Nothing is released before Git matches live.
- **Anything unexpected stops the run** with a reason: Git changed, a token is missing, a lost site came back as primary, an object is held by someone else.

It supports CloudNativePG in the distributed topology, Flux kustomize-controller with a `GitRepository`, and, optionally, [Strait](https://github.com/takodotid/strait) for routing. It runs on Linux, macOS and Windows, amd64 and arm64.

## A switchover

With a [Workload file](https://tack.tako.id/workload) that names the sites, their kube contexts and the Git repository:

```sh
tack plan -f workload.yaml --to b      # read every site and Git, print every step, change nothing
tack run -f workload.yaml --to b       # print the plan again, ask for the workload's name, then run it
tack status -f workload.yaml           # the workload's runs at every site
tack resume RUN -f workload.yaml       # continue a run that stopped (exit code 3) or waits (exit code 4)
```

After a site loss, confirm the loss first ([runbook](https://tack.tako.id/runbooks/site-loss)), then add `--mode site-loss` to `plan` and `run`.

## Install

Download the archive for your platform from the [releases page](https://github.com/takodotid/tack/releases), check it against `SHA256SUMS`, and put `tack` on your `PATH`. Then install the CRD at every site, in the ledger namespace:

```sh
VERSION=v1.6.0
for ctx in site-a site-b; do
  helm --kube-context "$ctx" upgrade --install tack oci://ghcr.io/takodotid/charts/tack --version "${VERSION#v}" \
    --namespace tack-system --create-namespace
done
```

[Install](https://tack.tako.id/guide/install) has every step: Windows, the chart's rules, what else Tack needs, shell completion, and how to verify a release. Before an upgrade, read [Upgrade](https://tack.tako.id/guide/upgrade).

## Docs

The documentation site is [tack.tako.id](https://tack.tako.id). Its pages are in [`docs/`](docs/), and read the same on GitHub.

| Section | What it holds |
|---|---|
| [Guide](https://tack.tako.id/guide/introduction) | why Tack exists and what keeps it safe, install, a first switchover, how a run works, upgrades |
| [Operations](https://tack.tako.id/operations) | reading a run, stopped runs, data loss and downtime, fencing, break-glass, permissions, and the runbooks |
| [Reference](https://tack.tako.id/reference/cli) | the `tack` command, the Workload file, the ledger, metrics, and the RFCs |
| [Limitations](https://tack.tako.id/limitations) | what Tack does not do yet, from each RFC |

`tack --help`, `tack <command> --help` and `man tack` document every command, flag and exit code. `tack --skill` prints the guide for AI agents. [SECURITY.md](SECURITY.md) says how to report a vulnerability.

## How this project is built

Tack is built with AI coding agents (Claude Code), and a person directs and approves the work.

**What the agents do:** write code, tests and docs, review each other's changes, and look for bugs and gaps.

**What a person decides:**
- **Design.** Every change starts as an RFC, and a maintainer approves each RFC and each change to one.
- **Scope.** A maintainer decides what goes into each round of work and what waits.
- **Merges and releases.** Only a maintainer merges a pull request. Releases come from tags signed by a maintainer, and a maintainer approves each release run.

**How the work is checked:**
- Each fix comes with a test that fails without the fix.
- CI runs the unit tests, the race detector, and end-to-end tests on kind for every pull request.
- Before a release, the change is tested on real clusters, not only in CI.
- Releases are signed, and you can verify every release asset (see [Verify a release](docs/guide/install.md#verify-a-release)).

**Why we trust it in production:** we run Tack ourselves, for our own databases. A failover tool that loses data or leaves two primaries would hurt us first.

If you find a problem, open an issue. Saying that it looks AI-written is fine, as long as you say what's wrong.

## Contributing

[AGENTS.md](AGENTS.md) holds the rules for every change, by a human or an agent. A design change starts as an RFC in [rfcs/](rfcs/). Before a pull request, run `make verify test lint`, and `make e2e` for a change to how a run behaves. [Contributing](https://tack.tako.id/contributing) has the end-to-end test and how to build this site.

## License

Apache-2.0. See [LICENSE](LICENSE).
