# The tack command

`tack` plans, runs and reads runs. `tack <command> --help` and `man tack-<command>` hold the full text of each command, and this page lists them.

| Command | Does |
|---|---|
| `tack plan -f WORKLOAD --to SITE [--mode planned\|site-loss]` | Prints every step and change of a run, and changes nothing. |
| `tack run -f WORKLOAD --to SITE [--mode planned\|site-loss]` | Starts a run: hold Flux, switch the primary, commit to Git, release. It prints the plan and asks for the workload's name first. |
| `tack resume RUN (-f WORKLOAD \| --context CONTEXT)` | Continues a run from its ledger. |
| `tack status [RUN] (-f WORKLOAD \| --context CONTEXT)` | Shows runs and their steps from the ledger. |
| `tack abandon RUN (-f WORKLOAD \| --context CONTEXT)` | Marks a run that must never go on ([Abandon a run](../runbooks/abandon-run.md)). |
| `tack prune -f WORKLOAD --keep N` | Deletes the ledgers of old runs that succeeded, keeping the `N` newest. |
| `tack completion bash\|zsh\|fish\|powershell` | Prints a shell completion script, and contacts nothing ([Shell completion](../guide/install.md#shell-completion)). |

## Flags

| Flag | Commands | Does |
|---|---|---|
| `--kubeconfig FILE` | every command | the kubeconfig holding every site's context. Default: the standard kubeconfig loading rules. |
| `-f`, `--file WORKLOAD` | every command but `completion` | the Workload file. `resume`, `status` and `abandon` then search every site of the workload for its runs. |
| `--to SITE` | `plan`, `run` | the site that becomes primary |
| `--from SITE` | `plan`, `run` | the site of the current primary. Default: the primary that the `--to` site declares. |
| `--mode planned\|site-loss` | `plan`, `run` | `planned`, the default: the old primary demotes and hands over its token. `site-loss`: promote without a token after the old site is lost. |
| `--accept-behind` | `plan`, `run` | with `--mode site-loss`: promote the target although another site that answers holds WAL past it. That site then needs a rebuild. |
| `-o`, `--output text\|yaml\|json` | `plan`, `status` | for `plan`, yaml or json print the `SiteFailover` the run would create |
| `--yes` | `run`, `resume`, `abandon`, `prune` | skip the confirmation. Only when a human approved this run. |
| `--take-over RUN` | `run` | take over the holds of this earlier run, which can no longer finish. Repeat it for each run. |
| `--force` | `run` with `--take-over`, `abandon` | go on although a tack process may still run the run: its lease is fresh, or its ledger was just written. Only once a human checked that none does. |
| `--fence SITE` | `resume` | demote this site although it answers as primary. Repeatable. |
| `--accept-git-head SHA` | `resume` | accept this commit, pushed during the run, as the run's new Git base |
| `--context CONTEXT` | `resume`, `status`, `abandon` | a kube context to read runs from, instead of `--file` |
| `--namespace NAMESPACE` | `resume`, `status`, `abandon` | the ledger namespace, with `--context`. Default: `tack-system`. |
| `--reason TEXT` | `abandon` | why the run is abandoned, recorded in its ledger |
| `--keep N` | `prune` | keep this many of the newest runs that succeeded, 1 or more |
| `--skill` | `tack` | print a guide for AI agents as a skill document, and exit |
| `-v`, `--version` | `tack` | print the version |

[The decision flags](../operations.md#stopped-and-waiting-runs) say when to pass `--fence`, `--accept-git-head` and `--take-over`.

## Exit codes

| Code | Means |
|---|---|
| 0 | success |
| 1 | error |
| 2 | usage error |
| 3 | the run stopped and waits for the operator (see `tack status`) |
| 4 | the run waits for something outside it, such as Git (`tack resume` later) |

## Rules for AI agents

`tack --help` and `tack --skill` hold these rules:

- An agent never starts `tack run` or `tack resume` without an explicit instruction from a human for that run.
- `tack plan` comes first. Show its output to the human before any run.
- An agent never passes `--yes`, `--fence`, `--accept-git-head`, `--accept-behind`, `--take-over` or `--force` unless the human's instruction for that run names it.
- `tack plan` and `tack status` only read; an agent may run them freely.
- When a run stops, report its reason and message to the human and wait.
