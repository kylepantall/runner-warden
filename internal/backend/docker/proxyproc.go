package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/dockerproxy"
)

// proxyLauncher runs `runner-warden proxy --spec <file>` as a unit that is
// independent of the daemon process.
type proxyLauncher interface {
	// Launch starts the proxy unit; it returns once the unit is started, not
	// once the socket is up.
	Launch(ctx context.Context, unit, specPath string) error
	// Stop stops the unit. A unit that does not exist is not an error.
	Stop(ctx context.Context, unit string) error
}

const (
	proxySockWait  = 10 * time.Second
	proxyDialLimit = time.Second
)

// proxyUnit is the transient systemd unit serving a runner's proxy.
func (b *Backend) proxyUnit(runner string) string {
	return "runner-warden-" + b.cfg.Instance + "-proxy-" + runner
}

// proxySpecPath is deliberately outside the runner dir: jobs own that dir
// (the runner uid can replace files in it), and the spec decides which host
// paths the proxy lets jobs mount; the unit re-reads it when systemd restarts it.
func (b *Backend) proxySpecPath(runner string) string {
	return filepath.Join(b.cfg.StateDir, "proxy", runner+".json")
}

func (b *Backend) proxyConfig(runner, sock string, allowed []string) dockerproxy.Config {
	return dockerproxy.Config{
		Socket: sock, Upstream: b.cfg.DockerSocket,
		Runner: runner, Instance: b.cfg.Instance, HostID: b.cfg.HostID,
		Slice: b.cfg.Slice, OOMScoreAdj: b.cfg.OOMScoreAdj,
		CacheBinds: b.cfg.CacheBinds, CacheEnv: b.cfg.CacheEnv, AllowedBinds: allowed, PersistentNames: b.cfg.PersistentNames,
		PullLock: PullLockPath(b.cfg.StateDir),
		Log:      b.log,
	}
}

// startProxy serves the runner's proxy on sock: as its own unit when a
// launcher is available, else inside this process.
func (b *Backend) startProxy(ctx context.Context, runner, sock string, allowed ...string) error {
	if err := mkdirAll(filepath.Dir(sock)); err != nil {
		return err
	}
	cfg := b.proxyConfig(runner, sock, allowed)
	if b.launcher != nil {
		return b.launchProxy(ctx, runner, cfg)
	}
	p, err := dockerproxy.New(cfg)
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

func (b *Backend) launchProxy(ctx context.Context, runner string, cfg dockerproxy.Config) error {
	unit, specPath := b.proxyUnit(runner), b.proxySpecPath(runner)
	// A previous unit (crash loop, leftover) must be gone before the new one
	// takes over the socket.
	if err := b.launcher.Stop(ctx, unit); err != nil {
		b.log.Warn("stopping previous proxy unit failed", "runner", runner, "error", err.Error())
	}
	spec := dockerproxy.Spec{Config: cfg, RunnerContainer: runner, LogLevel: b.cfg.LogLevel}
	if err := dockerproxy.WriteSpec(specPath, spec); err != nil {
		return fmt.Errorf("proxy spec: %w", err)
	}
	if err := b.launcher.Launch(ctx, unit, specPath); err != nil {
		return fmt.Errorf("start proxy: %w", err)
	}
	if err := waitForSocket(ctx, cfg.Socket, proxySockWait); err != nil {
		_ = b.launcher.Stop(context.WithoutCancel(ctx), unit)
		return fmt.Errorf("proxy socket: %w", err)
	}
	return nil
}

// stopProxyUnit stops the runner's proxy unit and drops its spec.
func (b *Backend) stopProxyUnit(ctx context.Context, runner string) error {
	if b.launcher == nil {
		return nil
	}
	err := b.launcher.Stop(ctx, b.proxyUnit(runner))
	if rerr := os.Remove(b.proxySpecPath(runner)); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
		err = errors.Join(err, rerr)
	}
	return err
}

// proxyAlive reports whether something accepts connections on sock.
func proxyAlive(sock string) bool {
	c, err := net.DialTimeout("unix", sock, proxyDialLimit)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func waitForSocket(ctx context.Context, sock string, limit time.Duration) error {
	deadline := time.NewTimer(limit)
	defer deadline.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if proxyAlive(sock) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("not accepting connections in time")
		case <-tick.C:
		}
	}
}

// --- systemd ---

// systemdAvailable reports whether proxies can run as transient system units:
// systemd present and we are root (the daemon always is; unprivileged test
// runs fall back to in-process proxies).
func systemdAvailable() bool {
	if os.Geteuid() != 0 {
		return false
	}
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return false
	}
	_, err := os.Stat("/run/systemd/system")
	return err == nil
}

type systemdLauncher struct{ exe string }

func (l systemdLauncher) Launch(ctx context.Context, unit, specPath string) error {
	args := []string{
		"--unit=" + unit, "--collect", "--quiet",
		"--description=runner-warden Docker proxy " + unit,
		"--property=KillMode=mixed", "--property=Restart=on-failure", "--property=RestartSec=1",
		"--", l.exe, "proxy", "--spec", specPath,
	}
	if out, err := exec.CommandContext(ctx, "systemd-run", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("systemd-run: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// notLoaded is systemctl's exit status for a unit that does not exist.
const notLoaded = 5

func (systemdLauncher) Stop(ctx context.Context, unit string) error {
	out, err := exec.CommandContext(ctx, "systemctl", "stop", unit+".service").CombinedOutput()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == notLoaded {
		return nil
	}
	if err != nil {
		return fmt.Errorf("systemctl stop: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
