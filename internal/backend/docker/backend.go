// Package docker is the Linux core.Backend: one ephemeral runner per container
// on the host Docker daemon, with a per-runner Docker API proxy so the jobs the
// runner starts are labelled, capped by the instance slice and cleaned up.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/dockerproxy"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// Config configures the backend.
type Config struct {
	Instance string
	Prefix   string // runner name prefix; names are validated against it
	HostID   string
	StateDir string // runner dirs and proxy sockets live in <StateDir>/runners
	Image    string // default runner image
	// DockerSocket is the real Docker socket (default /var/run/docker.sock).
	DockerSocket string
	Slice        string // systemd slice, e.g. naming.Slice(instance)
	RunnerUID    int    // 0 = image user "runner"
	TmpfsSize    int64  // /tmp size in bytes; 0 = no size cap
	OOMScoreAdj  int

	// HookBinary is mounted read-only into every runner at the same path.
	// HookDir is the base of the per-runner hook dirs, <HookDir>/<runner>,
	// each holding only that runner's hook.sock. The daemon creates the dir
	// before Start; only that one dir is mounted (read-write, at the same
	// path, so a recreated socket stays visible).
	HookBinary string
	HookDir    string

	// CacheBinds ("host:container[:opts]") and CacheEnv go to runner and jobs.
	CacheBinds []string
	CacheEnv   map[string]string

	// AllowedBinds are extra host paths jobs may bind-mount (besides the
	// runner dir, cache paths and the runner's own mounts).
	AllowedBinds []string

	// PersistentNames are path.Match globs on container names that jobs may
	// create as persistent containers (see dockerproxy.Config).
	PersistentNames []string

	// LogLevel is the slog level the out-of-process proxies log at ("" = info).
	LogLevel string

	Log *slog.Logger
}

// dockerAPI is the subset of the Docker client the backend uses.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, cfg *container.Config, hc *container.HostConfig, nc *network.NetworkingConfig, p *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerStop(ctx context.Context, id string, opts container.StopOptions) error
	ContainerKill(ctx context.Context, id, signal string) error
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
	ContainerWait(ctx context.Context, id string, cond container.WaitCondition) (<-chan container.WaitResponse, <-chan error)
	ContainerStatsOneShot(ctx context.Context, id string) (container.StatsResponseReader, error)
	NetworkList(ctx context.Context, opts network.ListOptions) ([]network.Summary, error)
	NetworkRemove(ctx context.Context, id string) error
	VolumeList(ctx context.Context, opts volume.ListOptions) (volume.ListResponse, error)
	VolumeRemove(ctx context.Context, id string, force bool) error
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
	ImageInspect(ctx context.Context, id string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
}

// Backend implements core.Backend.
type Backend struct {
	cfg Config
	log *slog.Logger
	api dockerAPI

	cgroupRoot string // /sys/fs/cgroup; injectable
	statGID    func(path string) (int, error)

	mu      sync.Mutex
	proxies map[string]*dockerproxy.Proxy
	paused  int
	stats   map[string]*runnerStats
	cgPaths map[string]string

	// launcher runs each runner's proxy as its own process, outside the
	// daemon, so a daemon restart does not cut running jobs. nil = serve the
	// proxies in-process (no systemd, tests).
	launcher proxyLauncher
}

var _ core.Backend = (*Backend)(nil)

// New connects to the Docker daemon at cfg.DockerSocket.
func New(cfg Config) (*Backend, error) {
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = "/var/run/docker.sock"
	}
	cli, err := client.NewClientWithOpts(client.WithHost("unix://"+cfg.DockerSocket), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	b, err := newBackend(cfg, cli)
	if err != nil {
		return nil, err
	}
	if systemdAvailable() {
		b.launcher = systemdLauncher{exe: cfg.HookBinary}
	}
	return b, nil
}

func newBackend(cfg Config, api dockerAPI) (*Backend, error) {
	switch {
	case cfg.Instance == "" || cfg.Prefix == "" || cfg.HostID == "" || cfg.StateDir == "" || cfg.Image == "" || cfg.Slice == "":
		return nil, errors.New("docker backend: instance, prefix, host id, state dir, image and slice are required")
	case cfg.HookBinary == "" || cfg.HookDir == "":
		return nil, errors.New("docker backend: hook binary and hook dir are required")
	}
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = "/var/run/docker.sock"
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Backend{
		cfg: cfg, log: log, api: api,
		cgroupRoot: "/sys/fs/cgroup", statGID: socketGID,
		proxies: map[string]*dockerproxy.Proxy{},
		stats:   map[string]*runnerStats{},
		cgPaths: map[string]string{},
	}, nil
}

// validName reports whether n is a runner name this instance could have made.
func (b *Backend) validName(n string) bool {
	return naming.ValidRunnerName(b.cfg.Prefix, b.cfg.HostID, n)
}

// hookDir is the runner's own hook dir (mounted into it).
func (b *Backend) hookDir(n string) string { return filepath.Join(b.cfg.HookDir, n) }

func (b *Backend) runnersDir() string        { return filepath.Join(b.cfg.StateDir, "runners") }
func (b *Backend) runnerDir(n string) string { return filepath.Join(b.runnersDir(), n) }

// PullLockPath is the file proxies and image GC use to exclude each other.
func PullLockPath(stateDir string) string { return filepath.Join(stateDir, "runners", ".pull.lock") }

// proxySock lives inside the runner dir, which is bind-mounted as a directory:
// the runner keeps reaching a re-created socket after a daemon restart (a
// file bind would pin the old, deleted inode).
func (b *Backend) proxySock(n string) string { return filepath.Join(b.runnerDir(n), "dp.sock") }

