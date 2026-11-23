# AGENTS.md

## Project overview

Tack switches a stateful workload's primary from one site to another: a planned switchover, or a failover after a site is lost. It acts through the workload's own declarative API, keeps GitOps in step, and records every step in a ledger.

- **The operator starts every run.** Tack never decides to fail over by itself.
- Tack is a CLI, `tack`, and a ledger resource, `SiteFailover` (API group `failover.tako.id`). It has no controller. A controller that runs the same ledger may come later.

Design decisions live in `rfcs/`. Read the RFCs on a subject before changing it.

## Scope

Belongs here:
- the `tack` CLI
- the ledger resource and its phases: Hold, Failover, Promote to GitOps, Release
- support per workload type, CloudNativePG first
- holding and releasing Flux for the objects a run changes
- confirming that routing followed, by reading Strait's status

Does not belong here:
- **Routing.** Strait's job. Tack may read Strait's objects and never writes them.
- **Deciding when to fail over.** The operator decides.
- **High availability inside one site.** The workload's own controller handles it.

## Rules

1. **The operator starts every run.** A run without the operator's command is a bug.
2. **Act through the workload's own API.**
   - For CloudNativePG: `.spec.replica.primary` and the promotion token.
   - Never edit a workload's data, files or processes directly.
3. **The ledger records every step.**
   - Write the phase before a step starts and the result after it ends: tokens, held objects, the Git commit.
   - A run resumes from its ledger, never from memory.
4. **Hold Flux narrowly.**
   - Annotate only the objects the run changes, in the cluster: `kustomize.toolkit.fluxcd.io/reconcile: disabled`.
   - Never suspend a whole Kustomization unless an RFC says so.
5. **Never release before Git matches live.**
   - Releasing Flux early undoes the switch.
   - After release, Flux must have nothing to change. Check it.
6. **Stop and ask on anything unexpected:** Git changed during a run, a token is missing, or a site that should be fenced answers as primary.
7. **Plan before apply.**
   - A dry run prints every step and every change, and changes nothing.
   - Changing a live cluster needs the operator's explicit approval.
8. **Secrets never in the repo.**
   - Secrets come from a secret store at runtime. Files hold references, never values.
   - Never print a secret into output or logs.
9. **Check facts, never assume.**
   - Verify against a live cluster or the primary source before acting. If a resource cannot be reached, stop and ask the operator.
   - Pass `--context` explicitly on every `kubectl` command.

## RFCs

### Format

One file per RFC: `rfcs/RFCNNNN.md`, numbered in order and never reused.

```markdown
<h1 style="text-align: center;">[RFC#NNNN] Title</h1>

# Abstract
# Status
- Version: X.Y
- Status: Draft | Active | Inactive
- Created: YYYY-MM-DD
- Last Updated: YYYY-MM-DD
# Table of Contents
# Overview
# Motivation          (optional: observed failures or reasons, so a rule is not undone by accident)
# Requirements
# Design
# Implementation      (when applicable: where the repo realises it, paths not copies)
# Security Considerations   (when applicable)
# Limitations
# Open Questions      (Draft only)
# References
```

An RFC MAY add a section its subject needs, such as a Glossary after the Overview.

### Writing them

- **Declarative.** An RFC states the end state and its constraints: values, boundaries, invariants. Steps go in `docs/runbooks/`, which the RFC links.
- **Normative.**
  - Requirements use MUST, MUST NOT, SHOULD and MAY (RFC 2119).
  - Every MUST is checkable by a test, a dry run, review of a change, or inspecting a cluster. Rewrite one that is not.
  - Rules go in Requirements. Design describes the shape that satisfies them.
- **Scoped.** One subject per RFC. When two touch, one owns the subject and the other cites it as `[RFC#NNNN]`.
- **No index.** `rfcs/` is the index. Each Status block is the source of truth.
- **Implementation conforms.** When code and RFC disagree, the RFC wins. Amend the RFC first, in the same change or an earlier one.

### API changes

The ledger resource is Tack's contract with its users and with a future controller.
- A change that breaks a user is a new API version. Compatible additions stay in the same version.
- Record the operator's agreement in the change's description.

### Lifecycle

| Status | Meaning |
|---|---|
| Draft | Under discussion. The code MUST NOT rely on it yet. May carry Open Questions. |
| Active | In force. The code conforms. |
| Inactive | Retired or superseded. Kept for history. The replacing RFC says "This RFC deprecates RFC#NNNN." |

