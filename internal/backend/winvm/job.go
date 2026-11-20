package winvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

const (
	startTimeout   = 10 * time.Minute
	statsTimeout   = 30 * time.Second
	cleanupTimeout = 3 * time.Minute
	stopAttempt    = 60 * time.Second
)

// runScript runs a script in the VM and fails on a non-zero exit.
func (p *Pool) runScript(ctx context.Context, v config.WindowsVM, script, stdin string, timeout time.Duration) (string, error) {
	ex, err := p.execFor(v)
	if err != nil {
		return "", err
	}
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, errOut, code, err := ex.RunStdin(c, script, stdin)
	if err != nil {
		return out, err
	}
	if code != 0 {
		return out, fmt.Errorf("guest script exit %d: %s", code, tail(errOut, 400))
	}
	return out, nil
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// EnsureRunnerFiles makes the guest runner install match the wanted version.
func (p *Pool) EnsureRunnerFiles(ctx context.Context, vm string) error {
	v, err := p.vm(vm)
	if err != nil {
		return err
	}
	ver, err := p.runnerVersion(ctx)
	if err != nil {
		return err
	}
	_, err = p.runScript(ctx, v, ensureRunnerScript(p.cfg.Windows.RunnerDir, ver), "", startTimeout)
	return err
}

// Start launches an ephemeral JIT runner in a VM. spec.Env[EnvVM] names the
// VM (from Pick); without it Start picks one for spec.ScaleSet.Target.
// MemBytes, CPUSet, Mounts, Image, Devices and other Env are ignored: the VM's
// size is fixed and nothing is forwarded into the guest except the JIT config.
func (p *Pool) Start(ctx context.Context, spec core.RunnerSpec) (core.Handle, error) {
	if spec.Name == "" || spec.JITConfig == "" {
		return core.Handle{}, errors.New("runner spec needs a name and JIT config")
	}
	if !runnerNameRE.MatchString(spec.Name) {
		return core.Handle{}, ErrBadRunner
	}
	vm := spec.Env[EnvVM]
	if vm == "" {
		picked, ok := p.Pick(spec.ScaleSet.Target)
		if !ok {
			return core.Handle{}, ErrNoVM
		}
		vm = picked
	}
	v, err := p.vm(vm)
	if err != nil {
		return core.Handle{}, err
	}
	rec := &runnerRec{Name: spec.Name, VM: vm, StartedAt: p.now()}
	p.mu.Lock()
	for _, r := range p.runners {
		if r.VM == vm && r.Name != spec.Name {
			p.mu.Unlock()
			return core.Handle{}, ErrBusy
		}
	}
	p.runners[spec.Name] = rec
	delete(p.claimed, vm)
	err = p.saveStateLocked(vm)
	p.mu.Unlock()
	if err != nil {
		p.dropRunner(spec.Name)
		return core.Handle{}, err
	}

	fail := func(err error) (core.Handle, error) {
		_ = p.Cleanup(context.WithoutCancel(ctx), spec.Name)
		p.dropRunner(spec.Name) // forget even if the guest could not be cleaned
		return core.Handle{}, fmt.Errorf("start windows runner: %w", err)
	}
	if err := p.EnsureUp(ctx, vm); err != nil {
		return fail(err)
	}
	if err := p.EnsureRunnerFiles(ctx, vm); err != nil {
		return fail(err)
	}
	if s, ok := p.sampleVM(ctx, v); ok {
		p.mu.Lock()
		rec.CPUBase, rec.IOBase = s.cpu, s.io
		_ = p.saveStateLocked(vm)
		p.mu.Unlock()
	}
	if _, err := p.runScript(ctx, v, startJobScript(p.paths(spec.Name), p.cfg.TaskLogon), spec.JITConfig, startTimeout); err != nil {
		return fail(err)
	}
	return core.Handle{RunnerName: spec.Name, BackendID: vm, StartedAt: rec.StartedAt}, nil
}

func (p *Pool) dropRunner(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.runners[name]
	if r == nil {
		return
	}
	delete(p.runners, name)
	delete(p.logBytes, name)
	p.lastActive[r.VM] = p.now()
	_ = p.saveStateLocked(r.VM)
}

type vmSample struct {
	cpu  float64
	io   int64
	mem  int64
	peak int64
}

// sampleVM reads the VM container's counters from the Docker stats API.
func (p *Pool) sampleVM(ctx context.Context, v config.WindowsVM) (vmSample, bool) {
	var s vmSample
	r, err := p.api.ContainerStatsOneShot(ctx, v.Container(p.cfg.Instance))
	if err != nil {
		return s, false
	}
	defer r.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		return s, false
	}
	s.cpu = float64(st.CPUStats.CPUUsage.TotalUsage) / 1e9
	s.mem = clampInt64(st.MemoryStats.Usage)
	s.peak = clampInt64(st.MemoryStats.MaxUsage)
	for _, e := range st.BlkioStats.IoServiceBytesRecursive {
		if strings.EqualFold(e.Op, "read") || strings.EqualFold(e.Op, "write") {
			s.io += clampInt64(e.Value)
		}
	}
	return s, true
}

