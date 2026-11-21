package winvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/go-connections/nat"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/naming"
)

const (
	roleVM    = "vm"
	labelVM   = naming.MetaPrefix + ".vm"
	labelSpec = naming.MetaPrefix + ".spec"
	sshPort   = "22/tcp"
)

func ramSize(b int64) string {
	if b%(1<<30) == 0 {
		return strconv.FormatInt(b>>30, 10) + "G"
	}
	return strconv.FormatInt(b>>20, 10) + "M"
}

// ownedBy errors unless the container carries this instance's VM labels.
func ownedBy(insp container.InspectResponse, cfg Config) error {
	if insp.Config == nil {
		return ErrNotOwned
	}
	l := insp.Config.Labels
	if l[naming.MetaManaged] != "1" || l[naming.MetaInstance] != cfg.Instance || l[naming.MetaRole] != roleVM {
		return ErrNotOwned
	}
	return nil
}

// vmEnv is the dockurr/windows environment for a VM.
func (p *Pool) vmEnv(v config.WindowsVM, password string) []string {
	env := p.baseEnv(v)
	if password != "" {
		env = append(env, "PASSWORD="+password)
	}
	return env
}

// baseEnv is the VM environment without any credentials (also the input of
// the drift hash).
func (p *Pool) baseEnv(v config.WindowsVM) []string {
	return []string{
		"VERSION=" + v.Version,
		"RAM_SIZE=" + ramSize(int64(v.Mem)),
		"CPU_CORES=" + strconv.Itoa(v.CPUs),
		"DISK_SIZE=" + v.DiskSize,
		"USERNAME=" + p.cfg.Windows.SSHUser,
	}
}

// containerSpec builds the VM container. storage/oem override the configured
// directories (adopt reuses a foreign VM's paths).
func (p *Pool) containerSpec(v config.WindowsVM, storage, oem string, env []string) (*container.Config, *container.HostConfig) {
	if storage == "" {
		storage = v.StorageDir
	}
	if oem == "" {
		oem = v.OEMDir
	}
	stop := int(p.cfg.StopTimeout / time.Second)
	port := nat.Port(sshPort)
	cfg := &container.Config{
		Image:        p.cfg.Windows.Image,
		Env:          env,
		ExposedPorts: nat.PortSet{port: struct{}{}},
		StopTimeout:  &stop,
		Labels: map[string]string{
			naming.MetaManaged:  "1",
			naming.MetaInstance: p.cfg.Instance,
			naming.MetaHostID:   p.cfg.HostID,
			naming.MetaRunner:   "",
			naming.MetaRole:     roleVM,
			labelVM:             v.Name,
		},
	}
	hc := &container.HostConfig{
		Binds:        []string{storage + ":/storage", oem + ":/oem"},
		PortBindings: nat.PortMap{port: {{HostIP: "127.0.0.1", HostPort: strconv.Itoa(v.SSHPort)}}},
		CapAdd:       []string{"NET_ADMIN"},
		Resources: container.Resources{
			CgroupParent: p.cfg.Slice,
			Devices: []container.DeviceMapping{
				{PathOnHost: "/dev/kvm", PathInContainer: "/dev/kvm", CgroupPermissions: "rwm"},
				{PathOnHost: "/dev/net/tun", PathInContainer: "/dev/net/tun", CgroupPermissions: "rwm"},
			},
		},
	}
	// The spec hash detects config drift. It is built from the config without
	// the guest password, which must not end up (even hashed) in a label.
	specEnv := p.baseEnv(v)
	h := sha256.New()
	_ = json.NewEncoder(h).Encode([]any{cfg.Image, specEnv, hc.Binds, hc.PortBindings, hc.CgroupParent})
	cfg.Labels[labelSpec] = hex.EncodeToString(h.Sum(nil))[:16]
	return cfg, hc
}