// Caps implements core.Backend. Job memory is bounded by the slice, not per
// container, so MemLimits is false.
func (b *Backend) Caps() core.Caps {
	return core.Caps{OS: core.Linux, MemLimits: false, DockerInJob: true}
}

// Prepare pulls the runner image. A failed pull is tolerated when the image is
// already present locally.
func (b *Backend) Prepare(ctx context.Context) error {
	if err := mkdirAll(b.runnersDir()); err != nil {
		return err
	}
	rc, err := b.api.ImagePull(ctx, b.cfg.Image, image.PullOptions{})
	if err == nil {
		_, err = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if err != nil {
		if _, ierr := b.api.ImageInspect(ctx, b.cfg.Image); ierr == nil {
			b.log.Warn("runner image pull failed, using local copy", "image", b.cfg.Image, "error", err.Error())
			return nil
		}
		return fmt.Errorf("pull %s: %w", b.cfg.Image, err)
	}
	return nil
}

func (b *Backend) labelFilter(runner string) filters.Args {
	f := filters.NewArgs(
		filters.Arg("label", naming.MetaManaged+"=1"),
		filters.Arg("label", naming.MetaInstance+"="+b.cfg.Instance),
	)
	if runner != "" {
		f.Add("label", naming.MetaRunner+"="+runner)
	}
	return f
}

// List returns this instance's runner containers (running or not), found by
// label, never by name.
func (b *Backend) List(ctx context.Context) ([]core.Handle, error) {
	f := b.labelFilter("")
	f.Add("label", naming.MetaRole+"=runner")
	cs, err := b.api.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return nil, err
	}
	out := make([]core.Handle, 0, len(cs))
	for _, c := range cs {
		if !b.validName(c.Labels[naming.MetaRunner]) {
			b.log.Warn("ignoring runner container with an invalid name label", "id", c.ID)
			continue
		}
		out = append(out, core.Handle{
			RunnerName: c.Labels[naming.MetaRunner],
			BackendID:  c.ID,
			StartedAt:  time.Unix(c.Created, 0),
		})
	}
	return out, nil
}

// Stop stops the runner and then its job containers: SIGTERM, SIGKILL after grace.
func (b *Backend) Stop(ctx context.Context, h core.Handle, grace time.Duration) error {
	secs := int(grace.Seconds())
	ids := []string{}
	if h.BackendID != "" {
		ids = append(ids, h.BackendID)
	}
	f := b.labelFilter(h.RunnerName)
	f.Add("label", naming.MetaRole+"=job")
	if jobs, err := b.api.ContainerList(ctx, container.ListOptions{Filters: f}); err == nil {
		for _, c := range jobs {
			ids = append(ids, c.ID)
		}
	}
	var errs []error
	for _, id := range ids {
		err := b.api.ContainerStop(ctx, id, container.StopOptions{Timeout: &secs})
		if err != nil && !cerrdefs.IsNotFound(err) {
			if kerr := b.api.ContainerKill(ctx, id, "KILL"); kerr != nil && !cerrdefs.IsNotFound(kerr) {
				errs = append(errs, kerr)
			}
		}
	}
	return errors.Join(errs...)
}

// --- pull gate across all runner proxies ---

// PullGate returns a gate over every proxy of this backend, present and
// future (a proxy created while paused starts paused). It satisfies
// imagegc.PullGate.
func (b *Backend) PullGate() interface {
	Pause()
	Resume()
	InFlight() int
} {
	return poolGate{b}
}

type poolGate struct{ b *Backend }

func (g poolGate) Pause() {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	g.b.paused++
	for _, p := range g.b.proxies {
		p.PausePulls()
	}
}

func (g poolGate) Resume() {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	if g.b.paused == 0 {
		return
	}
	g.b.paused--
	for _, p := range g.b.proxies {
		p.ResumePulls()
	}
}

func (g poolGate) InFlight() int {
	g.b.mu.Lock()
	defer g.b.mu.Unlock()
	n := 0
	for _, p := range g.b.proxies {
		n += p.InFlightPulls()
	}
	return n
}

// Adopt makes sure the runner's Docker proxy is up after a daemon restart. An
// out-of-process proxy normally survived and is left alone; a dead one (or an
// in-process one) is started again on the same socket path inside the runner
// dir, so the adopted runner keeps its Docker.
func (b *Backend) Adopt(ctx context.Context, h core.Handle) error {
	if !b.validName(h.RunnerName) {
		return errors.New("invalid runner name")
	}
	var binds []string
	if h.BackendID != "" {
		ci, err := b.api.ContainerInspect(ctx, h.BackendID)
		if err != nil {
			return err
		}
		if ci.HostConfig != nil {
			binds = ci.HostConfig.Binds
		}
	}
	sock := b.proxySock(h.RunnerName)
	if b.launcher != nil && proxyAlive(sock) {
		return nil // the proxy outlived the daemon; nothing to do
	}
	return b.startProxy(ctx, h.RunnerName, sock, b.allowedFromBinds(h.RunnerName, binds)...)
}

// allowedFromBinds is the set of host paths a job may mount: the runner dir,
// operator-allowed paths and the host side of the runner's own mounts, minus
// what must stay private (hook binary and dir, the proxy socket).
func (b *Backend) allowedFromBinds(runner string, binds []string) []string {
	out := append([]string{b.runnerDir(runner)}, b.cfg.AllowedBinds...)
	skip := map[string]bool{b.cfg.HookBinary: true, b.hookDir(runner): true, b.proxySock(runner): true}
	for _, bd := range binds {
		src, _, _ := strings.Cut(bd, ":")
		if src != "" && !skip[src] {
			out = append(out, src)
		}
	}
	return out
}
