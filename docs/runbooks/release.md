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
