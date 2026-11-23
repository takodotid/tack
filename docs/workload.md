# The Workload file

A Workload file describes one workload: its sites, the Git repository Flux applies, and where Tack records runs. `tack plan` and `tack run` read it with `-f`. `tack status` and `tack resume` read it to find the sites. `tack resume` runs with the copy in the run's ledger, not with the file.

## Example

Two sites, `a` and `b`, each with one CloudNativePG `Cluster` and one read-write `Pooler` in namespace `db`. Site a is the primary.

```yaml
apiVersion: failover.tako.id/v1
kind: Workload
metadata:
  name: pg-main
spec:
  driver: cnpg
  sites:
  - name: a
    kubeContext: site-a
    cnpg:
      cluster: {namespace: db, name: pg-a}
      pooler: {namespace: db, name: pg-a-pooler-rw, instances: 2}
  - name: b
    kubeContext: site-b
    cnpg:
      cluster: {namespace: db, name: pg-b}
      pooler: {namespace: db, name: pg-b-pooler-rw, instances: 2}
  git:
    url: ssh://git@git.example.com/gitops.git
    branch: main
    author: {name: Tack, email: tack@example.com}
    fields:
    # Site b sets the Pooler's instances in a Kustomize patch, so two
    # manifests define that Pooler. The mapping names the patch.
    - site: b
      object: {kind: Pooler, namespace: db, name: pg-b-pooler-rw}
      field: spec.instances
      file: clusters/site-b/db/pooler-instances.yaml
      path: spec.instances
    # Every other field is written into the manifest that defines its object.
    find:
      paths:
      - {site: a, path: clusters/site-a}
      - {site: b, path: clusters/site-b}
  routing:
    strait:
      services:
      - {site: a, namespace: db, name: postgres}
      - {site: b, namespace: db, name: postgres}
      members:
      - {site: a, member: pg-a}
      - {site: b, member: pg-b}
  timeouts: {demotion: 15m, promotion: 15m}
```

The Git repository:

```text
clusters/site-a/db/kustomization.yaml
clusters/site-a/db/cluster.yaml             Cluster pg-a
clusters/site-a/db/pooler.yaml              Pooler pg-a-pooler-rw
clusters/site-b/db/kustomization.yaml       patches: pooler-instances.yaml
clusters/site-b/db/cluster.yaml             Cluster pg-b
clusters/site-b/db/pooler.yaml              Pooler pg-b-pooler-rw
clusters/site-b/db/pooler-instances.yaml    a patch of Pooler pg-b-pooler-rw that sets spec.instances
```

For a switch to site b, `tack plan` turns this into these Git changes, and removes `spec.replica.promotionToken` from both Clusters:

```text
clusters/site-a/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site a, Cluster db/pg-a spec.replica.primary)
clusters/site-a/db/pooler.yaml, document 0: spec.instances = 0  (site a, Pooler db/pg-a-pooler-rw spec.instances)
clusters/site-b/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site b, Cluster db/pg-b spec.replica.primary)
clusters/site-b/db/pooler-instances.yaml: spec.instances = 2  (site b, Pooler db/pg-b-pooler-rw spec.instances)
```

Without the mapping, the plan stops: `several manifests define the object: clusters/site-b/db/pooler-instances.yaml document 0, clusters/site-b/db/pooler.yaml document 0`.

## Fields

Durations are Go durations, such as `90s`, `15m` or `1h`. Unknown fields are an error.

### Top level

| Field | Required | Default | Meaning |
|---|---|---|---|
| `apiVersion` | yes | | `failover.tako.id/v1`. `v1alpha1` is read with a warning ([Upgrade](guide/upgrade.md)). |
| `kind` | yes | | `Workload` |
| `metadata.name` | yes | | Lowercase letters, digits and `-`. A run is named `<name>-<UTC yyyymmdd-hhmmss>` and labelled `failover.tako.id/workload: <name>`. The operator types the name to confirm a run. |
| `spec.driver` | yes | | The built-in driver for the workload's API. The only driver is `cnpg`. |
| `spec.sites` | yes | | Every site, at least two. |
| `spec.git` | yes | | The Git repository Flux applies at every site. |
| `spec.ledger.namespace` | no | `tack-system` | The namespace of the `SiteFailover` objects. It must exist at every site. A run's ledger is created at its `--to` site. |
| `spec.routing` | no | | The routing check. Without it, a run has none. |
| `spec.timeouts` | no | | Time limits on the run's waits. |

### `spec.sites[]`

Quote a site name, or a context in `site:`, when YAML would read it as a boolean or a number, such as `"n"`, `"on"` or `"010"`. Tack refuses it unquoted.

