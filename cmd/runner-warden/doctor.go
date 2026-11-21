package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	dockerclient "github.com/docker/docker/client"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/naming"
	"github.com/kylepantall/runner-warden/internal/platform/linux"
)

type checkLevel int

const (
	ok checkLevel = iota
	warn
	fail
)

type checkResult struct {
	Name   string
	Level  checkLevel
	Detail string
}

func failed(rs []checkResult) bool {
	for _, r := range rs {
		if r.Level == fail {
			return true
		}
	}
	return false
}

func printChecks(w io.Writer, rs []checkResult) {
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	for _, r := range rs {
		mark := "✅"
		switch r.Level {
		case warn:
			mark = "⚠️ "
		case fail:
			mark = "❌"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", mark, r.Name, r.Detail)
	}
	tw.Flush()
}

// doctor runs host checks; live also checks the installed service.
func doctor(ctx context.Context, cfg *config.Config, live bool) []checkResult {
	var rs []checkResult
	add := func(name string, l checkLevel, format string, a ...any) {
		rs = append(rs, checkResult{Name: name, Level: l, Detail: fmt.Sprintf(format, a...)})
	}
	if os.Geteuid() == 0 {
		add("root", ok, "running as root")
	} else {
		add("root", fail, "setup and the service need root")
	}
	sock := "/var/run/docker.sock"
	if cfg != nil {
		sock = cfg.Linux.DockerSocket
	}
	if dc, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+sock), dockerclient.WithAPIVersionNegotiation()); err == nil {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		info, err := dc.Info(pctx)
		cancel()
		if err != nil {
			add("docker", fail, "not reachable at %s: %v", sock, err)
		} else {
			lvl := ok
			if info.CgroupVersion != "2" {
				lvl = fail
			}
			add("docker", lvl, "%s, cgroup v%s, driver %s", info.ServerVersion, info.CgroupVersion, info.CgroupDriver)
		}
	} else {
		add("docker", fail, "%v", err)
	}
	if _, err := os.Stat("/sys/fs/cgroup/cgroup.controllers"); err == nil {
		add("cgroup v2", ok, "unified hierarchy")
	} else {
		add("cgroup v2", fail, "cgroup v2 unified hierarchy required")
	}
	if _, err := os.Stat("/run/systemd/system"); err == nil {
		add("systemd", ok, "present")
	} else {
		add("systemd", fail, "systemd required")
	}
	if p, err := linux.DockerDefaultCgroupParent(); err == nil && p != "" {
		add("dockerd cgroup-parent", warn, "daemon.json sets %q for all containers; job containers still go to the instance slice", p)
	}
	if _, err := os.Stat("/dev/kvm"); err == nil {
		add("kvm", ok, "/dev/kvm present")
	} else if cfg != nil && cfg.Windows.Enabled {
		add("kvm", fail, "/dev/kvm required for Windows VMs")
	} else {
		add("kvm", warn, "/dev/kvm missing (emulator and Windows jobs need it)")
	}
	if cfg == nil {
		return rs
	}
	dir := cfg.StateDir
	for dir != "/" {
		if _, err := os.Stat(dir); err == nil {
			break
		}
		dir = parentDir(dir)
	}
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		freePct := float64(st.Bavail) / float64(st.Blocks) * 100
		lvl := ok
		if freePct < 15 {
			lvl = warn
		}
		add("disk", lvl, "%.0f%% free on %s", freePct, dir)
	}
	if _, err := gh.Token(cfg.GitHub.TokenFile); err != nil {
		lvl := warn
		if live {
			lvl = fail
		}
		add("token", lvl, "%v", err)
	} else {
		add("token", ok, "%s", cfg.GitHub.TokenFile)
	}
	if !live {
		return rs
	}
	if out, _ := exec.CommandContext(ctx, "systemctl", "is-active", naming.Slice(cfg.Instance)).Output(); strings.TrimSpace(string(out)) == "active" {
		add("job slice", ok, "%s active", naming.Slice(cfg.Instance))
	} else {
		add("job slice", fail, "%s not active", naming.Slice(cfg.Instance))
	}
	unit := "runner-warden@" + cfg.Instance
	if out, _ := exec.CommandContext(ctx, "systemctl", "is-active", unit).Output(); strings.TrimSpace(string(out)) == "active" {
		add("service", ok, "%s active", unit)
	} else {
		add("service", fail, "%s not active (journalctl -u %s)", unit, unit)
	}
	cl, err := controlClient()
	if err == nil {
		var rep daemon.StatusReport
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = cl.Status(sctx, &rep)
		cancel()
		if err != nil {
			add("daemon", fail, "control socket: %v", err)
		} else {
			add("daemon", ok, "%d lanes, %d runners, %d held", len(rep.Scheduler.Lanes), len(rep.Runners), rep.Scheduler.Held)
			for _, r := range rep.Runners {
				if r.OOM > 0 {
					add("oom", warn, "runner %s had %d OOM kills", r.Name, r.OOM)
				}
			}
		}
	}
	return rs
}

func parentDir(p string) string {
	i := strings.LastIndex(strings.TrimRight(p, "/"), "/")
	if i <= 0 {
		return "/"
	}
	return p[:i]
}