func (p *Pool) readPassword(vm string) string {
	b, err := os.ReadFile(p.passwordFile(vm))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// createVM creates and starts the container with the given env.
func (p *Pool) createVM(ctx context.Context, v config.WindowsVM, storage, oem string, env []string) error {
	cfg, hc := p.containerSpec(v, storage, oem, env)
	for _, d := range []string{orDefault(storage, v.StorageDir), orDefault(oem, v.OEMDir)} {
		if err := os.MkdirAll(d, 0o755); err != nil { //nolint:gosec // VM state dirs are mounted into the container and must be traversable
			return err
		}
	}
	created, err := p.api.ContainerCreate(ctx, cfg, hc, &network.NetworkingConfig{}, nil, v.Container(p.cfg.Instance))
	if err != nil {
		return fmt.Errorf("create vm container: %w", err)
	}
	return p.api.ContainerStart(ctx, created.ID, container.StartOptions{})
}

func orDefault(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

// ensureContainer creates the container if missing, else starts it. A stopped
// container whose spec drifted from the config is recreated (disk is kept).
func (p *Pool) ensureContainer(ctx context.Context, v config.WindowsVM) error {
	name := v.Container(p.cfg.Instance)
	insp, err := p.api.ContainerInspect(ctx, name)
	switch {
	case cerrdefs.IsNotFound(err):
		return p.createVM(ctx, v, "", "", p.vmEnv(v, p.readPassword(v.Name)))
	case err != nil:
		return err
	}
	if err := ownedBy(insp, p.cfg); err != nil {
		return err
	}
	if insp.State != nil && insp.State.Running {
		return nil
	}
	want, _ := p.containerSpec(v, "", "", p.vmEnv(v, p.readPassword(v.Name)))
	if insp.Config.Labels[labelSpec] != want.Labels[labelSpec] {
		// Config changed (mem, cpus, ports, image): recreate; the disk is kept.
		if err := p.api.ContainerRemove(ctx, insp.ID, container.RemoveOptions{Force: true}); err != nil {
			return err
		}
		return p.createVM(ctx, v, "", "", p.vmEnv(v, p.readPassword(v.Name)))
	}
	return p.api.ContainerStart(ctx, insp.ID, container.StartOptions{})
}

func (p *Pool) execFor(v config.WindowsVM) (Exec, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.execs[v.Name]; e != nil {
		return e, nil
	}
	if p.exec == nil {
		return nil, errors.New("winvm: no ssh exec configured")
	}
	e, err := p.exec(v)
	if err != nil {
		return nil, err
	}
	p.execs[v.Name] = e
	return e, nil
}

// EnsureUp creates (if missing) and starts the VM container and waits until
// guest SSH answers.
func (p *Pool) EnsureUp(ctx context.Context, vm string) error {
	return p.ensureUp(ctx, vm, p.cfg.SSHReadyTimeout)
}

func (p *Pool) ensureUp(ctx context.Context, name string, wait time.Duration) error {
	v, err := p.vm(name)
	if err != nil {
		return err
	}
	lock := p.vmLocks[name]
	lock.Lock()
	defer lock.Unlock()

	p.mu.Lock()
	if p.up[name] && p.ready[name] {
		p.mu.Unlock()
		return nil
	}
	if !p.up[name] {
		if p.licencesLocked(name) >= p.licenceCap() {
			p.mu.Unlock()
			return ErrLicenceCap
		}
		p.up[name] = true // reserve the licence before the slow part
	}
	p.mu.Unlock()

	if err := p.ensureContainer(ctx, v); err != nil {
		p.mu.Lock()
		p.up[name] = false
		p.mu.Unlock()
		return err
	}
	ex, err := p.execFor(v)
	if err != nil {
		return err
	}
	if err := p.waitSSH(ctx, v, ex, wait); err != nil {
		return err
	}
	p.mu.Lock()
	p.ready[name] = true
	p.lastActive[name] = p.now()
	p.mu.Unlock()
	return nil
}

// waitSSH polls until a trivial script succeeds in the guest.
func (p *Pool) waitSSH(ctx context.Context, v config.WindowsVM, ex Exec, wait time.Duration) error {
	start := p.now()
	lastLog := start
	for {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		out, _, code, err := ex.Run(c, "'rw-ready'")
		cancel()
		if err == nil && code == 0 && strings.Contains(out, "rw-ready") {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		insp, ierr := p.api.ContainerInspect(ctx, v.Container(p.cfg.Instance))
		if ierr == nil && (insp.State == nil || !insp.State.Running) {
			p.mu.Lock()
			p.up[v.Name] = false
			p.mu.Unlock()
			return ErrVMExited
		}
		now := p.now()
		if now.Sub(start) >= wait {
			return ErrSSHTimeout
		}
		if now.Sub(lastLog) >= time.Minute {
			lastLog = now
			p.log.Info("waiting for guest ssh", "vm", v.Name, "elapsed", now.Sub(start).Round(time.Second).String())
		}
		if err := p.sleep(ctx, p.cfg.SSHPoll); err != nil {
			return err
		}
	}
}

// stopVM gracefully stops a VM container we own and marks it down. The caller
// must have claimed the VM.
func (p *Pool) stopVM(ctx context.Context, v config.WindowsVM) error {
	name := v.Container(p.cfg.Instance)
	insp, err := p.api.ContainerInspect(ctx, name)
	if cerrdefs.IsNotFound(err) {
		p.markDown(v.Name)
		return nil
	}
	if err != nil {
		return err
	}
	if err := ownedBy(insp, p.cfg); err != nil {
		return err
	}
	if insp.State != nil && insp.State.Running {
		secs := int(p.cfg.StopTimeout / time.Second)
		if err := p.api.ContainerStop(ctx, insp.ID, container.StopOptions{Timeout: &secs}); err != nil {
			return err
		}
	}
	p.markDown(v.Name)
	return nil
}

func (p *Pool) markDown(vm string) {
	p.mu.Lock()
	p.up[vm], p.ready[vm] = false, false
	p.lastActive[vm] = p.now()
	p.mu.Unlock()
}

// restartVM hard-restarts a hung VM container.
func (p *Pool) restartVM(ctx context.Context, v config.WindowsVM) error {
	name := v.Container(p.cfg.Instance)
	insp, err := p.api.ContainerInspect(ctx, name)
	if err != nil {
		return err
	}
	if err := ownedBy(insp, p.cfg); err != nil {
		return err
	}
	secs := 20
	if err := p.api.ContainerRestart(ctx, insp.ID, container.StopOptions{Timeout: &secs}); err != nil {
		return err
	}
	p.mu.Lock()
	p.up[v.Name], p.ready[v.Name] = true, false
	p.mu.Unlock()
	return nil
}

// StopIdle stops VMs with no runner idle for at least idle_ttl and returns
// their names. Errors are logged and the VM is tried again next call.
func (p *Pool) StopIdle(ctx context.Context, now time.Time) []string {
	ttl := p.cfg.Windows.IdleTTL.D()
	var stopped []string
	for _, name := range p.order {
		p.mu.Lock()
		idle := p.up[name] && !p.busyLocked(name) && now.Sub(p.lastActive[name]) >= ttl
		if idle {
			p.claimed[name] = true // keep Pick away while stopping
		}
		p.mu.Unlock()
		if !idle {
			continue
		}
		err := p.stopVM(ctx, p.vms[name])
		p.Release(name)
		if err != nil {
			p.log.Warn("idle vm stop failed", "vm", name, "error", err.Error())
			continue
		}
		stopped = append(stopped, name)
	}
	return stopped
}

// StopNow stops an idle VM immediately, to free memory for another.
func (p *Pool) StopNow(ctx context.Context, vm string) error {
	v, err := p.vm(vm)
	if err != nil {
		return err
	}
	p.mu.Lock()
	if p.busyLocked(vm) {
		p.mu.Unlock()
		return ErrBusy
	}
	p.claimed[vm] = true
	p.mu.Unlock()
	defer p.Release(vm)
	return p.stopVM(ctx, v)
}
