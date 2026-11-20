# runner-warden

Autoscaling manager for **self-hosted GitHub Actions runners on your own machines**. One daemon per host serves the orgs and repos you add with fresh, one-job runners, sized by real memory use.

> ⚠️ Runners use the host Docker daemon, which is root-equivalent. Only serve repos whose workflows you trust. See [SECURITY.md](SECURITY.md).

## What it does

| Feature | How |
|---|---|
| One label per OS | Workflows use `runs-on: <prefix>-linux` or `<prefix>-windows` |
| Fresh runner per job | Ephemeral JIT runners; nothing persists between jobs except shared caches |
| Memory-aware autoscaling | One memory budget per host (a systemd slice hard cap, never throttled). A job that doesn't fit waits in GitHub's queue, where its timeout isn't ticking, while smaller jobs keep flowing |
| Busy repo uses the whole box | Work-conserving; fair sharing only when repos compete |
| `container:` / `services:` / `docker build` | Sibling containers through a per-runner Docker proxy that labels, places and cleans up everything a job creates |
| Hung jobs | Cancelled after no CPU/IO/output for `hang.idle`, or after `hang.max_duration` |
| Stale PR jobs | Cancelled when the PR is merged/closed or the pushed branch is deleted |
| Runner lost mid-job | Run cancelled immediately and the job re-run once |
| Shared caches | Host dirs (npm, Go, pub, Gradle…) mounted into runners and job containers with a fixed uid |
| Windows | `dockurr/windows` VMs on the same host, started on demand, one job at a time per VM |
| One-command install | `runner-warden setup` scans existing runners and workflows, installs the service, rewrites `runs-on`, retires old runners |

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/kylepantall/runner-warden/main/packaging/install.sh | sh
```

```bash
sudo runner-warden setup --prefix my-ci --org my-org --repo me/tool
```

Then change workflows to `runs-on: my-ci-linux` (setup opens a PR per repo for you).

Needs: Linux with systemd and cgroup v2, Docker 25+, `gh` logged in as a user with `repo` (+ `admin:org` for orgs, `workflow` for the label sweep).

## Commands

| Command | Does |
|---|---|
| `setup` | Install or update an instance (idempotent; `--dry-run` shows the plan) |
| `status` | Lanes, held/running jobs, memory, runners |
| `check` | Health check of service, slice, Docker, disk, daemon |
| `logs [-f] [runner\|run-id]` | Service logs, filtered |
| `drain [--off]` | Stop taking new jobs; running jobs finish |
| `kill <runner>` | Cancel a runner's job and stop it |
| `migrate apply\|retire\|rollback` | Label sweep and retirement of old runners |
| `windows provision\|adopt <vm>` | Create a Windows VM, or take over an existing dockurr disk |
| `config validate` | Validate the config |

## Configuration

`/etc/runner-warden/<instance>/config.toml` — see [docs/configuration.md](docs/configuration.md).

## How scheduling works

Each repo/org and OS gets several "lane" scale sets sharing one label. Each lane holds at most one job that hasn't started yet, so the manager always knows which job the next runner will get. A job that fits the memory budget gets a runner immediately; one that doesn't stays queued on GitHub without blocking the other lanes. Details: [PLAN.md](PLAN.md) and [spike/M0-FINDINGS.md](spike/M0-FINDINGS.md).

## Status

Pre-1.0. Linux and Windows backends; macOS is planned.

Licensed under [Apache-2.0](LICENSE).
