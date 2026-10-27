# AGENTS.md

## Project overview

Tack switches a stateful workload's primary from one site to another: a planned switchover, or a failover after a site is lost. It acts through the workload's own declarative API, keeps GitOps in step, and records every step in a ledger.

- **The operator starts every run.** Tack never decides to fail over by itself.
- Today Tack is a CLI, `tack`, and a ledger resource, `SiteFailover` (API group `failover.tako.id`), with no controller behind it. A controller that runs the same ledger may come later.

Design decisions live in `rfcs/`. Read the RFCs that cover a subject before changing anything in it.

## Scope

Belongs here:
- the `tack` CLI
- the ledger resource and its phases: Hold, Failover, Promote to GitOps, Release
- support per workload type, CloudNativePG first
- holding and releasing Flux for the objects a run changes
- confirming that routing followed, by reading Strait's status

Does not belong here:
- **Routing.** That is Strait's job. Tack may read Strait's objects; it never writes them.
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

- **Declarative.** An RFC states the end state and its constraints: values, boundaries, invariants. Step-by-step work goes in `docs/runbooks/`, and the RFC links to it.
- **Normative.**
  - Requirements use MUST, MUST NOT, SHOULD and MAY (RFC 2119).
  - Every MUST is checkable: by a test, by a dry run, by review of a change, or by inspecting a cluster. If it cannot be checked, rewrite it until it can.
  - Rules go in Requirements. Design describes the shape that satisfies them.
- **Scoped.** One subject per RFC. If two RFCs touch, one owns the subject and the other references it as `[RFC#NNNN]`.
- **No index.** `rfcs/` is the index, and each Status block is the source of truth.
- **Implementation conforms.** If the code and an RFC disagree, the RFC wins. Amend the RFC first, in the same change or an earlier one.

### API changes

The ledger resource is Tack's contract with its users and with a future controller.
- A change that would break a user is a new API version. Compatible additions are fine in the same version.
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

## Commits

Conventional commits: `type(scope): subject`. The type is one of `feat`, `fix`, `docs`, `chore`, `refactor` or `test`. The scope is an area or an RFC, for example `docs(rfc0002): define the ledger phases`.

Commit and push only when the operator asks.

## Current state

- Started on 2026-10-27. No RFC and no code yet.
- Tack uses Strait (`takodotid/strait`). Design Tack's reading of Strait's status against Strait's RFCs once they exist.
- There are no commands to run yet. Add them here when they exist.
