# M0 spike findings (2026-11-20)

Spike code: [`spike/m0`](m0/) — throwaway listener on `actions/scaleset` v0.5.0, one ephemeral runner container per job (`ghcr.io/actions/actions-runner`, runner 2.338.2), optional per-runner Docker socket proxy. Run on an Ubuntu 26.04 host (cgroup v2, systemd driver, Docker 29.9) against a private sandbox repo and one org.

## Results

| Question | Result |
|---|---|
| `gh auth token` (OAuth) with scale sets | ✅ Works at repo level and org level (create, session, JIT, delete) |
| Runner name limit | ✅ 64 chars; 65 rejected. Forbidden: `" / : < > \ \| * ?` |
| Scale set name limit | ✅ 64 chars; 65 rejected. Same forbidden set |
| Demand latency | ✅ Workflow dispatch → runner container started in ≈2 s |
| `JobAvailable` + `AcquireJobs` | ❌ Never delivered. Jobs arrive as `JobAssigned` (server-side assignment). **The manager cannot refuse or pick jobs** |
| Job identity before runner start | ✅ `JobAssigned` carries repo, owner, workflow ref, job display name, run id, labels |
| Which runner gets which job | ❌ Unknown until `JobStarted` (any idle runner in the set) |
| Job-started hook ordering | ✅ Runs before job containers / services are created; has `GITHUB_*` env (job, run, repo, runner) |
| Hook blocking vs `timeout-minutes` | ⚠️ Counts. A job with `timeout-minutes: 1` was cancelled while the hook waited 90 s |
| `container:` + `services:` via host Docker | ✅ Works **only if** the runner dir exists at the same path on host and in the runner container (job containers bind-mount `_work` and `externals` by host path) |
| Runner mounts `/var/run/docker.sock` into job containers | ⚠️ Yes — by host path, so jobs get the **real** socket unless the proxy rewrites that bind. Proxy rewrite verified |
| Socket proxy pass-through | ✅ `httputil.ReverseProxy` handles attach/exec hijack, image build, volumes, networks, nested `docker run` |
| Proxy rewrite of create calls | ✅ Labels + `CgroupParent` + `OomScoreAdj` stamped on every container/network/volume; containers landed in the configured slice (systemd created it on demand) |
| Label-based cleanup | ✅ Leaked container (`sleep 600`) and named volume removed at job completion by label only |
| BuildKit build steps | ⚠️ Not created via `containers/create` → not labelled or sliced by the proxy. Need daemon-level `cgroup-parent` default, or build-time handling |
| Cleanup permissions | ⚠️ Runner dirs end up owned by the job uid; manager cleanup must run as root or via a helper container |
| Org runner groups | ℹ️ Orgs may have several groups; scale sets need an explicit group id (default = 1) |

## Job-selection spike (E1–E9)

| # | Experiment | Result |
|---|---|---|
| E1 | `maxCapacity=1`, 4 jobs queued, no runners | ✅ Only 1 job assigned; the rest stay queued on GitHub, unannounced |
| E2 | `maxCapacity=0` | ✅ Nothing assigned to that scale set |
| E3 | Pick a non-head job via `AcquireJobs` | ❌ Impossible: `JobAvailable` is never delivered |
| E4 | Raise capacity live | ✅ Next job assigned FIFO, effective at the next poll (≤ 50 s) |
| E5 | Runners started one by one | ✅ Each runner receives the oldest assigned-unstarted job (strict FIFO) |
| E6 | Assigned job with no runner | ✅ Waiting in the queue does **not** count toward `timeout-minutes` (1-min-timeout job waited ≈4 min, passed). ⚠️ Every 5 min: `JobCompleted{canceled, runner ""}` + `JobAssigned` again (reassign churn); held ≥ 15 min with no failure |
| E7a | Runner killed 5 s after start | It was already bound: offline + busy, `DELETE` → 422. After 5 min GitHub rebinds; job then succeeds on another runner |
| E7b | Runner killed mid-job | Job cancelled by GitHub after ≈14 min, not retried |
| E8 | Two scale sets sharing one label | ✅ Allowed (extra label). Job routed to the set with capacity |
| E9 | Message vs REST identity | Messages carry `workflowRunId`, `eventName`, job name, labels; `JobID` is a GUID → match REST by run id + name |

## Plan changes

| Area | Change |
|---|---|
| Admission | Stays at the **job-started hook** (accept-time admission is impossible). Pre-warm: use `JobAssigned` identities to predict demand by cost. Document that admission wait counts toward `timeout-minutes`; setup's report flags jobs with tight timeouts |
| Docker backend | Runner dir at identical host/container path is mandatory. Proxy must rewrite the `docker.sock` bind. Set dockerd default `cgroup-parent` to the instance slice so BuildKit work is also capped |
| Cleanup | Root-owned async cleanup worker (or helper container) |
| Setup | Ask for / create the runner group at org level |
| Windows | Evaluate a Windows-in-Docker image (QEMU/KVM inside a container) as the Windows backend before building raw libvirt: same Docker lifecycle, labels and slice |
| Unverified → verified | Name limits, `gh` token auth, hook ordering, timeout behaviour |
