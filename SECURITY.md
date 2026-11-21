# Security policy

## Reporting

Report vulnerabilities privately via **GitHub → Security → Report a vulnerability** on this repository. Please don't open public issues for security problems. Expect a first reply within 7 days.

## Supported versions

Only the latest minor release gets fixes while the project is `0.x`.

## Threat model (read before deploying)

| Assumption | Consequence |
|---|---|
| The daemon runs as root and talks to the host Docker daemon | Anything that can reach the Docker socket is root-equivalent on the host |
| Jobs reach Docker through a per-runner proxy | The proxy labels, places and cleans up containers. It is **not** a sandbox: a job can still start privileged containers or mount host paths |
| Shared caches are writable by jobs | A malicious job can poison caches for later jobs of the same owner |
| The daemon's GitHub token never enters job containers | Jobs that need GitHub access use `GITHUB_TOKEN`, an App token or a secret |

| The daemon's token is readable by root | A Linux job can reach root via Docker, so treat the token as exposed to every served workflow. **Use a fine-grained PAT (or GitHub App) limited to the served orgs/repos** — not a personal `gh` token with `admin:org` across all your orgs |
| Windows VMs are persistent and jobs run as the VM's admin user | A job can tamper with the VM for later jobs (including other repos the VM serves). Scope each `[[windows.vm]]` with `targets` to repos that trust each other; a fresh disk per job is planned |

**Only serve repositories whose workflows you trust.** Public repositories are refused unless explicitly allowed; route fork pull requests to GitHub-hosted runners.

Reports that rely on a job being malicious are triaged against this model.
