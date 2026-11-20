package winvm

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
)

//go:embed embed/install.bat embed/install.ps1 embed/baseline.ps1
var embedded embed.FS

// Embedded returns a bundled file by name (install.bat, install.ps1, baseline.ps1).
func Embedded(name string) string {
	b, _ := embedded.ReadFile("embed/" + name)
	return string(b)
}

const (
	provisionSSHWait  = 90 * time.Minute // Windows install takes 20-40 min
	installDoneWait   = 15 * time.Minute
	baselineTimeout   = 90 * time.Minute
	userScriptTimeout = 60 * time.Minute
)

var ErrStorageNotEmpty = errors.New("winvm: storage already holds a VM disk; use adopt")

// OEMFiles writes the first-logon files into dir: install.bat, install.ps1
// and authorized_key.pub (the daemon's public key).
func OEMFiles(dir, pubKey string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	files := map[string]string{
		"install.bat":        Embedded("install.bat"),
		"install.ps1":        Embedded("install.ps1"),
		"authorized_key.pub": strings.TrimSpace(pubKey) + "\n",
	}
	for n, c := range files {
		if err := os.WriteFile(filepath.Join(dir, n), []byte(c), 0o644); err != nil {
			return err
		}
	}
	return nil
}

const pwChars = "abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"

// randomPassword is 24 characters with upper, lower and digit guaranteed.
func randomPassword() (string, error) {
	pick := func(set string) (byte, error) {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		if err != nil {
			return 0, err
		}
		return set[n.Int64()], nil
	}
	out := make([]byte, 24)
	for i := range out {
		c, err := pick(pwChars)
		if err != nil {
			return "", err
		}
		out[i] = c
	}
	for i, set := range []string{"abcdefghijkmnopqrstuvwxyz", "ABCDEFGHJKLMNPQRSTUVWXYZ", "23456789"} {
		c, err := pick(set)
		if err != nil {
			return "", err
		}
		out[i] = c
	}
	return string(out), nil
}

// Provision installs a fresh Windows VM: OEM files, guest user password,
// container, then the bundled baseline and the optional userScript over SSH.
func (p *Pool) Provision(ctx context.Context, vm, userScript string) error {
	v, err := p.vm(vm)
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(v.StorageDir, "data.img")); err == nil {
		return ErrStorageNotEmpty
	}
	_, pub, err := EnsureKey(p.cfg.Windows.SSHKeyFile)
	if err != nil {
		return err
	}
	if err := OEMFiles(v.OEMDir, pub); err != nil {
		return err
	}
	if err := os.MkdirAll(p.vmStateDir(vm), 0o700); err != nil {
		return err
	}
	pw := p.readPassword(vm)
	if pw == "" {
		if pw, err = randomPassword(); err != nil {
			return err
		}
		if err := os.WriteFile(p.passwordFile(vm), []byte(pw+"\n"), 0o600); err != nil {
			return err
		}
	}
	_ = os.Remove(p.knownHosts(vm)) // a fresh install has a new host key
	p.log.Info("provisioning windows vm, install takes 20-40 minutes", "vm", vm)
	if err := p.ensureUp(ctx, vm, provisionSSHWait); err != nil {
		return err
	}
	ex, err := p.execFor(v)
	if err != nil {
		return err
	}
	if err := p.waitInstallDone(ctx, ex); err != nil {
		return err
	}
	p.log.Info("guest first-boot setup done, installing baseline", "vm", vm)
	pre := "$ErrorActionPreference='Stop'\n"
	if _, err := p.runScript(ctx, v, pre+Embedded("baseline.ps1"), "", baselineTimeout); err != nil {
		return fmt.Errorf("baseline: %w", err)
	}
	if err := p.EnsureRunnerFiles(ctx, vm); err != nil {
		return fmt.Errorf("runner files: %w", err)
	}
	if strings.TrimSpace(userScript) != "" {
		p.log.Info("running user script", "vm", vm)
		if _, err := p.runScript(ctx, v, pre+userScript, "", userScriptTimeout); err != nil {
			return fmt.Errorf("user script: %w", err)
		}
	}
	return nil
}

func (p *Pool) waitInstallDone(ctx context.Context, ex Exec) error {
	deadline := p.now().Add(installDoneWait)
	for {
		c, cancel := context.WithTimeout(ctx, 30*time.Second)
		out, _, _, err := ex.Run(c, "if (Test-Path 'C:\\rw-install.done') { 'rw-done' } else { 'rw-wait' }")
		cancel()
		if err == nil && strings.Contains(out, "rw-done") {
			return nil
		}
		if !p.now().Before(deadline) {
			return errors.New("guest first-boot setup did not finish")
		}
		if err := p.sleep(ctx, p.cfg.SSHPoll); err != nil {
			return err
		}
	}
}

// Foreign describes an existing dockurr container to adopt.
type Foreign struct {
	Name       string
	Running    bool
	StorageDir string
	OEMDir     string
	Mem        int64 // RAM_SIZE in bytes, 0 if unset/unparseable
	CPUs       int
	DiskSize   string
	Version    string
	Username   string
	Password   string
}

