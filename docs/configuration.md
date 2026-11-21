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
image = "ghcr.io/actions/actions-runner:latest"
runner_uid = 1001
tmpfs_size = "8GiB"              # /tmp in runner containers
persistent_names = ["my-device-*"]  # containers jobs create with a matching name outlive the runner

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

## Persistent containers

`linux.persistent_names` opts container names in (`path.Match` globs). A job container created through the proxy with a matching `--name`:

| Aspect | Behaviour |
|---|---|
| Ownership | Labelled `role=persistent`, no runner label: any runner of this instance can list, exec, inspect, `cp`, `logs` and `rm` it. Other instances and non-matching names are unaffected |
| Cleanup | Runner cleanup, reconcile and restore never remove it. `uninstall` does |
| Memory | Runs in the job slice, outside the ledger. Its `memory.current` is added to `external_reserved` every 15s and shown in `status` |
