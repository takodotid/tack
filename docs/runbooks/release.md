# Release

The operator's steps to publish a release. What the workflow checks, and in which order: [RFC0009, Releases](../../rfcs/RFC0009.md#releases).

| Step | What to do |
|---|---|
| 1. Merge | Merge the release's pull request into `main`. The README's latest release and its install steps name the new version. |
| 2. Wait for CI | Wait until the `ci` run of the merge commit on `main` has finished with every job successful: `gh run list --workflow ci.yaml --branch main`. |
| 3. Tag | Sign an annotated tag on that commit, and push it: `git tag -s vX.Y.Z COMMIT -m "Tack vX.Y.Z"`, then `git push origin vX.Y.Z`. GitHub must show the tag as Verified: the key must belong to the tagger's GitHub account. |
| 4. Approve | When the environment `release` has reviewers, approve the run. Nothing runs before. |
| 5. Check | The job `publish` ends by verifying the release as the README's [Verify a release](../../README.md#verify-a-release) shows. Run those commands once more from another machine. |

## When the release stops

| The job's log says | What to do |
|---|---|
| `gate.sh: release refused: tag ... is not vX.Y.Z` | Tag the commit again with a name `vX.Y.Z`. |
| `is not annotated`, or `does not verify the signature` | Tag again with `git tag -s`, with a key that GitHub has for the tagger's account. |
| `is not on main`, or `points at ..., not at ...` | Tag the merge commit on `main`. |
| `has not finished` | Re-run the whole run once the `ci` run has succeeded: `gh run rerun RUN`. Nothing was pushed. |
| `no ci run of commit ... succeeded` | Fix `main`, and release a later commit. |
| `published already` | That version exists, and the release never replaces it. Release the next patch version. |
| `cannot be told` | GitHub or the registry did not answer. Re-run the failed jobs: `gh run rerun RUN --failed`. |
| `sha256sum: WARNING`, in `publish` | The assets differ from the ones that `test` built. Nothing was pushed. Find out why before any re-run. |

A tag that the release refused can be deleted and pushed again with the same name, where the repository's tag rules allow it. Otherwise, use the next patch version.

## A run that fails after a push

Only the job `publish` pushes, in this order: the chart, then the GitHub release.

| `publish` failed | What to do |
|---|---|
| Before its step `Push and sign the chart` | Re-run the failed jobs. Nothing of the version is visible. Attestations and the signature of `SHA256SUMS` from the failed attempt stay in the transparency log, and name assets that were never published. |
| In that step or later, before the GitHub release exists | Release the next patch version. A re-run stops with `published already`. |
| In its step `Verify the release` | The release is published, and one of its checks failed. Find out which before anyone installs it. If an asset or a signature is wrong, say so in the GitHub release's notes, and release the next patch version. |

## What the release does not check

| Limit | When to look again |
|---|---|
| `gate` runs from the workflow of the tag's commit. A tag on a commit whose `release.yaml` lacks it publishes anyway. Only the right to push `v*` tags limits that: a tag ruleset, a repository setting. | A release that must hold against a person who can push tags. |
| GitHub verifies a tag's signature against the keys of the tagger's GitHub account. Any account that may push tags passes the tag check ([RFC0009](../../rfcs/RFC0009.md#releases), Requirement 19) with its own key. | More than one person who may push tags, with only some trusted to release. |
| Some tools come pinned by others, or with the runner. `actions/setup-go` downloads Go. docker, gh, jq, curl and sha256sum come with the runner image. The e2e's Git server installs git with `apk add`, which checks the packages' signatures against the keys of its base image, pinned by digest, but not their versions. | A tool that a job with write access runs, and that these leave unpinned. |
| `renovate.json` asks Renovate to update the SHA-256 of kind, flux and cosign with their version, through `github-release-attachments`; whether that datasource does so was not observed: only Renovate's reading of the pins was run, not its lookups. kubectl and helm are downloaded from elsewhere: Renovate is configured to update their version once the operator approves it on the Dependency Dashboard, and their SHA-256 is copied by hand. CI's `lint` job installs every pin, for amd64 and arm64, so a pull request with a SHA-256 that does not match fails. | The first Renovate pull request of the tools, a SHA-256 that it leaves wrong, or a datasource that reads the checksums of kubectl or helm. |
| The SBOM lists the modules that the binary's build information names, with the Go version as a property, not the standard library as a component. `govulncheck` in CI covers the standard library. | A consumer of the SBOM that must see the standard library. |
