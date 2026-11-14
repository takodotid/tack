# Security policy

## Reporting a vulnerability

Report it privately, through GitHub's private vulnerability reporting: the repository's **Security** tab, then **Report a vulnerability**. Do not open a public issue or pull request for it.

| Include | Example |
|---|---|
| The version | `tack --version`, or the commit |
| What an attacker can do, and what access they need first | write access to a ledger namespace, a Git URL in a Workload file |
| How to reproduce it | the commands, and the Workload file without its secrets |

Tack holds no secret ([RFC0009, Security Considerations](rfcs/RFC0009.md#security-considerations)). Never send a kubeconfig, a token or a Git credential with a report.

## Supported versions

| Version | Security fixes |
|---|---|
| The latest release | yes |
| Older releases | no. Upgrade as [Upgrade within v1](docs/guide/upgrade.md#upgrade-within-v1) shows. |

## Verifying a release

Each release from v1.3.0 is signed and attested: [Verify a release](docs/guide/install.md#verify-a-release).
