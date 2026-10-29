# The Workload file

A Workload file describes one workload: its sites, the Git repository Flux applies, and where Tack records runs. `tack plan` and `tack run` read it with `-f`. `tack status` and `tack resume` read it to find the sites; `tack resume` then runs with the copy stored in the run's ledger, not with the file.

## Example

Two sites, `a` and `b`, each running one CloudNativePG `Cluster` and one read-write `Pooler` in namespace `db`. Site a is the primary.

```yaml
apiVersion: failover.tako.id/v1alpha1
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

The Git repository it assumes:

```text
clusters/site-a/db/kustomization.yaml
clusters/site-a/db/cluster.yaml             Cluster pg-a
clusters/site-a/db/pooler.yaml              Pooler pg-a-pooler-rw
clusters/site-b/db/kustomization.yaml       patches: pooler-instances.yaml
clusters/site-b/db/cluster.yaml             Cluster pg-b
clusters/site-b/db/pooler.yaml              Pooler pg-b-pooler-rw
clusters/site-b/db/pooler-instances.yaml    a patch of Pooler pg-b-pooler-rw that sets spec.instances
```

For a switch to site b, `tack plan` resolves the Git change of this example to:

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
| `apiVersion` | yes | | `failover.tako.id/v1alpha1` |
| `kind` | yes | | `Workload` |
| `metadata.name` | yes | | The workload's name. A run is named `<name>-<UTC yyyymmdd-hhmmss>` and labelled `failover.tako.id/workload: <name>`, and the operator types the name to confirm a run. Use lowercase letters, digits and `-`. |
| `spec.driver` | yes | | The built-in driver that knows the workload's API. Only `cnpg`. |
| `spec.sites` | yes | | Every site of the workload, at least two. |
| `spec.git` | yes | | The Git repository Flux applies at every site. |
| `spec.ledger.namespace` | no | `tack-system` | The namespace of the `SiteFailover` objects, at every site. It must exist at every site. A run's ledger is created at its `--to` site. |
| `spec.routing` | no | | The routing check. Without it, a run has no routing check. |
| `spec.timeouts` | no | | Bounds on the run's waits. |

### `spec.sites[]`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `name` | yes | | The site's name in `--to`, `--from`, `--fence` and the ledger. Unique. Lowercase letters, digits and `-`, starting and ending with a letter or digit, at most 63 characters. |
| `kubeContext` | yes | | The kubeconfig context for this site. A context missing from the kubeconfig is an error; Tack never uses the current context instead. |
| `cnpg.cluster.namespace`, `cnpg.cluster.name` | yes | | The site's CloudNativePG `Cluster`. Each site's Cluster needs its own name. |
| `cnpg.pooler.namespace`, `cnpg.pooler.name` | no | | The site's read-write `Pooler`. Tack also waits on the Deployment of the same name, which CloudNativePG runs for it. Without a pooler, Tack moves no Pooler at this site. Never name a Pooler `<cluster>-rw`, `<cluster>-ro` or `<cluster>-r`: its Service takes its name, the Cluster already owns Services with those names, and CloudNativePG then never reconciles the Pooler. |
| `cnpg.pooler.instances` | with a pooler | | The Pooler's instances while this site is primary, at least 1. At every other site Tack sets it to 0. |

`tack plan` checks the live objects too: each `Cluster` sets `.spec.replica.primary` and not `.spec.replica.enabled`, and a Flux Kustomization applies each `Cluster` and `Pooler`. [RFC#0004](../rfcs/RFC0004.md) and [RFC#0005](../rfcs/RFC0005.md) give every check.

### `spec.git`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `url` | yes | | The repository, as `git` takes it: SSH, HTTPS or a path. Never put a credential in it; access comes from the operator's `git` configuration. |
| `branch` | yes | | The branch Flux applies. |
| `author.name`, `author.email` | no | git's own `user.name` and `user.email` | The author and committer of Tack's commit. Without `author`, git must have an identity configured; Tack never lets git guess one. |
| `fields[]` | no | | Mappings: where a field lives in the repository. A mapping wins over `find`. |
| `find` | no | | Lets Tack find the manifest that defines an object, for every field without a mapping. |

Every field a run changes needs a mapping or `find`, or the plan fails. One exception: when neither covers the removal of a used CloudNativePG promotion token, the plan leaves it out and says so. The fields each driver changes are in [RFC#0004](../rfcs/RFC0004.md) Requirement 8 for `cnpg`; `tack plan` prints each one with its file and path.

### `spec.git.fields[]`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `site` | yes | | The site of the object. Must be a site of the workload. |
| `object.kind`, `object.name` | yes | | The live object, such as `Pooler` `pg-b-pooler-rw`. |
| `object.namespace` | no | any namespace | The live object's namespace. |
| `field` | yes | | The field's path in the live object, such as `spec.instances`. |
| `file` | yes | | The file, relative to the repository root. |
| `path` | yes | | The field's path in the file. `a.b.c` is key `c` under `b` under `a`; `a[2].b` is key `b` of the third item of list `a`. Keys that contain dots cannot be addressed. |
| `document` | no | | The YAML document to edit, counting from 0. Required when the file holds several documents. |

A mapping matches a change on site, kind, name and field, and on namespace when it sets one. Two mappings that match the same change fail the plan. When Git answers at plan time, the file and the parent of `path` must exist in it.

### `spec.git.find`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `paths[].site` | yes | | A site of the workload. |
| `paths[].path` | yes | | A directory of the repository that holds this site's manifests. A site may have several. |

`find: {}` searches the whole repository for every site, and so does any site without a `paths` entry. Tack reads the `.yaml` and `.yml` files there, at the branch's head, and looks for the one YAML document whose API group, kind, name and namespace match the object. A document without `metadata.namespace` matches any namespace. If none or several match, the plan fails; for several, it names them. A mapping, or narrower `paths`, resolves it.

The edit rules (what Tack changes in a file, and what it refuses to edit) are in [RFC#0006](../rfcs/RFC0006.md).

### `spec.routing.strait`

| Field | Required | Default | Meaning |
|---|---|---|---|
| `services[]` | yes | | The Strait `SiteService` to read, `{site, namespace, name}`, one per site that runs Strait. At least one. |
| `members[]` | yes | | For every site of the workload, `{site, member}`: the name of the site's member in the `SiteService`. |

With this block, a run ends with one `Routing` step per service: it waits until Strait at that site routes to the `--to` site's member only. [RFC#0008](../rfcs/RFC0008.md) has the details.

### `spec.timeouts`

| Field | Default | Bounds |
|---|---|---|
| `demotion` | `10m` | the wait for the old primary to demote and publish its token |
| `promotion` | `10m` | the wait for the new primary to promote, every Pooler wait, and the final check that the new primary is healthy |
| `flux` | `5m` | each Flux reconcile |
| `routing` | `2m` | each Strait routing check |

A wait that times out stops the run with reason `StepFailed`.

## When the file is checked

| When | What |
|---|---|
| Every command that reads the file | Unknown fields, `apiVersion` and `kind`, required fields, unique site names, and that each `site` in `git.fields`, `git.find.paths` and `routing.strait` names a site. With `routing.strait`, every site needs a member. |
| `tack plan` and `tack run`, before reading the sites | `driver` is built in; each site has `cnpg.cluster`; Cluster names differ; each pooler has a namespace and a name. |
| `tack plan` and `tack run`, when reading Git | Each mapped file and the parent of its path exist; `find` finds exactly one manifest per object. |
| `tack run`, when it creates the ledger | The API server checks the `SiteFailover` schema, which holds a copy of the Workload: the site name pattern and length, `pooler.instances` of at least 1, and at least one Strait service. A run that fails here has changed nothing. |
