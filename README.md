# Tack

Tack moves the primary of a CloudNativePG database from one Kubernetes site to another. It is for setups where Flux deploys both sites from Git. It handles two cases: a planned switchover, and a failover after a site is lost. It changes the database only through CloudNativePG's own API. It keeps Git in step, and it records every step in a ledger, the `SiteFailover` resource.

The operator starts every run. Tack never decides to fail over by itself.

## Why

Moving the primary by hand fails in two ways:

| What goes wrong | What Tack does |
|---|---|
| Flux applies the old values from Git and undoes the switch. | Stops Flux from applying the objects the run changes, and only those. Commits the new values to Git. Lets Flux apply those objects again once Git matches live. |
| The old primary's demotion token must be carried to the new primary's cluster, between two steps. Promoting without the token can fork the data: the two sites then hold different histories. | Waits for the old primary to publish the token. Records the token in the ledger. Promotes the new primary with it. |

[RFC#0001](rfcs/RFC0001.md) gives the full scope.

## How a run works

A run has four phases. Each step is written to the ledger before it starts and after it ends.

A planned run from site a to site b:

```mermaid
sequenceDiagram
  actor op as operator
  participant tack
  participant a as site a, old primary
  participant b as site b, new primary
  participant git as Git
  op->>tack: tack run, then types the workload's name
  Note over tack,b: Hold
  tack->>a: annotate the objects the run changes, reconcile disabled
  tack->>b: annotate the objects the run changes, reconcile disabled
  tack->>a: wait for a fresh Flux reconcile
  tack->>b: wait for a fresh Flux reconcile
  Note over tack,b: Failover, through CloudNativePG's API
  tack->>a: stop the old Pooler, demote pg-a
  a-->>tack: demotion token, recorded in the ledger
  tack->>b: promote pg-b with the token, start the new Pooler
  Note over tack,git: Promote to GitOps
  tack->>git: one commit that makes Git equal live
  tack->>a: wait until Flux applied the commit
  tack->>b: wait until Flux applied the commit
  Note over tack,b: Release
  tack->>a: diff the commit, rendered as Flux does, against live
  tack->>a: remove the annotations, check Flux changed nothing
  tack->>b: diff the commit, rendered as Flux does, against live
  tack->>b: remove the annotations, check Flux changed nothing
  opt Strait configured
    tack->>tack: wait until Strait routes to the new primary
  end
  tack-->>op: Succeeded
```

| Phase | Tack does |
|---|---|
| Hold | Adds the annotation `kustomize.toolkit.fluxcd.io/reconcile: disabled` to the live objects the run changes, and to no others. Then waits for a fresh Flux reconcile. From then on, Flux stops applying those objects. |
| Failover | Changes those objects through CloudNativePG's API, in order, and waits for each result. A planned run does this: stop the old Pooler, demote the old primary, capture its demotion token, promote the new primary with it, start the new Pooler. |
| Promote to GitOps | Writes one commit that makes Git match live. The commit goes on top of the revision that live matched at Hold. Then waits until Flux at each site has applied it. |
| Release | Renders the commit the way Flux does, and compares it with live. Removes the annotations only if the two match. Then checks that Flux changed nothing. If Strait is configured, waits until Strait routes to the new primary. |

The ledger is a `SiteFailover` object at the new primary's site. Its namespace is `tack-system` by default. Its `spec` holds the whole plan and never changes. Its `status` holds every step's result, the demotion token, the held objects and the Git commit. Read it with `tack status`, or with `kubectl --context site-b -n tack-system get sitefailovers` (short name `sfo`).

A run is in one of four states, recorded in the ledger:

```mermaid
stateDiagram-v2
  [*] --> Running: tack run
  Running --> Succeeded: each step ran or is deferred
  Running --> Stopped: unexpected, exit code 3
  Running --> Waiting: Git unreachable, exit code 4
  Stopped --> Running: tack resume, with the decision's flag
  Waiting --> Running: tack resume, once Git is back
  Succeeded --> [*]
```

| Mode | When | What happens |
|---|---|---|
| `--mode planned` (default) | Both sites answer. | The old primary demotes and hands its token to the new one. No write is lost. |
| `--mode site-loss` | The old site is lost. Confirm the loss first: [runbook](docs/runbooks/site-loss.md). | The new site promotes without a token. The old primary must be rebuilt before it can follow the new one again: [runbook](docs/runbooks/rebuild-old-primary.md). |

## Safety

| Property | How |
|---|---|
| The operator starts every run | Only `tack run` starts a run. Tack has no timer, watch or health check that starts one. |
| Plan first | `tack plan` reads every site and Git. It prints every step, patch, wait, Git edit and check. It changes nothing. |
| Typed confirmation | `tack run` and `tack resume` print the plan or the ledger. Then they ask the operator to type the workload's name. Without a terminal they refuse, unless `--yes` is passed for a run that a human approved. |
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

## Status

The latest release is v1.4.0. The API is `failover.tako.id/v1`, for both the `SiteFailover` ledger and the Workload file. A change that would break a user comes as a new API version. `failover.tako.id/v1alpha1` was the API of v0.1.x. It is deprecated but still served: see [Upgrading from v0.1.x](#upgrading-from-v01x).

## Install

Each release on the [releases page](https://github.com/takodotid/tack/releases) has an archive per platform, `SHA256SUMS`, the CRD, and a Helm chart. The chart installs the CRD and, if asked, roles. Each archive holds `tack`, its man pages and the CRD. The chart is also published at `oci://ghcr.io/takodotid/charts/tack`. From v1.3.0, each release also has an SBOM per archive, and signatures and attestations for every asset: see [Verify a release](#verify-a-release).

```sh
VERSION=v1.4.0
OS=linux      # or darwin
ARCH=amd64    # or arm64
NAME=tack-$VERSION-$OS-$ARCH
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/$NAME.tar.gz
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/SHA256SUMS
grep " $NAME.tar.gz\$" SHA256SUMS | sha256sum -c -    # on macOS: shasum -a 256 -c -
# Then check the signature of SHA256SUMS: see Verify a release, below.
tar -xzf $NAME.tar.gz
sudo install -m 0755 $NAME/tack /usr/local/bin/tack
sudo mkdir -p /usr/local/share/man/man1
sudo cp $NAME/man/man1/*.1 /usr/local/share/man/man1/
```

On Windows the archive is a `.zip` with `tack.exe`. Install it in PowerShell:

```powershell
$VERSION = "v1.4.0"
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

Add that folder to `Path` once, then open a new terminal.

| Difference on Windows | Detail |
|---|---|
| `KUBECONFIG` | Separates files with `;`, not `:`. See [docs/workload.md](docs/workload.md#every-sites-context-in-one-kubeconfig). |
| Man pages | Windows has no `man`. `tack --help` and `tack <command> --help` hold the same text. |
| Skill document | In Windows PowerShell 5.1, `tack --skill > SKILL.md` writes UTF-16LE, not the UTF-8 that `tack` prints. PowerShell 7 and `cmd.exe` keep UTF-8. |

To build from source instead: `make build` writes `bin/tack`.

At every site, install the CRD with the chart. The chart can also create the operators' roles. That is off by default: see [The chart's roles](docs/operations.md#the-charts-roles). Install the release in the ledger namespace, `spec.ledger.namespace` in the Workload file, so that `--create-namespace` creates that namespace. Tack never creates the ledger namespace. Without it, `tack run` fails when it creates the ledger, before any step.

```sh
for ctx in site-a site-b; do
  helm --kube-context "$ctx" upgrade --install tack oci://ghcr.io/takodotid/charts/tack --version "${VERSION#v}" \
    --namespace tack-system --create-namespace
done
```

| Rule | Why |
|---|---|
| One release per cluster. | The CRD is cluster-scoped. A second release, under another name or in another namespace, fails with `invalid ownership metadata` and changes nothing. |
| One way per cluster: the chart, or `kubectl apply --server-side` of the CRD. | The chart refuses a CRD that kubectl applied, with `invalid ownership metadata`. To hand such a CRD to the chart, label it `app.kubernetes.io/managed-by=Helm`, annotate it `meta.helm.sh/release-name=tack` and `meta.helm.sh/release-namespace=tack-system`, then install. |
| Upgrade with the same command. | `helm upgrade` without values reuses the release's last values, including `crds.keep` and `crds.install`. `helm --kube-context "$ctx" -n tack-system get values tack` shows them. With `--set` or `-f`, and without `--reset-then-reuse-values`, each value not given goes back to the chart's default. `--reuse-values` also drops the defaults that a newer chart adds. After each install and upgrade, the chart's notes print `crds.install` and `crds.keep`. |
| Before the first run, check at every site who may write ledgers: [docs/operations.md](docs/operations.md#who-may-write-ledgers). The chart prints the link after each install and upgrade. | Writing a ledger is privileged, because `tack resume` acts on what a ledger holds. The chart's roles only grant access. They take no access away from a GitOps controller or any other subject. |
| Never set `crds.keep=false` while the site holds a ledger that matters: a run that is not `Succeeded`, or a `Rebuild` follow-up not yet done. | With `crds.keep=false`, `helm uninstall` deletes the CRD, and every ledger with it. So does an upgrade with `crds.install=false`. If a `Rebuild` follow-up is lost, a planned run can go to a member that diverged ([RFC#0003](rfcs/RFC0003.md#requirements), Requirement 14). The upgrade that sets `crds.keep=false` removes the keep policy from the CRD at once. The value then stays across upgrades without values, and the notes warn while it is `false`. Set it back with `helm upgrade --reset-then-reuse-values --set crds.keep=true`. |

`helm uninstall` with `crds.keep=true`, the default, leaves the CRD, every ledger and the namespace in place. Installing again under the same release name and namespace takes the CRD back into the release. Without Helm, apply the CRD instead with `kubectl --context "$ctx" apply --server-side -f $NAME/failover.tako.id_sitefailovers.yaml`, then create the ledger namespace.

Tack also needs:

| Need | Detail |
|---|---|
| A kube context per site | All in one kubeconfig: the file `--kubeconfig` names, or else the standard loading rules. To combine one file per cluster, see [docs/workload.md](docs/workload.md#every-sites-context-in-one-kubeconfig). [docs/operations.md](docs/operations.md#permissions) lists the permissions. |
| Git access | Through the operator's `git` configuration, such as an SSH agent or a credential helper. Tack runs `git` with `GIT_TERMINAL_PROMPT=0` and stores no credential. The branch must accept a push from those credentials. |
| A Workload file | The sites, their contexts, the Git repository, and where each field lives in it: [docs/workload.md](docs/workload.md). |
| Clocks kept by NTP | Every site's API server and every host that runs `tack` stay within a few seconds of each other. Tack orders the runs of different sites by each ledger's creation time, which its site's API server sets. It allows a margin of 10 s ([RFC#0003](rfcs/RFC0003.md#requirements), Requirement 18). A run's Lease is timed by the host that renews it, and holds for 10 s past its duration (Requirement 28). With clocks further apart, an older run can resume after a newer one, or `tack abandon` can mark a run that a process still runs. |

### Shell completion

`tack completion` prints a completion script for bash, zsh, fish or PowerShell. The script completes commands, flags, the values of `--mode` and `--output`, Workload files, and the site names of the file given with `-f`. It reads only that file, and never contacts a cluster.

| Shell | Load it in this shell | Load it in every new shell |
|---|---|---|
| bash | `source <(tack completion bash)` | `tack completion bash \| sudo tee /etc/bash_completion.d/tack >/dev/null` |
| zsh | `source <(tack completion zsh)` | `tack completion zsh > "${fpath[1]}/_tack"`, with `compinit` in `~/.zshrc` |
| fish | `tack completion fish \| source` | `tack completion fish > ~/.config/fish/completions/tack.fish` |
| PowerShell | `tack completion powershell \| Out-String \| Invoke-Expression` | add that line to `$PROFILE` |

### Verify a release

From v1.3.0, each release signs `SHA256SUMS` and the chart with Sigstore. The signing is keyless, under the identity of the release workflow: no signing key exists. Each release also attests every asset with GitHub artifact attestations. `make dist` at a release's tag, in a clean checkout with the Go version of `go.mod`, builds the same archives. Releases v1.2.1 and older carry no signature.

| Asset | Verify with |
|---|---|
| `SHA256SUMS`, and through it every asset | [cosign](https://docs.sigstore.dev/cosign/verifying/verify/) 3: `cosign verify-blob` with `SHA256SUMS.sigstore.json` |
| Any asset, one by one | the GitHub CLI: `gh attestation verify` |
| The chart at `oci://ghcr.io/takodotid/charts/tack` | `cosign verify` |
| `tack-$VERSION-<os>-<arch>.cdx.json` | the CycloneDX SBOM of that archive's binary, listed in `SHA256SUMS` |

In the folder of the download above, before `tar -xzf`:

```sh
id="https://github.com/takodotid/tack/.github/workflows/release.yaml@refs/tags/$VERSION"
issuer=https://token.actions.githubusercontent.com
curl -fsSLO https://github.com/takodotid/tack/releases/download/$VERSION/SHA256SUMS.sigstore.json
cosign verify-blob --bundle SHA256SUMS.sigstore.json --certificate-identity "$id" --certificate-oidc-issuer "$issuer" SHA256SUMS
gh attestation verify $NAME.tar.gz --repo takodotid/tack --cert-identity "$id"
cosign verify ghcr.io/takodotid/charts/tack:${VERSION#v} --certificate-identity "$id" --certificate-oidc-issuer "$issuer"
```

`cosign verify-blob` proves that the tag's release workflow wrote `SHA256SUMS`. The `sha256sum -c` above then ties the archive to that file.

## Upgrading within v1

Upgrade in this order:

1. **Finish or stop every run.** For each workload, `tack status -f workload.yaml` lists every run as `Succeeded`, or as `Stopped` or `Waiting` with no `tack` process running it. A run that resumes after the upgrade runs its remaining steps by the new CLI's rules.
2. **The CRD at every site,** the way it was installed: the `helm upgrade --install` above, or `kubectl apply --server-side` of the new CRD. Do this before any operator runs the new CLI against the site. A CRD older than the CLI drops the fields it does not know, without an error. At such a site, `tack run` refuses to create a ledger, and status writes lose those fields.
3. **Every operator's CLI together,** and every script that runs `tack`. When a CLI writes a ledger, it drops the status fields it does not know. From v1.3.0, each write records the CLI's version in `status.writtenBy`. A CLI of an older release then refuses to resume or abandon that run, and names the version to use ([RFC#0003](rfcs/RFC0003.md#requirements), Requirement 24). A build whose version names no release refuses too. An example is the version `dev`, from `go build -buildvcs=false` or from `go install` of v1.4.0 or older. `go install` of a later release's tag takes that tag's version. A CLI older than v1.3.0 does not check.

Each release also needs:

| From | To | CRD | Also before the new CLI runs |
|---|---|---|---|
| v1.6.x | the next release (its version is set when it is tagged) | new optional fields: `spec.acceptBehind`, `status.fork`, `status.writes`, `status.steps[].earlier`, `wait.stall` of a step, `status.followUps[].snapshot` and `.archive`; in the Workload snapshot, `cnpg.snapshot`, `timeouts.snapshot` and `timeouts.archiving`; step types `Create` and `Record` | Apply the CRD first. A CRD of v1.6.x drops `spec.acceptBehind`, and `tack run --accept-behind` refuses to create a ledger there. Grant `get` on `pods` and `pods/proxy` in each member's namespace (row 18 of [docs/operations.md](docs/operations.md#permissions); the chart's objects Role has it where `rbac.objects` names `clusters`). The plan uses it to read each site's WAL position, and each replica's replay lag from its metrics. Without it, the plan only warns. To have a site-loss plan say up to which commit the new primary's data reaches, add the opt-in replay-time query to each Cluster ([docs/workload.md](docs/workload.md#replay-time)). A site-loss plan refuses a target that is behind another replica that answers, unless `--accept-behind` is passed ([RFC#0004](rfcs/RFC0004.md#requirements), Requirement 14). A site-loss run records the fork point and the old primary's WAL archive. With `cnpg.snapshot`, it also takes a snapshot of the old primary, and needs `get` and `create` on `backups` (row 19). A ledger that v1.6.x wrote resumes with the steps it planned. A planned switchover is refused when two sites archive WAL to the same destination; a site-loss plan warns instead ([docs/workload.md](docs/workload.md#wal-archives)). For the plan to check this, grant `get` on the barman-cloud `objectstores` that the Clusters name (row 20). After each promotion, the run waits up to `timeouts.archiving` for the new primary to archive WAL. If that time runs out, the run warns and records the follow-up `CheckArchiving`. Each run records when writes stopped and came back (`status.writes`). Each step keeps its earlier attempts (`status.steps[].earlier`). In a planned run, the wait for the promotion stops once replay has stood still for 2 minutes short of the token. |
| v1.5.x | v1.6.0 | no new field; the column TakenOverBy in `kubectl get sfo`, last | With a CRD of v1.5.x, everything works except that column. Tack obtains each site's credentials before its checks and before each step of a run. It runs one credential plugin at a time, with no time limit; Ctrl-C or SIGTERM ends the wait. If a site's credentials cannot be obtained, the command stops, or the run stops before that site's step. Such a site is never counted as lost ([RFC#0007](rfcs/RFC0007.md#requirements), Requirement 12). A planned switchover is refused when a site's Cluster does not name the new primary in `spec.externalClusters`. In that case a site-loss plan warns, and the run stops at that site's step ([RFC#0004](rfcs/RFC0004.md#requirements), Requirement 12). `tack prune` deletes a run whose follow-up `Rebuild` is done. For that, it needs `get` on the workload's objects (row 7 of [docs/operations.md](docs/operations.md#permissions)). Without it, prune keeps that run. Prune keeps more than `--keep` runs when the clocks of two sites cannot order them. |
| v1.4.x | v1.5.0 | unchanged | Grant `get`, `create` and `update` on `leases` in the ledger namespace (row 17 of [docs/operations.md](docs/operations.md#permissions); the chart's ledger Role has it with `rbac.create`). Without it, a run holds no Lease and says so once. `tack abandon` and `tack run --take-over` now refuse a run that a process may still run. With `--force`, they go on. A build whose version names no release, such as `dev`, refuses ledgers that a release wrote. Quote a `kubeContext` that YAML reads as other text, such as `on`: v1.5.0 refuses it unquoted. |
| v1.3.x | v1.4.0 | no new field; the column Abandoned in `kubectl get sfo`, after Reason: Phase and Age move one place right | With a CRD of v1.3.x, everything works except that column. `tack prune` needs `delete` on `sitefailovers` in the ledger namespace (row 16 of [docs/operations.md](docs/operations.md#permissions)). The chart grants it only with `rbac.prune`, so grant it only to the operators who prune. Once v1.4.0 writes a ledger, `tack resume` and `tack abandon` of v1.3.x refuse that run and name v1.4.0. |
| v1.3.0 | v1.3.1 | unchanged | When a site gives no answer, a script that runs `tack status -f` gets exit code 1, along with the runs that were read. The list is then incomplete ([docs/operations.md](docs/operations.md#reading-a-run)). |
| v1.2.x | v1.3.0 | new optional fields: `spec.takeOvers`, `status.abandoned`, `status.git.changedRevision`, `status.history`, `status.historyDropped`, `status.writtenBy`; the column Reason in `kubectl get sfo` | Grant `create` on `events` in the ledger namespace, for the ledger's Events. Without it, a run goes on without them ([docs/operations.md](docs/operations.md#permissions)). In each Workload file, quote every site name that YAML reads as a boolean or a number, such as `n`, `on` or `010`. v1.3.0 refuses it unquoted ([docs/workload.md](docs/workload.md#specsites)). A run planned before the upgrade recorded the decoded name, such as `false`. Its flags, such as `--fence`, take that name. |
| v1.2.0 | v1.2.1 | unchanged | nothing |
| v1.0.x or v1.1.x | v1.2.x | new optional fields: `spec.takeOver`, `spec.fingerprints`, `status.fingerprints`, `status.supersededBy`, `status.git.releaseRevision` | Grant `get` on the namespace `kube-system` at every site. The plan reads its UID to identify the site's cluster, and fails with `forbidden` without it. Tack refuses an `http` or `https` Git URL that holds a user, such as `https://user@git.example.com/gitops.git`. Move the user into git's configuration (`credential.<url>.username`). `ssh://git@git.example.com/gitops.git` and `git@git.example.com:gitops.git` still work. |

| A ledger written by | Under v1.6.0 |
|---|---|
| v1.5.x | As for v1.3.x, below. Also, `tack prune` deletes a run with a follow-up `Rebuild` once that site's member was created again, 10 s or more after the run's ledger. v1.5.x kept such a run until someone deleted it by hand. |
| v1.4.x | As for v1.3.x, below. Such a run has no Lease, so `tack abandon` and `tack run --take-over` judge it by the ledger. They refuse a `Running` run whose ledger was written within its `Running` step's timeout plus 5 minutes, unless `--force` is passed. |
| v1.3.x | `tack status` lists and shows it, with its history. `tack resume` continues it and keeps that history. `tack abandon` sees its mark. `tack prune` deletes it when its rules allow. |
| v1.2.x | `tack resume` continues it. |
| v1.0.x or v1.1.x | `tack resume` continues it. Each site's cluster is recorded in `status.fingerprints` on its first read, and checked after. |

## Upgrading from v0.1.x

1. At every site, upgrade the CRD the way it was installed: the `helm upgrade --install` above, or `kubectl apply --server-side` of the new CRD. Do this before running tack v1.0.0 against the site. tack v1.0.0 reads ledgers as `v1`. Against a site still on the v0.1.x CRD, it stops and says to upgrade the CRD.
2. Install tack v1.0.0 or later.
3. In each Workload file, set `apiVersion: failover.tako.id/v1`. Nothing else changes. Tack still reads `failover.tako.id/v1alpha1`, with a warning.

| After the upgrade | Behaviour |
|---|---|
| Ledgers written by v0.1.x | `tack status` reads them and `tack resume` continues them. The API server serves them as `v1` and stores each as `v1` the next time it is written. |
| tack v0.1.x | Still reads and writes ledgers, through `v1alpha1`, until every operator has upgraded. |
| `kubectl` | Warns on each `v1alpha1` request. Until its discovery cache refreshes, `kubectl get sfo` may still ask for `v1alpha1`. |
| Downgrading the CRD to v0.1.x | Refused by the API server: `v1` is now in the CRD's `status.storedVersions`. |

## Walkthrough

With the Workload file of [docs/workload.md](docs/workload.md), a planned switchover from site a to site b:

```sh
# Read every site and Git, print every step, change nothing.
tack plan -f workload.yaml --to b

# Print the plan again, ask for the workload's name, then run it.
tack run -f workload.yaml --to b

# List the workload's runs at every site, or show one run step by step.
tack status -f workload.yaml
tack status pg-main-20261027-101500 -f workload.yaml

# Continue a run that stopped (exit code 3) or waits (exit code 4).
tack resume pg-main-20261027-101500 -f workload.yaml

# Delete the ledgers of old runs that succeeded, keeping the 20 newest of them.
tack prune -f workload.yaml --keep 20
```

After a site loss, add `--mode site-loss` to `plan` and `run`. [docs/operations.md](docs/operations.md) covers stopped runs and break-glass.

An excerpt of `tack plan`:

```text
Plan for workload pg-main: Planned, site a to site b
Ledger: SiteFailover tack-system/pg-main-20261027-101500 at site b

Sites:
  a  Primary  declares a  healthy  Cluster db/pg-a: phase "Cluster in healthy state", declares pg-a primary, pod pg-a-1; Pooler pg-a-pooler-rw has 2 instances
  b  Replica  declares a  healthy  Cluster db/pg-b: phase "Cluster in healthy state", declares pg-a primary, pod pg-b-1; Pooler pg-b-pooler-rw has 0 instances
...
Failover
...
   9. demote-a  site a
      demote Cluster pg-a: declare pg-b primary and drop any old token
        patch Cluster db/pg-a (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":null}}}
  10. wait-demoted-a  site a
      wait until Cluster pg-a has demoted and published its demotion token
        wait up to 15m0s on Cluster db/pg-a until:
          {.status.demotionToken} is not empty
          {.status.switchReplicaClusterStatus.inProgress} NotEquals "true"
  11. capture-token  site a
      record the demotion token of Cluster pg-a in the ledger
        record {.status.demotionToken} of Cluster db/pg-a in the ledger as $(demotionToken)
  12. promote-b  site b
      promote Cluster pg-b with the demotion token, in one patch
        patch Cluster db/pg-b (merge): {"spec":{"replica":{"primary":"pg-b","promotionToken":"$(demotionToken)"}}}
...
PromoteToGitOps
  18. git-commit  Git
      commit the live state to ssh://***@git.example.com/gitops.git branch main
        clusters/site-a/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site a, Cluster db/pg-a spec.replica.primary)
        clusters/site-a/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site a, Cluster db/pg-a spec.replica.promotionToken)
        clusters/site-a/db/pooler.yaml, document 0: spec.instances = 0  (site a, Pooler db/pg-a-pooler-rw spec.instances)
        clusters/site-b/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site b, Cluster db/pg-b spec.replica.primary)
        clusters/site-b/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site b, Cluster db/pg-b spec.replica.promotionToken)
        clusters/site-b/db/pooler-instances.yaml: spec.instances = 2  (site b, Pooler db/pg-b-pooler-rw spec.instances)
        commit message: tack: pg-main primary a to b (planned)
...

Nothing was changed. To run this plan: tack run -f workload.yaml --to b --from a --mode planned
```

`tack --help` lists the exit codes. `tack <command> --help` and `man tack` document every command and flag.

## Quick start: the end-to-end test

The end-to-end test builds two kind clusters with Flux, CloudNativePG, Strait and a local Git server, then switches the primary between them.

| Needs | Version |
|---|---|
| Linux with Docker | Docker 29 or later |
| Go | as in `go.mod` |
| kind | v0.33.0 |
| kubectl | v1.37.1 |
| flux CLI | v2.10.0 exactly |
| helm | v4.3.1 |
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

Everything the test creates is named `tack-e2e*`. [test/e2e/env/README.md](test/e2e/env/README.md) describes the environment.

## Documentation

| Doc | Content |
|---|---|
| [docs/workload.md](docs/workload.md) | every field of the Workload file, with a complete example |
| [docs/operations.md](docs/operations.md) | reading a run, the decision flags, how long writes stay down, what fencing does, break-glass, permissions |
| [docs/wal-rpo-rto.md](docs/wal-rpo-rto.md) | what each mode can lose and how long writes stay down, choosing `--to`, WAL archiving, the failover-time budget |
| [docs/runbooks/site-loss.md](docs/runbooks/site-loss.md) | confirming a site loss, and fencing the old primary and its site's applications before a site-loss run |
| [docs/runbooks/recover-old-primary-writes.md](docs/runbooks/recover-old-primary-writes.md) | finding the old primary's writes after the fork point, and handing them to their owners |
| [docs/runbooks/rebuild-old-primary.md](docs/runbooks/rebuild-old-primary.md) | rebuilding an old primary after a site loss |
| [docs/runbooks/measure-failover-time.md](docs/runbooks/measure-failover-time.md) | measuring in staging how long writes stay down, and that none is lost |
| [docs/runbooks/stop-reasons.md](docs/runbooks/stop-reasons.md) | the way on for each reason a run stops or waits |
| [docs/runbooks/no-primary-after-demotion.md](docs/runbooks/no-primary-after-demotion.md) | a planned run that stopped between its demotion and its promotion |
| [docs/runbooks/abandon-run.md](docs/runbooks/abandon-run.md) | abandoning a run, and removing a hold left behind |
| [docs/runbooks/release.md](docs/runbooks/release.md) | publishing a release, and what to do when it stops |
| [SECURITY.md](SECURITY.md) | reporting a vulnerability, supported versions |
| [rfcs/](rfcs/) | the design, one subject per RFC |
| `tack --help`, `tack <command> --help`, `man tack` | every command, flag and exit code |
| `tack --skill` | the guide for AI agents |

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
- Releases are signed, and you can verify every release asset (see [Verify a release](#verify-a-release)).

**Why we trust it in production:** we run Tack ourselves, for our own databases. A failover tool that loses data or leaves two primaries would hurt us first.

If you find a problem, open an issue. Saying that it looks AI-written is fine, as long as you say what's wrong.

## Contributing

- [AGENTS.md](AGENTS.md) holds the rules for every change, by a human or an agent.
- The design lives in [rfcs/](rfcs/). A design change starts as an RFC. The RFC is `Draft` while under discussion, and becomes `Active` once the maintainers approve it. The code conforms to the Active RFCs. If the code and an RFC disagree, the RFC is amended first.
- Before a pull request: `make verify test lint`. For a change to how a run behaves, also `make e2e`.
- Commits follow conventional commits: `type(scope): subject`.

## License

Apache-2.0. See [LICENSE](LICENSE).
