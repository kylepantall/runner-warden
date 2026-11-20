// Package linux holds Linux host setup: the systemd job slice and read-only
// checks of the Docker daemon configuration.
package linux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Host is the Linux host the setup acts on. The zero value is the real host.
type Host struct {
	Root string // filesystem root prefix; "" = "/" (tests inject a temp dir)
	// Run executes a command; nil runs it for real. Tests inject a recorder.
	Run func(ctx context.Context, name string, args ...string) error
}

func (h Host) path(p string) string { return filepath.Join(h.Root, p) }

func (h Host) run(ctx context.Context, name string, args ...string) error {
	if h.Run != nil {
		return h.Run(ctx, name, args...)
	}
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return nil
}

// SliceUnit renders the slice unit file. MemoryHigh is deliberately never
// written: throttling at a soft limit livelocked a production host, a hard
// MemoryMax fails one job instead. memMax <= 0 omits MemoryMax; swapMax < 0
// omits MemorySwapMax (0 means no swap).
func SliceUnit(memMax, swapMax int64) string {
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=runner-warden job containers\n\n[Slice]\n")
	b.WriteString("MemoryAccounting=yes\nCPUAccounting=yes\nIOAccounting=yes\nTasksAccounting=yes\n")
	if memMax > 0 {
		fmt.Fprintf(&b, "MemoryMax=%d\n", memMax)
	}
	if swapMax >= 0 {
		fmt.Fprintf(&b, "MemorySwapMax=%d\n", swapMax)
	}
	return b.String()
}

// EnsureSlice writes /etc/systemd/system/<name> (e.g.
// "runner-warden-main.slice") and starts it. Idempotent: an unchanged unit is
// not rewritten and triggers no daemon-reload.
func (h Host) EnsureSlice(name string, memMax, swapMax int64) error {
	if !strings.HasSuffix(name, ".slice") || strings.ContainsAny(name, "/ ") {
		return errors.New("slice name must be a bare <name>.slice")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	file := h.path(filepath.Join("/etc/systemd/system", name))
	want := []byte(SliceUnit(memMax, swapMax))
	have, err := os.ReadFile(file) //nolint:gosec // systemd unit path under the host root
	changed := err != nil || !bytes.Equal(have, want)
	if changed {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil { //nolint:gosec // systemd unit dir is world-readable by design
			return err
		}
		tmp := file + ".tmp"
		if err := os.WriteFile(tmp, want, 0o644); err != nil { //nolint:gosec // systemd unit files are world-readable by design
			return err
		}
		if err := os.Rename(tmp, file); err != nil {
			_ = os.Remove(tmp)
			return err
		}
		if err := h.run(ctx, "systemctl", "daemon-reload"); err != nil {
			return err
		}
	}
	return h.run(ctx, "systemctl", "start", name)
}

// EnsureSlice is Host{}.EnsureSlice on the real host.
func EnsureSlice(name string, memMax, swapMax int64) error {
	return Host{}.EnsureSlice(name, memMax, swapMax)
}

// DockerDefaultCgroupParent reads "cgroup-parent" from /etc/docker/daemon.json.
// Report only: setup shows it, never edits it. A missing file or key yields "".
func (h Host) DockerDefaultCgroupParent() (string, error) {
	b, err := os.ReadFile(h.path("/etc/docker/daemon.json"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return "", nil
	}
	var cfg struct {
		CgroupParent string `json:"cgroup-parent"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return "", err
	}
	return cfg.CgroupParent, nil
}

// DockerDefaultCgroupParent is Host{}.DockerDefaultCgroupParent on the real host.
func DockerDefaultCgroupParent() (string, error) { return Host{}.DockerDefaultCgroupParent() }
