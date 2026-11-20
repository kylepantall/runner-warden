// Package winvm runs Windows runners in persistent dockurr/windows VM
// containers: one container per [[windows.vm]], one ephemeral JIT runner per
// job, started in the guest over SSH as a one-shot Scheduled Task.
//
// Scheduler contract (one Pool serves several VMs):
//
//   - Pick returns an idle VM that serves the job's target and claims it.
//     Pass the VM to Start via RunnerSpec.Env["RUNNER_WARDEN_VM"], or call
//     Release if the job is abandoned. Start with no such env picks itself.
//   - Reserve MemFor(vm) (the VM's full RAM) from the ledger while Up(vm).
//     Caps().FixedMemBytes is only the largest VM; prefer MemFor.
//   - Ask WorkFolder(vm, runner) before generating the JIT config and pass it
//     as RunnerScaleSetJitRunnerSetting.WorkFolder.
//   - Call StopIdle periodically, and StopNow(vm) to free memory.
package winvm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// EnvVM selects the VM in RunnerSpec.Env.
const EnvVM = "RUNNER_WARDEN_VM"

var (
	// ErrNoVM means no idle VM can take the job.
	ErrNoVM = errors.New("winvm: no idle VM available")
	// ErrUnknownVM means the named VM is not in the pool.
	ErrUnknownVM = errors.New("winvm: unknown VM")
	// ErrBusy means the VM already has a runner.
	ErrBusy = errors.New("winvm: VM has a runner")
	// ErrLicenceCap means the Windows concurrency limit is reached.
	ErrLicenceCap = errors.New("winvm: windows.max_concurrent reached")
	// ErrNotOwned means the container belongs to another instance.
	ErrNotOwned = errors.New("winvm: container is not owned by this instance")
	// ErrVMExited means the VM container stopped.
	ErrVMExited = errors.New("winvm: VM container exited")
	// ErrSSHTimeout means the guest never accepted SSH.
	ErrSSHTimeout = errors.New("winvm: guest ssh did not come up")
	// ErrNoRunnerVer means the runner version could not be determined.
	ErrNoRunnerVer = errors.New("winvm: runner version unknown")
	// ErrBadRunner means the runner name failed validation.
	ErrBadRunner = errors.New("winvm: invalid runner name")
	// ErrForeignAlive means a foreign container is still running.
	ErrForeignAlive = errors.New("winvm: foreign container is running")
)

// Config configures the Pool.
type Config struct {
	Instance string
	HostID   string
	StateDir string // per-VM state lives in <StateDir>/windows/<vm>/
	Slice    string // cgroup parent of VM containers, naming.Slice(instance)
	// DockerSocket defaults to /var/run/docker.sock.
	DockerSocket string
	Windows      config.Windows

	SSHReadyTimeout time.Duration // default 10m
	StopSSHTimeout  time.Duration // default 3m before the VM is restarted
	StopTimeout     time.Duration // graceful docker stop, default 2m
	SSHPoll         time.Duration // default 5s
	// TaskLogon is the Scheduled Task logon type: Interactive (default; needs
	// the dockurr auto-logon session) or S4U.
	TaskLogon string

	HTTPClient *http.Client // runner version lookup
	ReleaseAPI string       // default https://api.github.com/repos/actions/runner/releases/latest
	Log        *slog.Logger
}