func (p *Pool) recFor(h core.Handle) (*runnerRec, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r := p.runners[h.RunnerName]; r != nil {
		return r, true
	}
	return nil, false
}

// Stats reports container CPU/IO since the runner started (the VM is the
// resource boundary) and guest liveness. If the guest does not answer, Alive
// falls back to the container state and LogBytes to the last known value.
func (p *Pool) Stats(ctx context.Context, h core.Handle) (core.Usage, error) {
	u := core.Usage{SampledAt: p.now()}
	rec, ok := p.recFor(h)
	if !ok {
		return u, nil
	}
	v := p.vms[rec.VM]
	insp, err := p.api.ContainerInspect(ctx, v.Container(p.cfg.Instance))
	switch {
	case err == nil:
		u.Alive = insp.State != nil && insp.State.Running
	case !cerrdefs.IsNotFound(err):
		return u, err
	}
	if s, ok := p.sampleVM(ctx, v); ok {
		p.mu.Lock()
		cpu, io := rec.CPUBase, rec.IOBase
		if s.cpu < cpu || s.io < io { // container restarted: counters reset
			rec.CPUBase, rec.IOBase, cpu, io = 0, 0, 0, 0
		}
		p.mu.Unlock()
		u.CPUSeconds, u.IOBytes, u.MemCurrent, u.MemPeak = s.cpu-cpu, s.io-io, s.mem, s.peak
	}
	if !u.Alive {
		return u, nil
	}
	out, err := p.runScript(ctx, v, statsScript(p.paths(h.RunnerName)), "", statsTimeout)
	if err == nil {
		var g guestStats
		if g, err = parseGuestStats(out); err == nil {
			u.Alive = g.Alive()
			u.LogBytes = g.Log + g.Diag
			p.mu.Lock()
			p.logBytes[h.RunnerName] = u.LogBytes
			p.mu.Unlock()
			return u, nil
		}
	}
	p.log.Warn("guest stats unavailable", "vm", rec.VM, "runner", h.RunnerName, "error", err.Error())
	p.mu.Lock()
	u.LogBytes = p.logBytes[h.RunnerName]
	p.mu.Unlock()
	return u, nil
}

// Stop ends the runner's task and process trees. If the guest does not answer
// over SSH for StopSSHTimeout the VM container is restarted.
func (p *Pool) Stop(ctx context.Context, h core.Handle, grace time.Duration) error {
	rec, ok := p.recFor(h)
	if !ok {
		return nil
	}
	v := p.vms[rec.VM]
	if !p.Up(rec.VM) {
		return nil
	}
	script := stopScript(p.paths(h.RunnerName), int(min(max(grace, 0), 30*time.Second)/time.Second))
	deadline := p.now().Add(p.cfg.StopSSHTimeout)
	var last error
	for {
		_, err := p.runScript(ctx, v, script, "", stopAttempt)
		if err == nil {
			return nil
		}
		last = err
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !p.now().Before(deadline) {
			break
		}
		if err := p.sleep(ctx, p.cfg.SSHPoll); err != nil {
			return err
		}
	}
	p.log.Warn("guest unresponsive, restarting vm", "vm", rec.VM, "runner", h.RunnerName, "error", last.Error())
	if err := p.restartVM(ctx, v); err != nil {
		return err
	}
	p.mu.Lock()
	rec.Restarted = true
	_ = p.saveStateLocked(rec.VM)
	p.mu.Unlock()
	return nil
}

// Cleanup removes the runner's task, processes and work dir in the guest and
// forgets the runner. Idempotent; unknown runners are a no-op. When the VM is
// down or was restarted under the runner nothing in the guest is left running;
// leftover files are swept by the next Start.
func (p *Pool) Cleanup(ctx context.Context, runnerName string) error {
	p.mu.Lock()
	rec := p.runners[runnerName]
	p.mu.Unlock()
	if rec == nil {
		return nil
	}
	if p.Up(rec.VM) && !rec.Restarted {
		out, err := p.runScript(ctx, p.vms[rec.VM], cleanupScript(p.paths(runnerName)), "", cleanupTimeout)
		if err != nil {
			return err
		}
		if strings.Contains(out, "leftover=1") {
			return errors.New("work dir still present")
		}
	}
	p.dropRunner(runnerName)
	return nil
}

// clampInt64 converts a counter without wrapping on overflow.
func clampInt64(v uint64) int64 {
	return int64(min(v, math.MaxInt64))
}
