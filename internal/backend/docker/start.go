package docker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

const hookDir = "rw-hooks"

// entrypoint copies the runner install into the per-runner dir (which exists at
// the same path on the host, so job containers can bind-mount _work/externals
// by host path) and starts it. cp -RP, not -a: ownership is set by the runner uid.
//
// The JIT config arrives as a 0400 file in that dir (so it never shows in
// `docker inspect`), is read into the environment and deleted first thing.
const entrypoint = `set -e; J="$1/.jit"; ACTIONS_RUNNER_INPUT_JITCONFIG=$(cat "$J"); rm -f "$J"; export ACTIONS_RUNNER_INPUT_JITCONFIG; cp -RP /home/runner/. "$1"; cd "$1"; exec ./run.sh`

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) } //nolint:gosec // runner dirs must be traversable by the runner uid

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// hookWrapper is the script the runner executes for a job hook.
func hookWrapper(binary, event string) string {
	return "#!/bin/sh\nexec " + shQuote(binary) + " hook " + event + "\n"
}

func bind(host, ctr string, ro bool) string {
	s := host + ":" + ctr
	if ro {
		s += ":ro"
	}
	return s
}

func sortedEnv(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+m[k])
	}
	return out
}

// parseDevice accepts "/dev/kvm" or "host:container[:perms]".
func parseDevice(d string) container.DeviceMapping {
	parts := strings.Split(d, ":")
	m := container.DeviceMapping{PathOnHost: parts[0], PathInContainer: parts[0], CgroupPermissions: "rwm"}
	if len(parts) > 1 && parts[1] != "" {
		m.PathInContainer = parts[1]
	}
	if len(parts) > 2 && parts[2] != "" {
		m.CgroupPermissions = parts[2]
	}
	return m
}

// buildRunnerContainer is the pure part of Start: the container spec.
func (b *Backend) buildRunnerContainer(spec core.RunnerSpec, dir, sock string, gid int) (*container.Config, *container.HostConfig) {
	c := b.cfg
	img := c.Image
	if spec.Image != "" {
		img = spec.Image
	}
	started := filepath.Join(dir, hookDir, "job-started.sh")
	completed := filepath.Join(dir, hookDir, "job-completed.sh")

	env := map[string]string{}
	for k, v := range c.CacheEnv {
		env[k] = v
	}
	for k, v := range spec.Env {
		env[k] = v
	}
	env["DOCKER_HOST"] = "unix://" + sock
	binds := []string{bind(dir, dir, false)}
	if !c.NoHooks {
		env["ACTIONS_RUNNER_HOOK_JOB_STARTED"] = started
		env["ACTIONS_RUNNER_HOOK_JOB_COMPLETED"] = completed
		env["RUNNER_WARDEN_SOCKET"] = filepath.Join(b.hookDir(spec.Name), "hook.sock")
		env["RUNNER_WARDEN_RUNNER"] = spec.Name
		binds = append(binds,
			bind(c.HookBinary, c.HookBinary, true),
			bind(b.hookDir(spec.Name), b.hookDir(spec.Name), false))
	}
	for _, m := range spec.Mounts {
		binds = append(binds, bind(m.HostPath, m.ContainerPath, m.ReadOnly))
	}
	binds = append(binds, c.CacheBinds...)

	// PID 1 (docker-init) runs as root, like init on a host or VM; the runner
	// itself drops to the runner user. Tests that probe /proc/1 or other
	// root-owned state then behave as they do on a host runner.
	uid := "1001"
	if c.RunnerUID > 0 {
		uid = strconv.Itoa(c.RunnerUID)
	}
	env["HOME"] = "/home/runner"
	env["USER"] = "runner"
	// Docker's tmpfs defaults to noexec; jobs routinely run scripts and stub
	// binaries from /tmp, as they could on a host runner.
	tmpfs := "exec,mode=1777"
	if c.TmpfsSize > 0 {
		tmpfs = "size=" + strconv.FormatInt(c.TmpfsSize, 10) + "," + tmpfs
	}
	var devices []container.DeviceMapping
	for _, d := range spec.Devices {
		devices = append(devices, parseDevice(d))
	}
	init := true
	hc := &container.HostConfig{
		Binds:       binds,
		OomScoreAdj: c.OOMScoreAdj,
		Tmpfs:       map[string]string{"/tmp": tmpfs},
		Init:        &init,
		Resources:   container.Resources{CgroupParent: c.Slice, CpusetCpus: spec.CPUSet, Devices: devices},
	}
	if gid > 0 {
		hc.GroupAdd = []string{strconv.Itoa(gid)}
	}
	cfg := &container.Config{
		Image:      img,
		User:       "0",
		Env:        sortedEnv(env),
		WorkingDir: dir,
		Entrypoint: []string{"/usr/bin/setpriv", "--reuid=" + uid, "--regid=" + uid, "--init-groups",
			"/bin/sh", "-c", entrypoint, "runner-warden", dir},
		Labels: map[string]string{
			naming.MetaManaged:  "1",
			naming.MetaInstance: c.Instance,
			naming.MetaHostID:   c.HostID,
			naming.MetaRunner:   spec.Name,
			naming.MetaRole:     "runner",
		},
	}
	return cfg, hc
}

