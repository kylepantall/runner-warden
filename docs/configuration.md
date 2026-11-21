# Configuration

`/etc/runner-warden/<instance>/config.toml`. `setup` writes it; edit and `systemctl restart runner-warden@<instance>`. Check with `runner-warden config validate`.

```toml
prefix = "my-ci"                 # required, no default

[limits]
memory_budget = "22GiB"          # hard cap for all jobs (slice MemoryMax)
swap_max = "2GiB"
reserved_cores = "0-1"           # never pinned to jobs
default_job_mem = "2GiB"         # cost of jobs no rule matches
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
devices = ["/dev/kvm"]           # optional: host devices for matching jobs
image = "ghcr.io/my-org/android-ci:1"  # optional: runner image for matching jobs (beats the repo file)

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
```

## Sizing jobs

| Situation | Do |
|---|---|
| A job needs lots of memory (emulators, big builds) | Add a `[[cost]]` rule. Until it fits, the job waits in GitHub's queue, its timeout not ticking |
| Timing-sensitive tests | `pinned_cores = N` on their rule |
| Small, fast jobs | Leave them at or below `light_threshold` so they overtake held heavy jobs |

The slice's `MemoryMax` is the backstop: a job that exceeds its estimate is OOM-killed (reported in `status` and logs) instead of the host freezing.

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
