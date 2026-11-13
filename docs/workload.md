# The Workload file

A Workload file describes one workload: its sites, the Git repository Flux applies, and where Tack records runs. `tack plan` and `tack run` read it with `-f`. `tack status` and `tack resume` read it to find the sites. `tack resume` then runs with the copy stored in the run's ledger, not with the file.

## Example

Two sites, `a` and `b`, each running one CloudNativePG `Cluster` and one read-write `Pooler` in namespace `db`. Site a is the primary.

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

The example assumes this Git repository:

```text
clusters/site-a/db/kustomization.yaml
clusters/site-a/db/cluster.yaml             Cluster pg-a
clusters/site-a/db/pooler.yaml              Pooler pg-a-pooler-rw
clusters/site-b/db/kustomization.yaml       patches: pooler-instances.yaml
clusters/site-b/db/cluster.yaml             Cluster pg-b
clusters/site-b/db/pooler.yaml              Pooler pg-b-pooler-rw
clusters/site-b/db/pooler-instances.yaml    a patch of Pooler pg-b-pooler-rw that sets spec.instances
```

For a switch to site b, `tack plan` turns this example into these Git changes:

```text
clusters/site-a/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site a, Cluster db/pg-a spec.replica.primary)
clusters/site-a/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site a, Cluster db/pg-a spec.replica.promotionToken)
clusters/site-a/db/pooler.yaml, document 0: spec.instances = 0  (site a, Pooler db/pg-a-pooler-rw spec.instances)
clusters/site-b/db/cluster.yaml, document 0: spec.replica.primary = "pg-b"  (site b, Cluster db/pg-b spec.replica.primary)
clusters/site-b/db/cluster.yaml, document 0: spec.replica.promotionToken removed  (site b, Cluster db/pg-b spec.replica.promotionToken)
clusters/site-b/db/pooler-instances.yaml: spec.instances = 2  (site b, Pooler db/pg-b-pooler-rw spec.instances)
```

Without the mapping, the plan stops: `several manifests define the object: clusters/site-b/db/pooler-instances.yaml document 0, clusters/site-b/db/pooler.yaml document 0`.

## Fields

Required fields have no default. Durations are Go durations, such as `90s`, `15m` or `1h`.

### Top level

| Field | Required | Default | Meaning |
|---|---|---|---|
| `apiVersion` | yes | | `failover.tako.id/v1`. Tack still reads `failover.tako.id/v1alpha1`, the version of v0.1.x, with a warning. |
| `kind` | yes | | `Workload` |
| `metadata.name` | yes | | The workload's name. A run is named `<name>-<UTC yyyymmdd-hhmmss>` and labelled `failover.tako.id/workload: <name>`. The operator types this name to confirm a run. Use lowercase letters, digits and `-`. |
| `spec.driver` | yes | | The built-in driver that knows the workload's API. The only driver is `cnpg`. |
| `spec.sites` | yes | | Every site of the workload, at least two. |
| `spec.git` | yes | | The Git repository Flux applies at every site. |
| `spec.ledger.namespace` | no | `tack-system` | The namespace of the `SiteFailover` objects, at every site. It must exist at every site. A run's ledger is created at its `--to` site. |
| `spec.routing` | no | | The routing check. Without it, a run has no routing check. |
| `spec.timeouts` | no | | Time limits on the run's waits. |

