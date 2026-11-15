# Contributing

- [AGENTS.md](../AGENTS.md) holds the rules for every change, by a human or an agent.
- The design lives in [rfcs/](../rfcs/). A design change starts as an RFC. The RFC is `Draft` while under discussion, and becomes `Active` once the maintainers approve it. The code conforms to the Active RFCs. If the code and an RFC disagree, the RFC is amended first.
- Before a pull request: `make verify test lint`. For a change to how a run behaves, also `make e2e`.
- Commits follow conventional commits: `type(scope): subject`.

## The end-to-end test

The end-to-end test builds two kind clusters with Flux, CloudNativePG, Strait and a local Git server, then switches the primary between them.

| Needs | Version |
|---|---|
| Linux with Docker | Docker 29 or later |
| Go | as in `go.mod` |
| kind | v0.33.0 |
| kubectl | v1.37.2 |
| flux CLI | v2.10.0 exactly |
| helm | v4.3.2 |
| git, openssl, curl, sha256sum | any |
| Internet access | to pull images, and to fetch CloudNativePG's release manifest and Strait's source at the commit pinned in `test/e2e/env/lib.sh` |

`hack/tools/install.sh [DIR]` installs kind, kubectl, flux and helm at these versions into `DIR` (default `bin/tools`). It fails unless each download matches the SHA-256 pinned in the script. Add `DIR` to `PATH`. The script runs on Linux, amd64 or arm64. `TACK_TOOLS_ARCH=arm64` checks the downloads of the other architecture, as CI's `lint` job does.

```sh
make e2e
```

| Scenario | What it checks |
|---|---|
| Planned switchover | the token moves, no write is lost, Git equals live, Flux changes nothing after release |
| A blip during a plan | with one site's node paused for 10 s, the plan checks the site again, succeeds and notes the retry |
| Interrupted run | a run killed before its promotion, whose old primary was then promoted again by hand, stops before the promotion; the next run takes it over, is killed mid-step, and resumes from its ledger |
| Git changed during a run | the run stops; it goes on once the operator accepts the new head |
| Site loss with Git down | Tack switches live and waits for Git (break-glass) |
| A site coming back | a lost site that answers as primary stops the run until the operator fences it |
| Rebuild and switch back | a planned run to the failed-over site is refused until it is rebuilt; then it becomes primary again |
| Routing from Strait | with Strait running at both sites, a run ends once Strait routes to the new primary |

| Command | Effect |
|---|---|
| `TACK_E2E_KEEP=1 make e2e` | keeps the environment after the tests. Their Workload file is `.e2e/workload.yaml`. After `make build`, `bin/tack --kubeconfig .e2e/kubeconfig status -f .e2e/workload.yaml` lists their runs |
| `TACK_E2E_CRD_FROM=vX.Y.Z make e2e` | installs the chart and CRD of release `vX.Y.Z` instead of this tree's, and runs the scenarios with this tree's `tack`. This tests the CLI against the CRD of a site not upgraded yet. Checks of printer columns added after that release are skipped |
| `TACK_E2E_SITES="a b c" test/e2e/run.sh -run TestThreeSites` | builds three sites, the third with a Cluster only, and runs the three-site scenarios: a planned switchover with c following, a planned switchover with c down then resumed, and a run taken over at a lost site (#27) |
| `make e2e-up` | builds the environment and leaves it running |
| `make e2e-down` | removes it |

Everything the test creates is named `tack-e2e*`. [test/e2e/env/README.md](../test/e2e/env/README.md) describes the environment.

## This site

The site is VitePress, in `docs/`, built with [bun](https://bun.sh):

```sh
cd docs
bun install
bun run dev      # serves the site at http://localhost:5173 and reloads on each change
bun run build    # writes docs/.vitepress/dist, and fails on a dead link
```

Each page stays a plain markdown file that also reads well on GitHub, with relative links. A link to a file outside `docs/`, such as an RFC, goes to that file on GitHub. The RFC list and the [limitations](limitations.md) are read from `rfcs/` at build time. The look is in `docs/.vitepress/theme/tokens.css` and [DESIGN.md](../DESIGN.md).