| Field | Required | Default | Meaning |
|---|---|---|---|
| `name` | yes | | The name used in `--to`, `--from`, `--fence` and the ledger. Unique. Lowercase letters, digits and `-`, starting and ending with a letter or digit, at most 63 characters. |
| `kubeContext` | yes | | The kubeconfig context for this site. A missing context is an error: Tack never falls back to the current context. [One kubeconfig](#every-sites-context-in-one-kubeconfig) holds every site's context. |
| `cnpg.cluster.namespace`, `cnpg.cluster.name` | yes | | The site's CloudNativePG `Cluster`. Each site's Cluster needs its own name. |
| `cnpg.pooler.namespace`, `cnpg.pooler.name` | no | | The site's read-write `Pooler`. Tack also waits on the Deployment of the same name. Without it, Tack moves no Pooler at this site. Never name it `<cluster>-rw`, `<cluster>-ro` or `<cluster>-r`: the Cluster owns Services with those names, and CloudNativePG then never reconciles the Pooler. |
| `cnpg.pooler.instances` | with a pooler | | The Pooler's instances while this site is primary, at least 1. While another site is primary, Tack sets it to 0. |
| `cnpg.snapshot` | no | `false` | Take a cold snapshot of this Cluster's volumes when a site loss leaves it the old primary: once the site answers again, before anyone rebuilds the Cluster. It is a CloudNativePG `Backup` with `method: volumeSnapshot`, so the Cluster needs `spec.backup.volumeSnapshot` and the site needs VolumeSnapshot support. A failed Backup stops the run. Set it on every site, since any site can be the old primary. See [recover-old-primary-writes.md](runbooks/recover-old-primary-writes.md). |

`tack plan` checks the live objects too. Each `Cluster` must set `.spec.replica.primary` and not `.spec.replica.enabled`. A Flux Kustomization must apply each `Cluster` and `Pooler`. [RFC#0004](../rfcs/RFC0004.md) and [RFC#0005](../rfcs/RFC0005.md) list every check.

### Every site's context in one kubeconfig

Tack reads every site's `kubeContext` from one kubeconfig: the file `--kubeconfig` names, or else the one the standard loading rules find. With one file per cluster, list them in `KUBECONFIG` (separator `:`, on Windows `;`):

```sh
KUBECONFIG=site-a.kubeconfig:site-b.kubeconfig tack plan -f workload.yaml --to b
```

- `--kubeconfig` takes one file, and then Tack ignores `KUBECONFIG`.
- Context, cluster and user names must differ between the files. When a name repeats, the first file's entry wins, and a site's context could reach another site's cluster or use its credentials.
- To keep one file, run `kubectl config view --flatten > sites.kubeconfig` with `KUBECONFIG` set as above, and pass it with `--kubeconfig`. It holds every site's credentials: protect it like the originals.

### `spec.git`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `url` | yes | | The repository, as `git` takes it: SSH, HTTPS or a path. Access comes from the operator's `git` configuration ([Git access](permissions.md#git-access)). Tack refuses a URL with a password, and an HTTP or HTTPS URL with a user: set the user with `git config credential.<url>.username`. |
| `branch` | yes | | The branch Flux applies. |
| `author.name`, `author.email` | no | git's `user.name` and `user.email` | The author and committer of Tack's commit. Without `author`, git must have an identity configured. |
| `fields[]` | no | | Mappings: where a field lives in the repository. A mapping wins over `find`. |
| `find` | no | | Lets Tack find the manifest that defines an object, for every field without a mapping. |

Every field a run changes needs a mapping or `find`, or the plan fails. The exception is the removal of a used CloudNativePG promotion token: when neither covers it, the plan leaves that change out and says so. [RFC#0004](../rfcs/RFC0004.md) Requirement 8 lists the fields the `cnpg` driver changes. `tack plan` prints each with its file and path.

### `spec.git.fields[]`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `site` | yes | | The site of the object. |
| `object.kind`, `object.name` | yes | | The live object, such as `Pooler` `pg-b-pooler-rw`. |
| `object.namespace` | no | any namespace | The live object's namespace. |
| `field` | yes | | The field's path in the live object, such as `spec.instances`. |
| `file` | yes | | The file, relative to the repository root. |
| `path` | yes | | The field's path in the file. `a.b.c` is key `c` under `b` under `a`. `a[2].b` is key `b` of the third item of list `a`. Keys that contain dots cannot be addressed. |
| `document` | no | | The YAML document to edit, counting from 0. Required when the file holds several documents. |

A mapping matches a change when site, kind, name and field match, and the namespace too if the mapping sets one. Two mappings that match the same change fail the plan. When Git answers at plan time, the file and the parent of `path` must exist in it.

### `spec.git.find`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `paths[].site` | yes | | A site of the workload. |
| `paths[].path` | yes | | A directory of the repository that holds this site's manifests. A site may have several. |

`find: {}`, or a site without a `paths` entry, searches the whole repository for that site. Tack reads the `.yaml` and `.yml` files at the branch's head and looks for the one YAML document whose API group, kind, name and namespace match the object. A document without `metadata.namespace` matches any namespace. No match, or several, fails the plan, which names the several. A mapping, or narrower `paths`, fixes that.

The edit rules (what Tack changes in a file, and what it refuses to edit) are in [RFC#0006](../rfcs/RFC0006.md).

### `spec.routing.strait`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `services[]` | yes | | The Strait `SiteService` to read, `{site, namespace, name}`, one per site that runs Strait. At least one. |
| `members[]` | yes | | For every site, `{site, member}`: the site's member in the `SiteService`. |

A run then ends with one `Routing` step per service. Each waits until Strait at that site routes only to the `--to` site's member ([RFC#0008](../rfcs/RFC0008.md)).

### `spec.timeouts`

| Field | Default | Bounds |
|---|---|---|
| `demotion` | `10m` | the wait for the old primary to demote and publish its token |
| `promotion` | `10m` | the wait for the new primary to promote, every Pooler wait, and the final check that the new primary is healthy |
| `flux` | `5m` | each Flux reconcile |
| `routing` | `2m` | each Strait routing check |
| `snapshot` | `30m` | the wait for the old primary's snapshot (`cnpg.snapshot`) |
| `archiving` | `5m` | the wait, after the promotion, for the new primary to archive WAL of its new timeline. On a timeout the run warns, records the follow-up `CheckArchiving`, and goes on |

Any other wait that times out stops the run with reason `StepFailed`.

## WAL archives

Give each site's Cluster a WAL archive of its own from the start: its own `destinationPath`, or its own `serverName` under a shared path. A replica Cluster archives too (`archive_mode = always` in CloudNativePG v1.30.1), so every site writes WAL all the time. Two sites with one destination write the same file names, and CloudNativePG refuses to start a Cluster on an archive that is not empty (`Expected empty archive`).

| Tack | Does |
|---|---|
| `tack plan` | Reads each site's destination: `spec.backup.barmanObjectStore`, or the `destinationPath` of the barman-cloud plugin's `ObjectStore`. Refuses a planned switchover when two sites that answer share one. For a site loss it only warns, since the fix is in Git, which may have gone with the lost site. Also warns when the new primary reports `ContinuousArchiving` `False`. |
| `verify-archiving-<to>`, after the promotion | Waits up to `timeouts.archiving` until `ContinuousArchiving` is `True` and the new primary's last archived file is of its new timeline. A Cluster without an archive passes at once. On a timeout the run warns and records `CheckArchiving`: writes are back by then, and the operator fixes the archive. Until archiving works, no point-in-time recovery reaches past the promotion. |
| after a site loss | Records where the old primary archived, on its `Rebuild` follow-up. The old archive is kept, and the rebuilt Cluster gets a new destination ([rebuild-old-primary.md](runbooks/rebuild-old-primary.md)). |

## Replay time

Opt-in. With it, a site-loss plan says when the last commit the new primary replayed was. Without it, the plan prints each replica's replay lag only, which is 0 once a replica has replayed all it received: the usual state after a site loss.

At each site, make two changes in Git:

1. **Add the ConfigMap** [deploy/cnpg/replay-time.yaml](../deploy/cnpg/replay-time.yaml) in the Cluster's namespace. Keep its query name `tack_replay`, its column name `timestamp_seconds` and its label `cnpg.io/reload`. Tack reads the metric `cnpg_tack_replay_timestamp_seconds` and nothing else.
2. **Name it in the Cluster.** Add an entry to `spec.monitoring.customQueriesConfigMap`, and keep the entries already there:

   ```yaml
   spec:
     monitoring:
       customQueriesConfigMap:
         - name: tack-replay-time
           key: queries
   ```

| Question | Answer, from CloudNativePG v1.30.1 |
|---|---|
| Restarts PostgreSQL? | No. Each instance reads the named ConfigMaps whenever it reconciles its Cluster. |
| Cost | One query, `pg_last_xact_replay_timestamp()`, at most once per `spec.monitoring.metricsQueriesTTL` (30 seconds by default), when something reads `/metrics`. |
| Permissions | None more: Tack reads it with the `get` on `pods/proxy` ([Permissions](permissions.md)). |
| Where kept | A site-loss run reads it again at `record-position-<to>`, right before the promotion, and keeps it in the ledger as `status.fork.lastCommit`. `tack status RUN` prints it on the `Fork:` line. Upgrade the CRD first ([Upgrade](guide/upgrade.md)). |

| The plan prints | When |
|---|---|
| `last replayed commit <time> (<age> ago)` in the `Replay` column | the query answers. The time is the old primary's clock, to the millisecond; the age is by Tack's clock. In a site-loss plan it adds: the writes the old primary committed after that time are lost, and so may be a few committed just before it |
| `(the clocks differ)` | the time is after Tack's clock |
| `replayed no transaction since it started` | PostgreSQL returns NULL |

The time is that of the last commit the replica replayed. It does not mean every earlier commit is in the replica: PostgreSQL stamps a commit before it writes the commit record, so under concurrent writes a commit stamped just before another can come after it in the WAL and not be replayed yet. An old primary that took no writes after the time loses none. The value can be up to `metricsQueriesTTL` old.