// prepareDir creates the runner dir (same path inside the container) and the
// hook wrappers.
func (b *Backend) prepareDir(dir string) error {
	if err := mkdirAll(dir); err != nil {
		return err
	}
	if err := os.Chown(dir, b.runnerOwner(), -1); err != nil {
		// Not root (tests, rootless): make the dir usable by any uid instead.
		b.log.Warn("runner dir chown failed, widening mode", "dir", dir, "error", err.Error())
		if err := os.Chmod(dir, 0o777); err != nil { //nolint:gosec // fallback when not root: the runner uid must be able to write
			return err
		}
	}
	hd := filepath.Join(dir, hookDir)
	if err := mkdirAll(hd); err != nil {
		return err
	}
	for event, file := range map[string]string{"job-started": "job-started.sh", "job-completed": "job-completed.sh"} {
		if err := os.WriteFile(filepath.Join(hd, file), []byte(hookWrapper(b.cfg.HookBinary, event)), 0o755); err != nil { //nolint:gosec // hook wrappers must be executable by the runner
			return err
		}
	}
	return nil
}

// writeJIT leaves the JIT config for the entrypoint: readable by the runner
// uid only, and removed by the entrypoint as soon as it is read.
func (b *Backend) writeJIT(dir, jit string) error {
	p := filepath.Join(dir, ".jit")
	if err := os.WriteFile(p, []byte(jit), 0o400); err != nil {
		return err
	}
	if err := os.Chown(p, b.runnerOwner(), -1); err != nil {
		// Not root (tests, rootless): the dir is world-usable then, so is this.
		return os.Chmod(p, 0o444) //nolint:gosec // read-only for everyone is the intent when chown is unavailable
	}
	return nil
}

func (b *Backend) runnerOwner() int {
	if b.cfg.RunnerUID > 0 {
		return b.cfg.RunnerUID
	}
	return 1001 // default uid of the runner user in the upstream image
}

// Start creates the runner dir and proxy, then creates and starts the runner.
func (b *Backend) Start(ctx context.Context, spec core.RunnerSpec) (core.Handle, error) {
	if spec.Name == "" || spec.JITConfig == "" {
		return core.Handle{}, errors.New("runner spec needs a name and JIT config")
	}
	if !b.validName(spec.Name) {
		return core.Handle{}, errors.New("invalid runner name")
	}
	// The daemon creates the runner's private hook dir first; a missing one
	// would be created by Docker as a root-owned 0755 dir.
	if _, err := os.Stat(b.hookDir(spec.Name)); err != nil && !b.cfg.NoHooks {
		return core.Handle{}, fmt.Errorf("runner hook dir: %w", err)
	}
	dir, sock := b.runnerDir(spec.Name), b.proxySock(spec.Name)
	if err := b.prepareDir(dir); err != nil {
		return core.Handle{}, err
	}
	gid, err := b.statGID(b.cfg.DockerSocket)
	if err != nil {
		b.log.Warn("docker socket gid unknown", "socket", b.cfg.DockerSocket, "error", err.Error())
		gid = 0
	}
	if err := b.writeJIT(dir, spec.JITConfig); err != nil {
		_ = b.Cleanup(ctx, spec.Name)
		return core.Handle{}, err
	}
	cfg, hc := b.buildRunnerContainer(spec, dir, sock, gid)
	if err := b.startProxy(ctx, spec.Name, sock, b.allowedFromBinds(spec.Name, hc.Binds)...); err != nil {
		_ = b.Cleanup(ctx, spec.Name)
		return core.Handle{}, err
	}
	created, err := b.api.ContainerCreate(ctx, cfg, hc, &network.NetworkingConfig{}, nil, spec.Name)
	if err == nil {
		err = b.api.ContainerStart(ctx, created.ID, container.StartOptions{})
	}
	if err != nil {
		_ = b.Cleanup(context.WithoutCancel(ctx), spec.Name)
		return core.Handle{}, fmt.Errorf("start runner: %w", err)
	}
	return core.Handle{RunnerName: spec.Name, BackendID: created.ID, StartedAt: time.Now()}, nil
}