- **Promote Draft to Active only with the operator's approval**, once every Open Question is resolved and the section is removed.
- A decision deliberately deferred to a later trigger is a Limitation, not an Open Question.
- **Bump the version and Last Updated on every change.**

## Writing (docs, comments, commits, replies)

- **English only. Neutral:** refer to "the operator" or a role, never a person, and no personal pronouns.
- **Not written = doesn't exist.** Don't write what's absent, a default, general knowledge or history. Delete what's obsolete; git is the archive.
- **Dead simple.** Short sentences, everyday words. No intros, no filler, no em-dashes.
- **Tables over prose for facts.** Each table row is one record.
- **A fact lives in one doc;** link, don't copy.
- Talk to the operator in their language, just as plain.

### Keeping text lean

- **Search before adding.** Before writing a fact, `rg` for it. If a page owns it, link there.
- **Never grow prose back.** An edit keeps or lowers the word count of what it touches, unless it adds a new fact. Never restore cut text, or add reasons, examples or restatements to it.
- **Effect, then action.** A table cell or a comment states the effect and what to do, in one or two sentences, about 30 words at most. The mechanism lives in the requirement or RFC it cites. More becomes a list or a section.
- **No history anywhere:** no issue, PR or review references such as `(#35)` or `#35 row 47`, no "since v1.3.0" or "used to". Per-release notes live only in the upgrade and rollback guides. `make prose` checks the references.
- **No line references into other projects' source.** Name the version, link the upstream docs.
- **No meta lines:** no `This page explains`, no recap, no sentence that repeats a heading, table, code block or link text.
- **RFC Requirements hold rules only.** Reasons go in Motivation, never deleted; steps go in `docs/runbooks/`.
- **README is a pitch and links.** It holds no commands or tables from the guides.

| Fact | Owner |
|---|---|
| flags, exit codes, rules for AI agents | `docs/reference/cli.md` |
| run states, reading a run, Lease timing, fencing, break-glass, write downtime | `docs/operations.md` |
| what to do per stop reason | `docs/runbooks/stop-reasons.md` |
| permissions, the chart's roles, who may write ledgers, Git access | `docs/permissions.md`; `deploy/helm/rbac_test.go` parses its table |
| pruning old ledgers | `docs/old-ledgers.md` |
| Workload file fields, replay time, WAL archives | `docs/workload.md` |
| data loss and time budgets per mode | `docs/wal-rpo-rto.md` |
| phases and glossary | `docs/guide/concepts.md` |
| scope and supported versions | `docs/guide/introduction.md` |
| install, clocks, verifying a release | `docs/guide/install.md` |
| per-release upgrade notes | `docs/guide/upgrade.md` |
| contributing, the end-to-end test, reproducing a release | `docs/contributing.md` |
| rules Tack follows | the RFC that owns the subject; docs link to it, never restate it |

## Commits

Conventional commits: `type(scope): subject`. The type is one of `feat`, `fix`, `docs`, `chore`, `refactor` or `test`. The scope is an area or an RFC, for example `docs(rfc0002): define the ledger phases`.

Commit and push only when the operator asks.

## Current state

- RFC0001 to RFC0009 are Active.
- Tack reads Strait's `SiteService` status ([RFC#0008](rfcs/RFC0008.md)). The e2e runs Strait built from the commit pinned in `test/e2e/env/lib.sh`.

| Command | Does |
|---|---|
| `make build` | builds `bin/tack` |
| `make test` | unit and golden tests; `go test ./internal/cli -update` rewrites the golden files |
| `make test-race` | the same tests with the race detector, about ten times slower |
| `make verify` | regenerates the CRD and deepcopy code, checks gofmt and vet, fails on stale generated files |
| `make dist` | static binaries for Linux, macOS and Windows, man pages, the CRD; the same bytes for the same commit |
| `make sbom` | after `make dist`, a CycloneDX SBOM per archive |
| `make lint` | `go mod verify`, `govulncheck`, staticcheck (its default checks), `helm lint`, and the check that every action is pinned by SHA |
| `make prose` | lints every tracked Markdown file and Vue template with `vale`, by the rules of Writing; the rules are in `hack/vale/Tack/` |
| `hack/release/build.sh vX.Y.Z` | every asset of a release and `SHA256SUMS` in `dist/`, as the release workflow builds them |
| `make e2e` | the end-to-end test on kind; it creates only `tack-e2e*` clusters, networks, containers and images |
| `cd docs && bun install && bun run build` | the documentation site, tack.tako.id; fails on a dead link. Its look is [DESIGN.md](DESIGN.md), its readers and voice [PRODUCT.md](PRODUCT.md) |