// dockerAPI is the subset of the Docker client the pool uses.
type dockerAPI interface {
	ContainerCreate(ctx context.Context, cfg *container.Config, hc *container.HostConfig, nc *network.NetworkingConfig, p *ocispec.Platform, name string) (container.CreateResponse, error)
	ContainerStart(ctx context.Context, id string, opts container.StartOptions) error
	ContainerInspect(ctx context.Context, id string) (container.InspectResponse, error)
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ContainerStop(ctx context.Context, id string, opts container.StopOptions) error
	ContainerRestart(ctx context.Context, id string, opts container.StopOptions) error
	ContainerRemove(ctx context.Context, id string, opts container.RemoveOptions) error
	ContainerStatsOneShot(ctx context.Context, id string) (container.StatsResponseReader, error)
	ImageInspect(ctx context.Context, id string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImagePull(ctx context.Context, ref string, opts image.PullOptions) (io.ReadCloser, error)
}

// Pool implements core.Backend for Windows and manages every VM.
type Pool struct {
	cfg Config
	log *slog.Logger
	api dockerAPI
	// exec returns the key-authenticated Exec for a VM; passExec the
	// password-authenticated one (adopt only).
	exec     func(vm config.WindowsVM) (Exec, error)
	passExec func(vm config.WindowsVM, password string) Exec

	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error

	mu         sync.Mutex
	vms        map[string]config.WindowsVM
	order      []string
	up         map[string]bool
	ready      map[string]bool // ssh answered since the container started
	claimed    map[string]bool
	lastActive map[string]time.Time
	runners    map[string]*runnerRec
	execs      map[string]Exec
	logBytes   map[string]int64
	vmLocks    map[string]*sync.Mutex

	versionMu sync.Mutex
	version   string // resolved runner version
}

var _ core.Backend = (*Pool)(nil)

// New connects to Docker and loads (or generates) the SSH key.
func New(cfg Config) (*Pool, error) {
	if cfg.DockerSocket == "" {
		cfg.DockerSocket = "/var/run/docker.sock"
	}
	cli, err := client.NewClientWithOpts(client.WithHost("unix://"+cfg.DockerSocket), client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	signer, _, err := EnsureKey(cfg.Windows.SSHKeyFile)
	if err != nil {
		return nil, err
	}
	p, err := newPool(cfg, cli, nil)
	if err != nil {
		return nil, err
	}
	p.exec = func(vm config.WindowsVM) (Exec, error) {
		return NewSSHExec(SSHConfig{
			Addr: sshAddr(vm), User: cfg.Windows.SSHUser, Signer: signer, KnownHosts: p.knownHosts(vm.Name),
		}), nil
	}
	p.passExec = func(vm config.WindowsVM, pw string) Exec {
		return NewSSHExec(SSHConfig{Addr: sshAddr(vm), User: cfg.Windows.SSHUser, Password: pw, KnownHosts: p.knownHosts(vm.Name)})
	}
	return p, nil
}

func sshAddr(vm config.WindowsVM) string { return fmt.Sprintf("127.0.0.1:%d", vm.SSHPort) }

func newPool(cfg Config, api dockerAPI, exec func(config.WindowsVM) (Exec, error)) (*Pool, error) {
	switch {
	case cfg.Instance == "" || cfg.StateDir == "" || cfg.Slice == "":
		return nil, errors.New("winvm: instance, state dir and slice are required")
	case len(cfg.Windows.VMs) == 0:
		return nil, errors.New("winvm: no VMs configured")
	}
	dur := func(d *time.Duration, def time.Duration) {
		if *d <= 0 {
			*d = def
		}
	}
	dur(&cfg.SSHReadyTimeout, 10*time.Minute)
	dur(&cfg.StopSSHTimeout, 3*time.Minute)
	dur(&cfg.StopTimeout, 2*time.Minute)
	dur(&cfg.SSHPoll, 5*time.Second)
	if cfg.TaskLogon == "" {
		cfg.TaskLogon = "Interactive"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.ReleaseAPI == "" {
		cfg.ReleaseAPI = "https://api.github.com/repos/actions/runner/releases/latest"
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Pool{
		cfg: cfg, log: log, api: api, exec: exec,
		now: time.Now, sleep: sleepCtx,
		vms: map[string]config.WindowsVM{}, up: map[string]bool{}, ready: map[string]bool{},
		claimed: map[string]bool{}, lastActive: map[string]time.Time{},
		runners: map[string]*runnerRec{}, execs: map[string]Exec{}, logBytes: map[string]int64{}, vmLocks: map[string]*sync.Mutex{},
		version: cfg.Windows.RunnerVersion,
	}
	for _, v := range cfg.Windows.VMs {
		p.vms[v.Name] = v
		p.order = append(p.order, v.Name)
		p.vmLocks[v.Name] = &sync.Mutex{}
	}
	return p, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (p *Pool) vm(name string) (config.WindowsVM, error) {
	v, ok := p.vms[name]
	if !ok {
		return v, ErrUnknownVM
	}
	return v, nil
}

func (p *Pool) paths(runner string) jobPaths {
	return jobPaths{Runner: runner, RunnerDir: p.cfg.Windows.RunnerDir, WorkRoot: p.cfg.Windows.WorkDir}
}

// Caps reports the VM limits: FixedMemBytes is the largest VM; use MemFor for the exact value.
func (p *Pool) Caps() core.Caps {
	var mem int64
	for _, v := range p.cfg.Windows.VMs {
		mem = max(mem, int64(v.Mem))
	}
	n := len(p.cfg.Windows.VMs)
	if m := p.cfg.Windows.MaxConcurrent; m > 0 {
		n = min(n, m)
	}
	return core.Caps{OS: core.Windows, FixedMemBytes: mem, MaxConcurrent: n}
}

// MemFor is the RAM a VM holds while its container is up.
func (p *Pool) MemFor(vm string) int64 { return int64(p.vms[vm].Mem) }

// Up reports whether the VM container is running.
func (p *Pool) Up(vm string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.up[vm]
}

// VMFor returns the VM a known runner sits on.
func (p *Pool) VMFor(runner string) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.runners[runner]; r != nil {
		return r.VM, true
	}
	return "", false
}

// WorkFolder is the absolute guest work folder for the JIT config's
// WorkFolder setting. The runner resolves it with Path.Combine(root, folder),
// so an absolute path wins (unverified against the runner source).
func (p *Pool) WorkFolder(_, runner string) string { return p.paths(runner).WorkFolder() }

func (p *Pool) busyLocked(vm string) bool {
	if p.claimed[vm] {
		return true
	}
	for _, r := range p.runners {
		if r.VM == vm {
			return true
		}
	}
	return false
}

// licencesLocked counts VMs that are up or about to be, except skip.
func (p *Pool) licencesLocked(skip string) int {
	n := 0
	for name := range p.vms {
		if name != skip && (p.up[name] || p.claimed[name]) {
			n++
		}
	}
	return n
}

func (p *Pool) licenceCap() int {
	if m := p.cfg.Windows.MaxConcurrent; m > 0 {
		return m
	}
	return len(p.vms)
}

// Pick claims an idle VM serving target, preferring one already running. It
// returns false when none is idle or the licence cap forbids booting another.
func (p *Pool) Pick(target core.Target) (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cold string
	for _, name := range p.order {
		if !p.vms[name].Serves(target) || p.busyLocked(name) {
			continue
		}
		if p.up[name] {
			p.claimed[name] = true
			return name, true
		}
		if cold == "" {
			cold = name
		}
	}
	if cold == "" || p.licencesLocked(cold) >= p.licenceCap() {
		return "", false
	}
	p.claimed[cold] = true
	return cold, true
}

// Peek is Pick without claiming: the VM Pick would choose for target, its
// memory, and whether it is up (and, being unclaimed and unbound, idle). It
// never blocks. vm is "" when no VM serves the target.
func (p *Pool) Peek(target core.Target) (vm string, mem int64, up bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	cold := ""
	for _, name := range p.order {
		if !p.vms[name].Serves(target) || p.busyLocked(name) {
			continue
		}
		if p.up[name] {
			return name, int64(p.vms[name].Mem), true
		}
		if cold == "" {
			cold = name
		}
	}
	if cold == "" {
		return "", 0, false
	}
	return cold, int64(p.vms[cold].Mem), false
}

// Release drops a claim made by Pick that will not reach Start.
func (p *Pool) Release(vm string) {
	p.mu.Lock()
	delete(p.claimed, vm)
	p.lastActive[vm] = p.now()
	p.mu.Unlock()
}

// Prepare pulls the image if missing, resolves the runner version, loads
// persisted runner state and discovers which VM containers are running.
func (p *Pool) Prepare(ctx context.Context) error {
	if _, err := p.api.ImageInspect(ctx, p.cfg.Windows.Image); err != nil {
		rc, err := p.api.ImagePull(ctx, p.cfg.Windows.Image, image.PullOptions{})
		if err != nil {
			return fmt.Errorf("pull %s: %w", p.cfg.Windows.Image, err)
		}
		_, _ = io.Copy(io.Discard, rc)
		_ = rc.Close()
	}
	if _, err := p.runnerVersion(ctx); err != nil {
		p.log.Warn("runner version lookup failed", "error", err.Error())
	}
	for _, name := range p.order {
		recs, err := p.loadState(name)
		if err != nil {
			return err
		}
		insp, err := p.api.ContainerInspect(ctx, p.vms[name].Container(p.cfg.Instance))
		running := err == nil && insp.State != nil && insp.State.Running && ownedBy(insp, p.cfg) == nil
		p.mu.Lock()
		p.up[name] = running
		p.lastActive[name] = p.now()
		for _, r := range recs {
			p.runners[r.Name] = r
		}
		p.mu.Unlock()
	}
	return nil
}

// List returns the runners recorded in the per-VM state files.
func (p *Pool) List(_ context.Context) ([]core.Handle, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]core.Handle, 0, len(p.runners))
	for _, r := range p.runners {
		out = append(out, core.Handle{RunnerName: r.Name, BackendID: r.VM, StartedAt: r.StartedAt})
	}
	return out, nil
}
