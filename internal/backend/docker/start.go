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
	"github.com/kylepantall/runner-warden/internal/dockerproxy"
	"github.com/kylepantall/runner-warden/internal/naming"
)

const hookDir = "rw-hooks"

// entrypoint copies the runner install into the per-runner dir (which exists at
// the same path on the host, so job containers can bind-mount _work/externals
// by host path) and starts it. cp -RP, not -a: ownership is set by the runner uid.
const entrypoint = `set -e; cp -RP /home/runner/. "$1"; cd "$1"; exec ./run.sh`

func mkdirAll(p string) error { return os.MkdirAll(p, 0o755) }

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

func envKey(e string) string { k, _, _ := strings.Cut(e, "="); return k }

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
	env["ACTIONS_RUNNER_INPUT_JITCONFIG"] = spec.JITConfig
	env["ACTIONS_RUNNER_HOOK_JOB_STARTED"] = started
	env["ACTIONS_RUNNER_HOOK_JOB_COMPLETED"] = completed
	env["RUNNER_WARDEN_SOCKET"] = c.HookSocket
	env["RUNNER_WARDEN_RUNNER"] = spec.Name

	binds := []string{
		bind(sock, "/var/run/docker.sock", false),
		bind(dir, dir, false),
		bind(c.HookBinary, c.HookBinary, true),
		bind(c.HookSocket, c.HookSocket, true),
	}
	for _, m := range spec.Mounts {
		binds = append(binds, bind(m.HostPath, m.ContainerPath, m.ReadOnly))
	}
	binds = append(binds, c.CacheBinds...)

	user := "runner"
	if c.RunnerUID > 0 {
		user = strconv.Itoa(c.RunnerUID)
	}
	tmpfs := "mode=1777"
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
		User:       user,
		Env:        sortedEnv(env),
		WorkingDir: dir,
		Entrypoint: []string{"/bin/sh", "-c", entrypoint, "runner-warden", dir},
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
		if err := os.Chmod(dir, 0o777); err != nil {
			return err
		}
	}
	hd := filepath.Join(dir, hookDir)
	if err := mkdirAll(hd); err != nil {
		return err
	}
	for event, file := range map[string]string{"job-started": "job-started.sh", "job-completed": "job-completed.sh"} {
		if err := os.WriteFile(filepath.Join(hd, file), []byte(hookWrapper(b.cfg.HookBinary, event)), 0o755); err != nil {
			return err
		}
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
	dir, sock := b.runnerDir(spec.Name), b.proxySock(spec.Name)
	if err := b.prepareDir(dir); err != nil {
		return core.Handle{}, err
	}
	gid, err := b.statGID(b.cfg.DockerSocket)
	if err != nil {
		b.log.Warn("docker socket gid unknown", "socket", b.cfg.DockerSocket, "error", err.Error())
		gid = 0
	}
	if err := b.startProxy(spec.Name, sock); err != nil {
		_ = b.Cleanup(ctx, spec.Name)
		return core.Handle{}, err
	}
	cfg, hc := b.buildRunnerContainer(spec, dir, sock, gid)
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

func (b *Backend) startProxy(runner, sock string) error {
	if err := mkdirAll(b.runnersDir()); err != nil {
		return err
	}
	p, err := dockerproxy.New(dockerproxy.Config{
		Socket: sock, Upstream: b.cfg.DockerSocket,
		Runner: runner, Instance: b.cfg.Instance, HostID: b.cfg.HostID,
		Slice: b.cfg.Slice, OOMScoreAdj: b.cfg.OOMScoreAdj,
		CacheBinds: b.cfg.CacheBinds, CacheEnv: b.cfg.CacheEnv,
		Log: b.log,
	})
	if err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if old := b.proxies[runner]; old != nil {
		_ = old.Close()
	}
	for i := 0; i < b.paused; i++ {
		p.PausePulls()
	}
	b.proxies[runner] = p
	return nil
}