### `spec.sites[]`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `name` | yes | | The site's name, as used in `--to`, `--from`, `--fence` and the ledger. It must be unique. It must use lowercase letters, digits and `-`, start and end with a letter or digit, and have at most 63 characters. Quote a name that YAML reads as a boolean or a number, such as `"n"`, `"on"` or `"010"`. Quote it here and wherever a `site:` names it: Tack refuses it unquoted. |
| `kubeContext` | yes | | The kubeconfig context for this site. A context missing from the kubeconfig is an error: Tack never uses the current context instead. Quote a context that YAML reads as a boolean or a number, such as `"on"`: Tack refuses it unquoted. [One kubeconfig](#every-sites-context-in-one-kubeconfig) holds every site's context. |
| `cnpg.cluster.namespace`, `cnpg.cluster.name` | yes | | The site's CloudNativePG `Cluster`. Each site's Cluster needs its own name. |
| `cnpg.pooler.namespace`, `cnpg.pooler.name` | no | | The site's read-write `Pooler`. Tack also waits on the Deployment of the same name, which CloudNativePG runs for the Pooler. Without a pooler, Tack moves no Pooler at this site. Never name a Pooler `<cluster>-rw`, `<cluster>-ro` or `<cluster>-r`. The Pooler's Service takes the Pooler's name, and the Cluster already owns Services with those names. CloudNativePG then never reconciles the Pooler. |
| `cnpg.pooler.instances` | with a pooler | | The Pooler's instances while this site is primary. At least 1. While another site is primary, Tack sets it to 0. |
| `cnpg.snapshot` | no | `false` | Take a cold snapshot of the volumes of this site's Cluster, after a site loss that leaves this Cluster the old primary. Tack takes it once the site answers again, before anyone rebuilds the Cluster. The snapshot is a CloudNativePG `Backup` with `method: volumeSnapshot`. The Cluster needs `spec.backup.volumeSnapshot`, and the site needs VolumeSnapshot support. A Backup that fails stops the run. Set it on every site, because any site can be the old primary. See [recover-old-primary-writes.md](runbooks/recover-old-primary-writes.md). |

`tack plan` checks the live objects too. Each `Cluster` must set `.spec.replica.primary` and not `.spec.replica.enabled`. A Flux Kustomization must apply each `Cluster` and `Pooler`. [RFC#0004](../rfcs/RFC0004.md) and [RFC#0005](../rfcs/RFC0005.md) list every check.

### Every site's context in one kubeconfig

Tack reads every site's `kubeContext` from one kubeconfig: the file `--kubeconfig` names, or else the one the standard loading rules find. When each cluster has its own kubeconfig file, list the files in `KUBECONFIG`. client-go then merges them:

```sh
KUBECONFIG=site-a.kubeconfig:site-b.kubeconfig tack plan -f workload.yaml --to b
```

On Windows the separator is `;`. In PowerShell:

```powershell
$env:KUBECONFIG = "site-a.kubeconfig;site-b.kubeconfig"
tack plan -f workload.yaml --to b
```

| Rule | Why |
|---|---|
| `--kubeconfig` takes one file. | With it, Tack ignores `KUBECONFIG`. |
| Context, cluster and user names differ between the files. | If a name repeats, the first file's entry wins and the others are dropped. A site's context could then reach another site's cluster, or use its credentials. |

To keep a single file instead, flatten the merged files into one, then pass it with `--kubeconfig`:

```sh
KUBECONFIG=site-a.kubeconfig:site-b.kubeconfig kubectl config view --flatten > sites.kubeconfig
tack --kubeconfig sites.kubeconfig plan -f workload.yaml --to b
```

The merged file holds every site's credentials. Protect it like the originals.

### `spec.git`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `url` | yes | | The repository, as `git` takes it: SSH, HTTPS or a path. Never put a credential in it: access comes from the operator's `git` configuration. Tack refuses a URL with a password, and an HTTP or HTTPS URL with a user. Set the user with `git config credential.<url>.username` instead. |
| `branch` | yes | | The branch Flux applies. |
| `author.name`, `author.email` | no | git's own `user.name` and `user.email` | The author and committer of Tack's commit. Without `author`, git must have an identity configured. Tack never lets git guess one. |
| `fields[]` | no | | Mappings. Each says where a field lives in the repository. A mapping wins over `find`. |
| `find` | no | | Lets Tack find the manifest that defines an object, for every field without a mapping. |

Every field a run changes needs a mapping or `find`, or the plan fails. There is one exception: the removal of a used CloudNativePG promotion token. When neither covers it, the plan leaves that change out and says so. For `cnpg`, the fields the driver changes are in [RFC#0004](../rfcs/RFC0004.md) Requirement 8. `tack plan` prints each one with its file and path.

### `spec.git.fields[]`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `site` | yes | | The site of the object. Must be a site of the workload. |
| `object.kind`, `object.name` | yes | | The live object, such as `Pooler` `pg-b-pooler-rw`. |
| `object.namespace` | no | any namespace | The live object's namespace. |
| `field` | yes | | The field's path in the live object, such as `spec.instances`. |
| `file` | yes | | The file, relative to the repository root. |
| `path` | yes | | The field's path in the file. `a.b.c` is key `c` under `b` under `a`. `a[2].b` is key `b` of the third item of list `a`. Keys that contain dots cannot be addressed. |
| `document` | no | | The YAML document to edit, counting from 0. Required when the file holds several documents. |

A mapping matches a change when site, kind, name and field match. If the mapping sets a namespace, the namespace must match too. If two mappings match the same change, the plan fails. When Git answers at plan time, the file and the parent of `path` must exist in it.

### `spec.git.find`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `paths[].site` | yes | | A site of the workload. |
| `paths[].path` | yes | | A directory of the repository that holds this site's manifests. A site may have several. |

`find: {}` searches the whole repository for every site. A site without a `paths` entry is searched the same way. Tack reads the `.yaml` and `.yml` files there, at the branch's head. It looks for the one YAML document whose API group, kind, name and namespace match the object. A document without `metadata.namespace` matches any namespace. If no document matches, or several do, the plan fails. When several match, the plan names them. A mapping, or narrower `paths`, fixes that.

The edit rules (what Tack changes in a file, and what it refuses to edit) are in [RFC#0006](../rfcs/RFC0006.md).

### `spec.routing.strait`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `services[]` | yes | | The Strait `SiteService` to read, `{site, namespace, name}`, one per site that runs Strait. At least one. |
| `members[]` | yes | | For every site of the workload, `{site, member}`: the name of the site's member in the `SiteService`. |

With this block, a run ends with one `Routing` step per service. Each step waits until Strait at that site routes only to the `--to` site's member. [RFC#0008](../rfcs/RFC0008.md) has the details.

### `spec.timeouts`

| Field | Default | Bounds |
|---|---|---|
| `demotion` | `10m` | the wait for the old primary to demote and publish its token |
| `promotion` | `10m` | the wait for the new primary to promote, every Pooler wait, and the final check that the new primary is healthy |
| `flux` | `5m` | each Flux reconcile |
| `routing` | `2m` | each Strait routing check |
| `snapshot` | `30m` | the wait for the old primary's snapshot (`cnpg.snapshot`) |
| `archiving` | `5m` | the wait, after the promotion, for the new primary to archive WAL of its new timeline. When it times out, the run warns, records the follow-up `CheckArchiving`, and goes on |

A wait that times out stops the run with reason `StepFailed`. The exception is `archiving`.

## WAL archives

Give each site's Cluster a WAL archive of its own from day one: its own `destinationPath`, or its own `serverName` under a shared path. A replica Cluster archives too, because CloudNativePG runs it with `archive_mode = always` (v1.30.1, `pkg/postgres/configuration.go:736-746`). So every site writes WAL to its archive all the time, not only after a promotion. Two sites with one destination write the same file names there. And CloudNativePG refuses to start a Cluster on an archive that is not empty (`docs/src/recovery.md:620-629`, `Expected empty archive`).

| Tack | Does |
|---|---|
| `tack plan` | reads each site's destination: `spec.backup.barmanObjectStore`, or the `destinationPath` of the barman-cloud plugin's `ObjectStore`. A planned switchover is refused when two sites that answer share one. A site-loss plan only warns, since the fix is in Git, and Git may have gone with the lost site. The plan also warns when the new primary reports `ContinuousArchiving` `False`. |
| `verify-archiving-<to>`, after the promotion | waits up to `timeouts.archiving` until the new primary archives WAL of its new timeline. That means `ContinuousArchiving` is `True`, and its last archived file is of that timeline. A Cluster without an archive passes at once. When the wait times out, the run warns, records `CheckArchiving`, and goes on. Writes are back by then, and the operator fixes the archive. |
| after a site loss | records, on the old primary's `Rebuild` follow-up, where it archived. The rebuilt Cluster gets a new destination, and the old one is kept ([rebuild-old-primary.md](runbooks/rebuild-old-primary.md)). |

## Replay time

Opt-in. With it, a site-loss plan says when the last commit that the new primary replayed was. Without it, the plan prints each replica's replay lag only. That lag is 0 once a replica has replayed all it received, which is the usual state after a site loss.

Make two changes in Git, at each site:

1. **Add the ConfigMap** [deploy/cnpg/replay-time.yaml](../deploy/cnpg/replay-time.yaml) in the Cluster's namespace. Keep its query name `tack_replay` and its column name `timestamp_seconds`. Tack reads the metric `cnpg_tack_replay_timestamp_seconds` and nothing else. Keep its label `cnpg.io/reload`.
2. **Name it in the Cluster.** Add an entry to `spec.monitoring.customQueriesConfigMap`. Keep the entries that are already there:

   ```yaml
   spec:
     monitoring:
       customQueriesConfigMap:
         - name: tack-replay-time
           key: queries
   ```

| Question | Answer, from CloudNativePG v1.30.1 |
|---|---|
| Does it restart PostgreSQL? | No. Each instance reads the named ConfigMaps each time it reconciles its Cluster (`internal/management/controller/instance_controller.go:176,1035-1118`). `spec.monitoring` reaches the Pod only through `tls.enabled` (`pkg/specs/pods.go:300-302`). |
| What if the ConfigMap is missing? | Each instance logs a warning and runs without the query (`internal/management/controller/instance_controller.go:1057-1066`). Once the ConfigMap exists, its label `cnpg.io/reload` makes the operator reconcile the Clusters that name it (`internal/controller/cluster_predicates.go:154-157`, `internal/controller/cluster_controller.go:1652-1666`), and each instance reads it again. |
| Does it need `disableDefaultQueries: false`? | No. Tack reads the replay lag and the replay time from one GET, and uses each one it finds. |
| What does it cost? | One query, `pg_last_xact_replay_timestamp()`, run at most once per `spec.monitoring.metricsQueriesTTL` (30 seconds by default), when something reads `/metrics`. |
| What permissions? | None more: Tack reads it with the GET of row 18 of [operations.md](operations.md#permissions). |
| Where is it kept? | A site-loss run reads it again right before the promotion, at `record-position-<to>`, and keeps it in the ledger as `status.fork.lastCommit`. `tack status RUN` prints it on the `Fork:` line. Upgrade the CRD first ([README](../README.md#upgrading-within-v1)): a CRD without this field drops it from every status write, without an error. |

| The plan prints | When |
|---|---|
| `last replayed commit <time> (<age> ago)` in the `Replay` column, and in a site-loss plan: the writes that the old primary committed after that time are lost, and so may be a few committed just before it | the query answers. The time is the old primary's clock, to the millisecond; the age is by Tack's clock. |
| `(the clocks differ)` | the time is after Tack's clock |
| `replayed no transaction since it started` | the instance replayed no transaction since it started: PostgreSQL returns NULL |
| what it prints without the query | the metric is not there |

The time is that of the last commit the replica replayed. It is not a line before which every commit is in the replica. PostgreSQL stamps a commit before it writes the commit record to the WAL (`src/backend/access/transam/xact.c`, `RecordTransactionCommit`). So, under concurrent writes, a commit stamped just before another can come after it in the WAL, and not be replayed yet. An old primary that took no writes after the time loses none, however old the time is. The value can be up to `metricsQueriesTTL` old.

## When the file is checked

| When | What |
|---|---|
| Every command that reads the file | Unknown fields, `apiVersion` and `kind`, required fields, unique site names, and that each `site` in `git.fields`, `git.find.paths` and `routing.strait` names a site. With `routing.strait`, every site needs a member. |
| `tack plan` and `tack run`, before reading the sites | `driver` is built in. Each site has `cnpg.cluster`. Cluster names differ. Each pooler has a namespace and a name. |
| `tack plan` and `tack run`, when reading Git | Each mapped file and the parent of its path exist. `find` finds exactly one manifest per object. |
| `tack run`, when it creates the ledger | The API server checks the `SiteFailover` schema, which holds a copy of the Workload. It checks the site name pattern and length, `pooler.instances` of at least 1, and at least one Strait service. A run that fails here has changed nothing. |
