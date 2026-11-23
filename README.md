# Tack

<p><b>Move a database primary between sites, on the operator's command.</b><br>Tack moves the primary of a CloudNativePG database from one Kubernetes site to another: a planned switchover, or a failover after a site is lost. It keeps Flux and Git in step, and records every step in a ledger.</p>

<p>
    <a href="https://github.com/takodotid/tack/actions/workflows/ci.yaml?query=branch%3Amain"><img src="https://github.com/takodotid/tack/actions/workflows/ci.yaml/badge.svg?branch=main" alt="ci"></a>
    <a href="https://github.com/takodotid/tack/releases"><img src="https://img.shields.io/github/v/release/takodotid/tack?label=release&color=b0127a" alt="release"></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-Apache--2.0-16233b" alt="license"></a>
    <a href="https://tack.tako.id"><img src="https://img.shields.io/badge/docs-tack.tako.id-b0127a" alt="docs"></a>
</p>

## What it does

- The operator starts every run, after reading its plan. Tack never fails over by itself.
- Flux is held only on the objects the run changes, and the new values are committed to Git.
- Every step is in a ledger, and a run that stops resumes from it.
- A site loss works even when Git is down.

## Docs

[Install](https://tack.tako.id/guide/install) ·
[Getting started](https://tack.tako.id/guide/getting-started) ·
[Concepts](https://tack.tako.id/guide/concepts) ·
[Runbooks](https://tack.tako.id/operations) ·
[Contributing](https://tack.tako.id/contributing)

[SECURITY.md](https://github.com/takodotid/tack/blob/main/SECURITY.md) says how to report a vulnerability.

## How this project is built

AI coding agents (Claude Code) write the code, tests and docs, and a maintainer approves each RFC, merge and release. Each fix comes with a test, and CI runs the unit tests, the race detector and end-to-end tests on kind. The maintainers run Tack on their own databases.

## License

Apache-2.0. See [LICENSE](LICENSE).
