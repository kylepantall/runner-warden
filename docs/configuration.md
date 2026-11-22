# Configuration

`/etc/runner-warden/<instance>/config.toml`. `setup` writes it; edit and `systemctl restart runner-warden@<instance>`. Check with `runner-warden config validate`.

```toml
prefix = "my-ci"                 # required, no default

[limits]
memory_budget = "22GiB"          # hard cap for all jobs (slice MemoryMax)
swap_max = "2GiB"
reserved_cores = "0-1"           # never pinned to jobs
default_job_mem = "2GiB"         # cost of jobs no rule matches
cpu_budget = 0                   # logical CPUs jobs share; 0 = all outside reserved_cores, -1 = unchecked
default_job_cpus = 2             # CPU cost of jobs whose rule sets none
light_threshold = "1GiB"         # jobs at or below may overtake held jobs
lanes = 4                        # scale sets per repo/org and OS
admission_timeout = "40m"
external_reserved = "0B"         # memory used by things outside runner-warden

[linux]
enabled = true
image = "ghcr.io/kylepantall/runner-warden-runner:latest"  # runner + docker compose, gh, make, terraform, php, ...
runner_uid = 1001
tmpfs_size = "8GiB"              # /tmp in runner containers
persistent_names = ["my-device-*"]  # containers jobs create with a matching name outlive the runner
allowed_images = ["ghcr.io/my-org/*"]  # images repos may pick in .github/runner-warden.yml; empty = off

[[linux.build]]                  # a runner image runner-warden builds: linux.image + apt packages
name = "desktop"
packages = ["clang", "cmake", "ninja-build", "pkg-config", "libgtk-3-dev"]

[[cache]]                        # shared host dirs, mounted into runners and job containers
name = "pub"
host_path = "/opt/ci-cache/flutter-pub"
env = "PUB_CACHE"                # exported pointing at container_path
uid = 1001
dir_mode = "2775"

[[cost]]                         # owner/repo/workflow-file/job, path.Match per segment
match = "my-org/app/ci.yml/maestro*"
mem = "15GiB"
pinned_cores = 3                 # optional: whole physical cores, SMT siblings together
cpus = 4                         # optional: shared logical CPUs (ignored with pinned_cores)
devices = ["/dev/kvm"]           # optional: host devices for matching jobs
image = "ghcr.io/my-org/android-ci:1"  # optional: runner image for matching jobs (beats the repo file), or a [[linux.build]] name
local = true                     # optional: never on a [[linux.host]] (devices/pinned cores imply it)
machine = "pc-wsl"               # optional: only on this machine (any host or vm name, or "local"), waiting while it is busy

[hang]
idle = "30m"                     # no CPU, IO or job output
max_duration = "6h"
rerun_on_runner_loss = true

[stale]
cancel_closed_prs = true
cancel_deleted_branches = true

[image_gc]
high_watermark_percent = 85
unused_for = "24h"

[[target]]
org = "my-org"                   # org-level runners (needs admin:org)
runner_group = "Default"
os = ["linux", "windows"]

[[target]]
repo = "me/tool"                 # repo-level runners
weight = 2                       # share under contention
max = 0                          # 0 = unlimited

[windows]
enabled = true
max_concurrent = 1               # number of Windows licences
idle_ttl = "15m"                 # stop idle VMs

[[windows.vm]]
name = "build"
mem = "8GiB"
cpus = 8
disk_size = "96G"
ssh_port = 2222                  # bound to 127.0.0.1 only
targets = ["my-org", "me/tool"]  # empty = any Windows target

[[linux.host]]                   # another Linux machine (or WSL distro) for overflow jobs
name = "pc-wsl"
address = "pc.example.ts.net"    # host or host:port (default 22)
ssh_user = "me"                  # default root; key: linux.ssh_key_file (default windows.ssh_key_file)
exec = "wsl -d Ubuntu -u root --"  # runs commands as root there; default "sudo -n"
max_concurrent = 2               # jobs at once
targets = ["my-org"]

[macos]                          # macOS jobs, in tart VMs on separate Macs
enabled = true

[[macos.host]]
name = "mac"
address = "mac.example.ts.net"
ssh_user = "me"                  # owns the tart VM; key: macos.ssh_key_file (default windows.ssh_key_file)
vm = "runner-xcode"              # a running tart VM with the guest agent
targets = ["my-org"]             # add "macos" to the target's os list too

[[windows.package]]              # kept installed on every Windows VM and host (winget)
id = "Microsoft.VisualStudio.2022.BuildTools"
override = "--quiet --wait --norestart --add Microsoft.VisualStudio.Workload.VCTools --includeRecommended"

[[windows.host]]                 # a separate Windows machine, driven over SSH
name = "pc"
address = "pc.example.ts.net"    # host or host:port (default 22)
ssh_user = "me"                  # an administrator; default windows.ssh_user
task_logon = "S4U"               # default S4U: runs whether or not anyone is signed in
max_concurrent = 2               # jobs at once (default 1), each with its own runner install and TEMP
targets = ["my-org"]
```

