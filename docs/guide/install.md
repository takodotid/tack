# Install

Install the `tack` CLI on each operator's machine and the CRD at every site.

## The CLI

Each release on the [releases page](https://github.com/takodotid/tack/releases) has an archive per platform (`tack`, man pages, the CRD), `SHA256SUMS`, the CRD and a Helm chart. To check a download, see [Verify a release](#verify-a-release).

On Linux and macOS:

```sh
VERSION=v1.6.0
OS=linux      # or darwin
ARCH=amd64    # or arm64
NAME=tack-$VERSION-$OS-$ARCH
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/$NAME.tar.gz
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/SHA256SUMS
grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c -    # on macOS: shasum -a 256 -c -
# Check the signature of SHA256SUMS: see Verify a release.
tar -xzf $NAME.tar.gz
sudo install -m 0755 $NAME/tack /usr/local/bin/tack
sudo mkdir -p /usr/local/share/man/man1
sudo cp $NAME/man/man1/*.1 /usr/local/share/man/man1/
```

On Windows the archive is a `.zip` with `tack.exe`. In PowerShell:

```powershell
$VERSION = "v1.6.0"
$NAME = "tack-$VERSION-windows-amd64"    # or arm64
$URL = "https://github.com/takodotid/tack/releases/download/$VERSION"
Invoke-WebRequest "$URL/$NAME.zip" -OutFile "$NAME.zip" -UseBasicParsing
Invoke-WebRequest "$URL/SHA256SUMS" -OutFile SHA256SUMS -UseBasicParsing
$sum = (Get-Content SHA256SUMS | Where-Object { $_.EndsWith("  $NAME.zip") }).Split(" ")[0]
if ((Get-FileHash "$NAME.zip" -Algorithm SHA256).Hash -ne $sum) { throw "$NAME.zip does not match SHA256SUMS" }
Expand-Archive "$NAME.zip" -DestinationPath .
$bin = "$env:LOCALAPPDATA\Programs\tack"
New-Item -ItemType Directory -Force $bin | Out-Null
Copy-Item "$NAME\tack.exe" $bin
```

Add that folder to `Path`, then open a new terminal.

| Difference on Windows | Detail |
|---|---|
| `KUBECONFIG` | Separates files with `;`. See [the Workload file](../workload.md#every-sites-context-in-one-kubeconfig). |
| Man pages | Windows has no `man`. `tack --help` and `tack <command> --help` have the same text. |
| Skill document | In Windows PowerShell 5.1, `tack --skill > SKILL.md` writes UTF-16LE. PowerShell 7 and `cmd.exe` keep UTF-8. |

To build from source: `make build` writes `bin/tack`.

## The CRD at every site

Install the CRD with the Helm chart at `oci://ghcr.io/takodotid/charts/tack`. The chart can also create the operators' roles: [the chart's roles](../permissions.md#the-charts-roles).

Install the release in the ledger namespace (`spec.ledger.namespace` in the Workload file), so `--create-namespace` creates it. Tack never creates it, and without it `tack run` fails when it creates the ledger, before any step.

```sh
for ctx in site-a site-b; do
  helm --kube-context "$ctx" upgrade --install tack oci://ghcr.io/takodotid/charts/tack --version "${VERSION#v}" \
    --namespace tack-system --create-namespace
done
```

| Rule | Why |
|---|---|
| One release per cluster. | The CRD is cluster-scoped. A second release fails with `invalid ownership metadata` and changes nothing. |
| One way per cluster: the chart, or `kubectl apply --server-side` of the CRD. | The chart refuses a CRD that kubectl applied (`invalid ownership metadata`). To hand it to the chart, label it `app.kubernetes.io/managed-by=Helm`, annotate it `meta.helm.sh/release-name=tack` and `meta.helm.sh/release-namespace=tack-system`, then install. |
| Upgrade with the same command. | With `--set` or `-f`, every value not given goes back to the chart's default, unless `--reset-then-reuse-values` is passed. `helm --kube-context "$ctx" -n tack-system get values tack` shows the current values. |
| Before the first run, check at every site [who may write ledgers](../permissions.md#who-may-write-ledgers). | Writing a ledger is privileged, and the chart's roles take no access away. |
| Never set `crds.keep=false` while the site holds a ledger that matters: a run that is not `Succeeded`, or a `Rebuild` follow-up not yet done. | `helm uninstall`, or an upgrade with `crds.install=false`, then deletes the CRD and every ledger. A lost `Rebuild` follow-up can send a planned run to a member that diverged ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 14). To undo it: `helm upgrade --reset-then-reuse-values --set crds.keep=true`. |

`helm uninstall` with `crds.keep=true` (the default) leaves the CRD, every ledger and the namespace. Installing again under the same release name and namespace takes the CRD back. Without Helm, apply the CRD with `kubectl --context "$ctx" apply --server-side -f $NAME/failover.tako.id_sitefailovers.yaml`, then create the ledger namespace.

## What else Tack needs

| Need | Detail |
|---|---|
| A kube context per site | All in one kubeconfig: the file `--kubeconfig` names, else the standard loading rules. One file per cluster: see [the Workload file](../workload.md#every-sites-context-in-one-kubeconfig). [Permissions](../permissions.md) lists what each context needs. |
| Git access | The operator's `git` configuration. The branch must accept a push from those credentials: [Git access](../permissions.md#git-access). |
| A Workload file | The sites, their contexts and the Git repository: [the Workload file](../workload.md). |
| Clocks kept by NTP | Every site's API server and every host that runs `tack` stay within a few seconds of each other. Tack orders runs of different sites by ledger creation time, with a margin of 10 s ([RFC#0003](../../rfcs/RFC0003.md#requirements), Requirement 18). With clocks further apart, an older run can resume after a newer one, or `tack abandon` can mark a run that a process still runs. |

## Shell completion

`tack completion` prints a completion script for bash, zsh, fish or PowerShell. The script completes commands, flags, the values of `--mode` and `--output`, Workload files, and the site names of the `-f` file. It reads only that file and never contacts a cluster.

| Shell | Load it in this shell | Load it in every new shell |
|---|---|---|
| bash | `source <(tack completion bash)` | `tack completion bash \| sudo tee /etc/bash_completion.d/tack >/dev/null` |
| zsh | `source <(tack completion zsh)` | `tack completion zsh > "${fpath[1]}/_tack"`, with `compinit` in `~/.zshrc` |
| fish | `tack completion fish \| source` | `tack completion fish > ~/.config/fish/completions/tack.fish` |
| PowerShell | `tack completion powershell \| Out-String \| Invoke-Expression` | add that line to `$PROFILE` |

## Verify a release

From v1.3.0, each release signs `SHA256SUMS` and the chart with keyless Sigstore, under the release workflow's identity. It also attests every asset with GitHub artifact attestations and has an SBOM per archive. Earlier releases have none of these.

| Asset | Verify with |
|---|---|
| `SHA256SUMS`, and through it every asset | [cosign](https://docs.sigstore.dev/cosign/verifying/verify/) 3: `cosign verify-blob` with `SHA256SUMS.sigstore.json` |
| Any asset, one by one | the GitHub CLI: `gh attestation verify` |
| The chart at `oci://ghcr.io/takodotid/charts/tack` | `cosign verify` |
| `tack-$VERSION-<os>-<arch>.cdx.json` | the CycloneDX SBOM of that archive's binary, listed in `SHA256SUMS` |

In the download folder, before `tar -xzf`:

```sh
id="https://github.com/takodotid/tack/.github/workflows/release.yaml@refs/tags/$VERSION"
issuer=https://token.actions.githubusercontent.com
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/SHA256SUMS.sigstore.json
cosign verify-blob --bundle SHA256SUMS.sigstore.json --certificate-identity "$id" --certificate-oidc-issuer "$issuer" SHA256SUMS
gh attestation verify $NAME.tar.gz --repo takodotid/tack --cert-identity "$id"
cosign verify ghcr.io/takodotid/charts/tack:${VERSION#v} --certificate-identity "$id" --certificate-oidc-issuer "$issuer"
```

`cosign verify-blob` proves the tag's release workflow wrote `SHA256SUMS`; `sha256sum -c` ties the archive to that file.
