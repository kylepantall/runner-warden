# runner-warden

Autoscaling manager for **self-hosted GitHub Actions runners on your own machines**. One daemon serves the orgs and repos you add with fresh, one-job runners, sized by real memory and CPU use, on its host and on other machines it drives over SSH.

> ⚠️ Runners use the host Docker daemon, which is root-equivalent. Only serve repos whose workflows you trust. See [SECURITY.md](SECURITY.md).

## What it does

| Feature | How |
|---|---|
| One label per OS | Workflows use `runs-on: <prefix>-linux`, `<prefix>-windows` or `<prefix>-macos` |
| Fresh runner per job | Ephemeral JIT runners; nothing persists between jobs except shared caches |
| Memory- and CPU-aware autoscaling | One memory budget (a systemd slice hard cap, never throttled) and one CPU budget per host. A job that doesn't fit waits in GitHub's queue, where its timeout isn't ticking, while smaller jobs keep flowing |
| Busy repo uses the whole box | Work-conserving; fair sharing only when repos compete |
| `container:` / `services:` / `docker build` | Sibling containers through a per-runner Docker proxy that labels, places and cleans up everything a job creates |
| Hung jobs | Cancelled after no CPU/IO/output for `hang.idle`, or after `hang.max_duration` |
| Stale PR jobs | Cancelled when the PR is merged/closed or the pushed branch is deleted |
| Runner lost mid-job | Run cancelled immediately and the job re-run once |
| Shared caches | Host dirs (npm, Go, pub, Gradle…) mounted into runners and job containers with a fixed uid |
| Windows | `dockurr/windows` VMs on the same host, started on demand, and separate Windows machines over SSH; one job at a time each |
| Overflow Linux hosts | Other Linux machines or WSL distros take the Linux jobs this host can't fit; the daemon installs and updates itself there |
| macOS | A persistent tart VM on a Mac, driven over SSH; a fresh runner per job in the guest's GUI session (login keychain works) |
| One-command install | `runner-warden setup` scans existing runners and workflows, installs the service, rewrites `runs-on`, retires old runners |

## Quick start

```bash
curl -fsSL https://raw.githubusercontent.com/kylepantall/runner-warden/main/packaging/install.sh | sh
```

```bash
sudo runner-warden setup --prefix my-ci --org my-org --repo me/tool
```

Then change workflows to `runs-on: my-ci-linux` (setup opens a PR per repo for you). `runs-on: my-ci` also works for Linux.

More machines: run `sudo runner-warden host add` once per machine. It asks what kind of machine it is and how to reach it, shows the one step to run there (trusting this instance's SSH key), checks it, offers to fix what's missing (Docker Engine in WSL, a WSL keep-alive task) and restarts the service.

`runner-warden` is a single binary in `/usr/local/bin`: `runner-warden --help` lists every command, and `sudo runner-warden update` keeps it current. `setup` also enables `runner-warden-update.timer`, which runs `update` daily around 04:00–06:00 (`setup --auto-update=false` turns it off).

Needs: Linux with systemd and cgroup v2, Docker 25+, `gh` logged in as a user with `repo` (+ `admin:org` for orgs, `workflow` for the label sweep).

## Commands

| Command | Does |
|---|---|
| `setup` | Install or update an instance (idempotent; `--dry-run` shows the plan) |
| `status [--once\|--json]` | Live view of lanes, held/running jobs, memory and runners (`--once` / `--json` for a snapshot) |
| `check` | Health check of service, slice, Docker, disk, daemon |
| `logs [-f] [runner\|run-id]` | Service logs, filtered |
| `drain [--off]` | Stop taking new jobs; running jobs finish |
| `kill <runner>` | Cancel a runner's job and stop it |
| `migrate apply\|retire\|rollback` | Label sweep and retirement of old runners |
| `migrate check [--save]` | Re-scan for jobs still on old labels (e.g. branches older than the sweep); exit 3 if any, so it can run from cron or CI |
| `windows provision\|adopt <vm>` | Create a Windows VM, or take over an existing dockurr disk |
| `host add` | Wizard: link another machine (Linux, WSL on a Windows PC, Windows, Mac with tart) over SSH, check and fix it, add it to the config |
| `host list` | Linked machines and whether they are reachable |
| `windows host-script\|host-setup <host>` | Prepare a separate Windows machine as a host by hand |
| `config validate` | Validate the config |
| `update [--check]` | Update to the latest release (checksum + signature verified), then restart instances; running jobs survive |
| `uninstall [--purge]` | Stop the service, delete its scale sets, containers, VM containers and slice; `--purge` also removes config, token and state |

## Configuration

`/etc/runner-warden/<instance>/config.toml` — see [docs/configuration.md](docs/configuration.md).

## How scheduling works

Each repo/org and OS gets several "lane" scale sets sharing one label. Each lane holds at most one job that hasn't started yet, so the manager always knows which job the next runner will get. A job that fits the budget gets a runner immediately; one that doesn't goes to a free remote host if one serves it, or stays queued on GitHub without blocking the other lanes. Remote hosts reserve nothing on this host. Details: [PLAN.md](PLAN.md) and [spike/M0-FINDINGS.md](spike/M0-FINDINGS.md).

## Status

Pre-1.0. Linux (local and remote hosts), Windows (local VMs and remote hosts) and macOS (tart on remote Macs).

Licensed under [Apache-2.0](LICENSE).