// InspectForeign reads the bind sources and sizing of an existing container.
func (p *Pool) InspectForeign(ctx context.Context, name string) (Foreign, error) {
	insp, err := p.api.ContainerInspect(ctx, name)
	if err != nil {
		return Foreign{}, err
	}
	f := Foreign{Name: strings.TrimPrefix(insp.Name, "/")}
	f.Running = insp.State != nil && insp.State.Running
	for _, m := range insp.Mounts {
		switch m.Destination {
		case "/storage":
			f.StorageDir = m.Source
		case "/oem":
			f.OEMDir = m.Source
		}
	}
	if insp.Config != nil {
		for _, e := range insp.Config.Env {
			k, val, _ := strings.Cut(e, "=")
			switch k {
			case "RAM_SIZE":
				f.Mem = parseRAM(val)
			case "CPU_CORES":
				f.CPUs, _ = strconv.Atoi(val)
			case "DISK_SIZE":
				f.DiskSize = val
			case "VERSION":
				f.Version = val
			case "USERNAME":
				f.Username = val
			case "PASSWORD":
				f.Password = val
			}
		}
	}
	if f.StorageDir == "" {
		return f, errors.New("foreign container has no /storage bind")
	}
	return f, nil
}

func parseRAM(s string) int64 {
	s = strings.TrimSpace(strings.ToUpper(s))
	mult := int64(1)
	switch {
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, strings.TrimSuffix(s, "G")
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, strings.TrimSuffix(s, "M")
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// AdoptResult reports what Adopt found and did.
type AdoptResult struct {
	Foreign        Foreign
	RunnerServices []string // actions.runner.* services stopped and disabled in the guest
}

// Adopt takes over an existing dockurr VM: our labelled container reuses its
// storage (no copy), our key is installed with the password in passwordFile
// (used once), and any actions.runner.* service is stopped and disabled. The
// foreign container is left in place; the caller deregisters the runners and
// removes it. A running foreign container is only stopped when stopForeign.
func (p *Pool) Adopt(ctx context.Context, vm, fromContainer, passwordFile string, stopForeign bool) (AdoptResult, error) {
	var res AdoptResult
	v, err := p.vm(vm)
	if err != nil {
		return res, err
	}
	f, err := p.InspectForeign(ctx, fromContainer)
	if err != nil {
		return res, err
	}
	res.Foreign = f
	if f.StorageDir != v.StorageDir {
		return res, fmt.Errorf("winvm: set storage_dir = %q for vm %q to adopt in place", f.StorageDir, vm)
	}
	if f.Running {
		if !stopForeign {
			return res, ErrForeignAlive
		}
		secs := int(p.cfg.StopTimeout / time.Second)
		if err := p.api.ContainerStop(ctx, f.Name, container.StopOptions{Timeout: &secs}); err != nil {
			return res, err
		}
	}
	pwb, err := os.ReadFile(passwordFile)
	if err != nil {
		return res, err
	}
	password := strings.TrimSpace(string(pwb))
	_, pub, err := EnsureKey(p.cfg.Windows.SSHKeyFile)
	if err != nil {
		return res, err
	}
	if p.passExec == nil {
		return res, errors.New("winvm: no password ssh configured")
	}
	_ = os.Remove(p.knownHosts(vm))

	oem := orDefault(f.OEMDir, v.OEMDir)
	env := p.vmEnv(v, password)

	// Boot our container on the same storage.
	p.mu.Lock()
	if p.licencesLocked(vm) >= p.licenceCap() && !p.up[vm] {
		p.mu.Unlock()
		return res, ErrLicenceCap
	}
	p.up[vm] = true
	p.mu.Unlock()
	name := v.Container(p.cfg.Instance)
	insp, err := p.api.ContainerInspect(ctx, name)
	switch {
	case client.IsErrNotFound(err):
		err = p.createVM(ctx, v, f.StorageDir, oem, env)
	case err == nil:
		if err = ownedBy(insp, p.cfg); err == nil && !(insp.State != nil && insp.State.Running) {
			err = p.api.ContainerStart(ctx, insp.ID, container.StartOptions{})
		}
	}
	if err != nil {
		p.markDown(vm)
		return res, err
	}

	// Password auth once: install our key, then switch to key auth.
	pex := p.passExec(v, password)
	if err := p.waitSSH(ctx, v, pex, p.cfg.SSHReadyTimeout); err != nil {
		return res, err
	}
	c, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_, errOut, code, err := pex.Run(c, installKeyScript(pub))
	cancel()
	if err != nil || code != 0 {
		return res, fmt.Errorf("install ssh key: %v %s", err, tail(errOut, 300))
	}
	kex, err := p.execFor(v)
	if err != nil {
		return res, err
	}
	if err := p.waitSSH(ctx, v, kex, 2*time.Minute); err != nil {
		return res, fmt.Errorf("key auth after install: %w", err)
	}
	out, err := p.runScript(ctx, v, disableRunnerServicesScript, "", 2*time.Minute)
	if err != nil {
		return res, err
	}
	for _, l := range strings.Split(out, "\n") {
		if s, ok := strings.CutPrefix(strings.TrimSpace(l), "service="); ok && s != "" {
			res.RunnerServices = append(res.RunnerServices, s)
		}
	}
	p.mu.Lock()
	p.ready[vm] = true
	p.lastActive[vm] = p.now()
	p.mu.Unlock()
	return res, nil
}
