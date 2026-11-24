# Release

What the workflow checks, in order: [RFC0009, Releases](../../rfcs/RFC0009.md#releases).

| Step | What to do |
|---|---|
| 1. Merge | Merge the release's pull request into `main`. [Install](../guide/install.md) names the new version, and [Upgrade](../guide/upgrade.md) has its section. |
| 2. Wait for CI | Wait until the `ci` run of the merge commit on `main` finishes with every job successful: `gh run list --workflow ci.yaml --branch main`. |
| 3. Tag | Sign an annotated tag on that commit and push it: `git tag -s vX.Y.Z COMMIT -m "Tack vX.Y.Z"`, then `git push origin vX.Y.Z`. GitHub must show it as Verified. |
| 4. Approve | If the environment `release` has reviewers, approve the run. |
| 5. Check | Run the commands of [Verify a release](../guide/install.md#verify-a-release) from another machine, after the job `publish` verifies the release. |
| 6. The site | The job `pages` builds the site from the tag and publishes it at tack.tako.id. If only it fails, the release stands: run `gh workflow run pages.yaml` on `main`. |

GitHub shows the tag as Verified when the key belongs to the tagger's GitHub account.

## When the release stops

| The job's log says | What to do |
|---|---|
| `gate.sh: release refused: tag ... is not vX.Y.Z` | Tag again with a name of the form `vX.Y.Z`. |
| `is not annotated`, or `does not verify the signature` | Tag again with `git tag -s` and a key of the tagger's GitHub account. |
| `is not on main`, or `points at ..., not at ...` | Tag the merge commit on `main`. |
| `has not finished` | When the `ci` run has succeeded, re-run the whole run: `gh run rerun RUN`. Nothing was pushed. |
| `no ci run of commit ... succeeded` | Fix `main`, and release a later commit. |
| `published already` | The version exists and is never replaced. Release the next patch version. |
| `cannot be told` | GitHub or the registry did not answer. Run `gh run rerun RUN --failed`. |
| `sha256sum: WARNING`, in `publish` | The assets differ from those `test` built. Nothing was pushed. Find the cause before any re-run. |

A refused tag can be deleted and pushed again under the same name if the repository's tag rules allow it. Otherwise use the next patch version.

## A run that fails after a push

Only the job `publish` pushes. It pushes the chart first, then the GitHub release.

| `publish` failed | What to do |
|---|---|
| Before its step `Push and sign the chart` | Re-run the failed jobs. Nothing of the version is visible. The failed attempt's attestations and `SHA256SUMS` signature stay in the transparency log and name assets that were never published. |
| In that step or later, before the GitHub release exists | Release the next patch version. A re-run stops with `published already`. |
| In its step `Verify the release` | The release is published and one of its checks failed. Find out which before anyone installs it. If an asset or signature is wrong, release the next patch version. |

If an asset or signature is wrong, also say so in the GitHub release's notes.

## What the release does not check

| Limit | When to look again |
|---|---|
| `gate` runs from the tag's commit. A tag on a commit whose `release.yaml` lacks `gate` publishes anyway; only a tag ruleset on `v*` limits that. | A release that must hold against a person who can push tags. |
| GitHub verifies a tag against the tagger's own account keys, so any account that may push tags passes the tag check ([RFC0009](../../rfcs/RFC0009.md#releases), Requirement 19). | More than one person who may push tags, with only some trusted to release. |
| Go comes from `actions/setup-go`; docker, gh, jq, curl and sha256sum from the runner image. The e2e's Git server runs `apk add git` with versions not pinned. | A tool that a job with write access runs and these leave unpinned. |
| `renovate.json` asks Renovate to update the SHA-256 of kind, flux, cosign and vale with their version through `github-release-attachments`. Not observed: only Renovate's reading of the pins ran, not its lookups. | The first Renovate pull request of these tools, or a SHA-256 that it leaves wrong. |
| kubectl and helm come from elsewhere. Renovate updates their version once the operator approves it on the Dependency Dashboard; the SHA-256 is copied by hand. | A datasource that reads the checksums of kubectl or helm. |
| The SBOM lists the modules in the binary's build information and records the Go version as a property, not the standard library as a component. | A consumer of the SBOM that must see the standard library. |

- `apk add git` checks signatures against the digest-pinned base image's keys.
- `govulncheck` in CI covers the standard library.