## Windows hosts

A `[[windows.host]]` runs Windows jobs on another machine, like a VM that is always on: one at a time, or `max_concurrent` at once.

| Aspect | Behaviour |
|---|---|
| Setup | `runner-warden windows host-script <host>` prints PowerShell to run once, elevated, on the machine (OpenSSH Server + runner-warden's key). Then `runner-warden windows host-setup <host> [--script tools.ps1]` installs the baseline and runner files |
| Routing | Free local VMs first, then hosts. A host that does not answer over SSH is skipped and re-checked every minute |
| Budget | Uses none of this host's memory or CPU and no `max_concurrent` licence |
| Per job | Ephemeral JIT runner, work dir and its own TEMP wiped, tool cache kept at `<work_dir>\_tool`. With `max_concurrent` > 1 each slot has its own runner install (`<runner_dir>-s<n>`); jobs never sweep each other, and the signed-in user's TEMP is never touched |
| Limits | The machine must stay on and reachable; runner-warden cannot restart it. Hang detection reads whole-machine CPU and the runner logs |

## Linking machines

`sudo runner-warden host add` writes the `[[linux.host]]`, `[[windows.host]]` or `[[macos.host]]` entry for you (keeping a backup and your comments), enables the section and adds the OS to the targets it serves.

| Kind | You do on the machine | The wizard checks / fixes |
|---|---|---|
| `linux` | Add the shown key to `~/.ssh/authorized_keys` (root, or a user with passwordless sudo) | root, systemd, Docker Engine (installs `docker.io` on apt distros) |
| `wsl` | Run the shown PowerShell once, elevated (OpenSSH Server + key) | picks the distro; root, systemd, Docker Engine (installs it), a keep-alive startup task (adds it) |
| `windows` | Run the shown PowerShell once, elevated | installs the baseline and runner files over SSH |
| `macos` | Turn on Remote Login, add the shown key | picks a running tart VM; guest agent, Xcode, guest uid |

Every question has a flag (`--kind --address --user --name --targets --distro --vm --jobs`); `--yes` takes defaults and accepts fixes, `--no-restart` leaves the restart to you. `runner-warden host list` shows linked machines.

## Linux hosts

A `[[linux.host]]` takes Linux jobs this host cannot fit. It runs the same Docker backend, driven over SSH; the daemon keeps the scale sets and the scheduler.

| Aspect | Behaviour |
|---|---|
| Needs | SSH to the machine, Docker Engine and systemd there, root through `exec`. runner-warden installs its own binary at `binary` (default `/usr/local/bin/runner-warden`) and replaces it after every update |
| Routing | This host first. A job goes to a host only when this host's memory or CPU budget (or `max_runners`) cannot fit it, a reachable host serving its target has a free slot, and its rule is not `local` |
| Budget | Host jobs reserve none of this host's memory or CPU; `max_concurrent` caps them. The host's own memory is not checked |
| Per job | Ephemeral JIT runner with its Docker proxy, caches (`[[cache]]` paths, created there) and per-job image (falls back to the default when the host cannot pull it). No job hooks, no persistent containers |
| Outages | Probed every minute. A host that stops answering takes no new jobs; its runners stay tracked for 15 minutes, then count as gone |
| WSL | Behind Windows OpenSSH use `exec = "wsl -d <distro> -u root --"`. Install Docker Engine inside the distro (`apt install docker.io`, systemd on in `/etc/wsl.conf`), not Docker Desktop. WSL stops a distro with no session attached: keep one, e.g. a startup task running `wsl.exe -d <distro> -u root -- sleep infinity` |

## macOS hosts

A `[[macos.host]]` runs macOS jobs in a tart VM on another Mac, one at a time. Workflows use `runs-on: <prefix>-macos`.

| Aspect | Behaviour |
|---|---|
| Needs | Remote Login on the Mac with runner-warden's public key, tart, and a VM that stays running (e.g. a launchd agent running `tart run <vm> --no-graphics` with `KeepAlive`). The VM needs the tart guest agent (all Cirrus Labs images have it) |
| Per job | A fresh JIT runner, cloned from a template install in the guest (`~/rw/<instance>/template`, kept at `runner_version`), started as a launchd job in the guest user's GUI session so the login keychain works. Its dir and work dir are removed afterwards; the tool cache (`RUNNER_TOOL_CACHE`, `~/rw/<instance>/_tool`) and caches elsewhere in the guest stay |
| Clock | The guest clock is synced when it drifts over 30s (a sleeping Mac leaves it behind, and the runner's token then looks expired) |
| Budget | Uses none of this host's memory or CPU. Waiting macOS jobs never hold back jobs here |
| Outages | Probed every minute; a sleeping or unreachable Mac takes no jobs. runner-warden does not start the VM |

## Sizing jobs

| Situation | Do |
|---|---|
| A job needs lots of memory (emulators, big builds) | Add a `[[cost]]` rule. Until it fits, the job waits in GitHub's queue, its timeout not ticking |
| CPU-heavy jobs (Gradle, compilers, VMs) | `cpus = N` on their rule. A job starts only when its memory and CPU both fit; pinned cores' threads count against `cpu_budget` |
| Timing-sensitive tests | `pinned_cores = N` on their rule |
| A job must run on one machine (a device attached to it, its toolchain, testing it) | `machine = "<name>"` on its rule. It waits for that machine instead of running elsewhere; `"local"` means this host |
| Small, fast jobs | Leave them at or below `light_threshold` so they overtake held heavy jobs |

The slice's `MemoryMax` is the backstop: a job that exceeds its estimate is OOM-killed (reported in `status` and logs) instead of the host freezing.

## Build tools

Declare what jobs need; runner-warden installs it, so new machines need nothing by hand.

| OS | Declare | runner-warden does |
|---|---|---|
| Linux | `[[linux.build]] name + packages`, used by `[[cost]] image = "<name>"` | Builds `FROM linux.image` with the apt packages on first use, on this host and on each `[[linux.host]]`. The tag hashes the base and packages, so an edit means a fresh build and an unchanged one is never rebuilt |
| Windows | `[[windows.package]] id + override` | Installs, with winget, each package a VM or host doesn't list: every 2 minutes for members that are up, and before a job on a member not yet checked. `override` (installer arguments) only applies at first install |

## Per-project runner images

Keep the default image lean; projects that need more ship their own image built `FROM` it.

```dockerfile
FROM ghcr.io/kylepantall/runner-warden-runner:v0.1.4
USER root
RUN apt-get update && apt-get install -y --no-install-recommends openjdk-17-jdk && rm -rf /var/lib/apt/lists/*
```

The repo picks it in `.github/runner-warden.yml`:

```yaml
image: ghcr.io/my-org/app-ci:1        # every job in this repo
jobs:                                  # first match wins; workflow-file/job-name globs
  - match: ci.yml/android*
    image: ghcr.io/my-org/android-ci:1
```

| Rule | Detail |
|---|---|
| Precedence | `[[cost]] image` → repo file → `linux.image` |
| Allowlist | Repo images are used only if they match `linux.allowed_images` (`*` stops at `/`); empty list = repo file ignored |
| Which file | Read from the workflow's ref (a PR can change it), cached 1 min; reusable workflows from another repo read the default branch |
| Pulls | Anonymous, when missing or last pulled over 1h ago. Same ref = one local copy; images sharing a base share its layers. For private registries, `docker pull` on the host first (a local copy is used when a pull fails) |
| Failure | Missing, invalid or disallowed file, or an image that cannot be pulled → default image, logged |
| Base | The image must keep the runner, `setpriv` and uid 1001 from the base |

⚠️ The image follows the job the runner was started for. If GitHub hands that runner a different job (rare), it runs in the first job's image.

## Persistent containers

`linux.persistent_names` opts container names in (`path.Match` globs). A job container created through the proxy with a matching `--name`:

| Aspect | Behaviour |
|---|---|
| Ownership | Labelled `role=persistent`, no runner label: any runner of this instance can list, exec, inspect, `cp`, `logs` and `rm` it. Other instances and non-matching names are unaffected |
| Cleanup | Runner cleanup, reconcile and restore never remove it. `uninstall` does |
| Memory | Runs in the job slice, outside the ledger. Its `memory.current` is added to `external_reserved` every 15s and shown in `status` |
