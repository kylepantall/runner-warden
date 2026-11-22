package winvm

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// newHostFixture is a pool with one local VM "a" and one remote host "pc".
func newHostFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	api := newFakeDocker()
	ex := &fakeExec{}
	p, err := newPool(Config{
		Instance: "main", HostID: "h1", StateDir: filepath.Join(dir, "state"), Slice: "runner-warden-main.slice",
		SSHReadyTimeout: 50 * time.Millisecond, StopSSHTimeout: 20 * time.Millisecond, SSHPoll: time.Millisecond,
		HostProbe: 50 * time.Millisecond,
		Windows: config.Windows{
			Enabled: true, MaxConcurrent: 1, Image: "dockurr/windows", SSHUser: "runner",
			SSHKeyFile: filepath.Join(dir, "key"), RunnerDir: `C:\actions-runner`, WorkDir: `C:\rw`,
			RunnerVersion: "2.999.0", IdleTTL: config.Duration(time.Minute),
			VMs: []config.WindowsVM{{
				Name: "a", Targets: []string{"*"}, Mem: 8 << 30, CPUs: 4, Version: "11", DiskSize: "96G", SSHPort: 2222,
				StorageDir: filepath.Join(dir, "vm", "a", "storage"), OEMDir: filepath.Join(dir, "vm", "a", "oem"),
			}},
			Hosts: []config.WindowsHost{{Name: "pc", Address: "pc.tail", SSHUser: "kyle", Targets: []string{"*"}}},
		},
	}, api, func(config.WindowsVM) (Exec, error) { return ex, nil })
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{t: t, pool: p, api: api, ex: ex, dir: dir}
}

func TestHostMembers(t *testing.T) {
	f := newHostFixture(t)
	v := f.pool.vms["pc"]
	if !v.IsRemote() || sshAddr(v) != "pc.tail:22" || sshUser(f.pool.cfg.Windows, v) != "kyle" {
		t.Fatalf("%+v", v)
	}
	if f.pool.MemFor("pc") != 0 || !f.pool.IsRemote("pc") || f.pool.IsRemote("a") {
		t.Fatal("remote host must hold no memory here")
	}
	if c := f.pool.Caps(); c.MaxConcurrent != 2 {
		t.Fatalf("one licence plus one host: %+v", c)
	}
}

func TestPickUsesHostOnlyWhenUp(t *testing.T) {
	f := newHostFixture(t)
	tgt := core.Target{Owner: "o"}
	if vm, ok := f.pool.Pick(tgt); !ok || vm != "a" {
		t.Fatalf("down host: boot the VM, got %q %v", vm, ok)
	}
	if vm, ok := f.pool.Pick(tgt); ok {
		t.Fatalf("a down host is never booted, got %q", vm)
	}
	f.setUp("pc", time.Now())
	if vm, ok := f.pool.Pick(tgt); !ok || vm != "pc" {
		t.Fatalf("up host takes the job, got %q %v", vm, ok)
	}
	// The host uses no licence: with "a" released, it can still boot.
	f.pool.Release("a")
	if vm, ok := f.pool.Pick(tgt); !ok || vm != "a" {
		t.Fatalf("licence counted the host: %q %v", vm, ok)
	}
}

func TestProbeMarksHostUpAndDown(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()
	f.ex.handler = func(string, string) (string, string, int, error) { return "", "", -1, errors.New("no route") }
	if err := f.pool.EnsureUp(ctx, "pc"); !errors.Is(err, ErrHostDown) || f.pool.Up("pc") {
		t.Fatalf("unreachable: %v up=%v", err, f.pool.Up("pc"))
	}
	f.ex.handler = nil
	f.pool.StopIdle(ctx, time.Now().Add(time.Hour))
	if !f.pool.Up("pc") || !f.pool.HostsUp()["pc"] {
		t.Fatal("StopIdle should re-probe a down host")
	}
	if len(f.api.created)+len(f.api.started)+len(f.api.stopped) != 0 {
		t.Fatal("a host has no container to touch")
	}
	// An idle reachable host is never stopped.
	if stopped := f.pool.StopIdle(ctx, time.Now().Add(time.Hour)); len(stopped) != 0 || !f.pool.Up("pc") {
		t.Fatalf("stopped %v", stopped)
	}
}

func TestStartOnHostUsesS4UAndGuestStats(t *testing.T) {
	f := newHostFixture(t)
	ctx := context.Background()
	f.ex.handler = func(s, _ string) (string, string, int, error) {
		if strings.Contains(s, "state=$state") {
			return "state=Running procs=2 log=10 diag=5 cpu=321 mem=4096", "", 0, nil
		}
		return defaultGuest(s)
	}
	h, err := f.pool.Start(ctx, spec("rw-h1-pc", "pc"))
	if err != nil {
		t.Fatal(err)
	}
	js, ok := f.ex.find("Register-ScheduledTask")
	if !ok || !strings.Contains(js.script, "S4U") {
		t.Fatal("host jobs default to an S4U task")
	}
	u, err := f.pool.Stats(ctx, h)
	if err != nil || !u.Alive || u.CPUSeconds != 321 || u.MemCurrent != 4096 || u.LogBytes != 15 {
		t.Fatalf("%+v %v", u, err)
	}
	if len(f.api.created) != 0 {
		t.Fatal("started a container for a host")
	}
}

func TestProvisionRefusesHost(t *testing.T) {
	f := newHostFixture(t)
	if err := f.pool.Provision(context.Background(), "pc", ""); !errors.Is(err, ErrRemote) {
		t.Fatal(err)
	}
	if !strings.Contains(HostScript("ssh-ed25519 AAAA k"), "administrators_authorized_keys") {
		t.Fatal("host script must trust the key")
	}
}

func TestPickNamed(t *testing.T) {
	f := newHostFixture(t)
	tgt := core.Target{Owner: "o"}
	if _, ok := f.pool.PickNamed("pc", tgt); ok {
		t.Fatal("a down host is never picked")
	}
	if _, _, remote, idle := f.pool.PeekNamed("pc"); !remote || idle {
		t.Fatal("down host: remote, not idle")
	}
	f.setUp("pc", time.Now())
	if vm, ok := f.pool.PickNamed("pc", tgt); !ok || vm != "pc" {
		t.Fatalf("%q %v", vm, ok)
	}
	if _, ok := f.pool.PickNamed("pc", tgt); ok {
		t.Fatal("claimed host picked twice")
	}
	if _, ok := f.pool.PickNamed("nope", tgt); ok {
		t.Fatal("unknown vm")
	}
	if mem, up, _, idle := f.pool.PeekNamed("a"); up || !idle || mem == 0 {
		t.Fatal("stopped local vm can boot")
	}
}

func TestEnsurePackages(t *testing.T) {
	f := newHostFixture(t)
	f.pool.cfg.Windows.Packages = []config.WindowsPackage{{ID: "Microsoft.VisualStudio.2022.BuildTools", Override: "--quiet --add Microsoft.VisualStudio.Workload.VCTools"}}
	f.setUp("pc", time.Now())
	if got := f.pool.PackagesPending(); len(got) != 1 || got[0] != "pc" || !f.pool.NeedsPackages("pc") {
		t.Fatalf("pending %v", got)
	}
	fail := true
	f.ex.handler = func(s, _ string) (string, string, int, error) {
		if strings.Contains(s, "winget list") {
			if fail {
				return "failed Microsoft.VisualStudio.2022.BuildTools -1\n", "", 0, nil
			}
			return "installed Microsoft.VisualStudio.2022.BuildTools\n", "", 0, nil
		}
		return defaultGuest(s)
	}
	if _, err := f.pool.EnsurePackages(context.Background(), "pc"); err == nil || !f.pool.NeedsPackages("pc") {
		t.Fatal("a failed install is reported and retried")
	}
	fail = false
	got, err := f.pool.EnsurePackages(context.Background(), "pc")
	if err != nil || len(got) != 1 || f.pool.NeedsPackages("pc") || len(f.pool.PackagesPending()) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	js, _ := f.ex.find("winget list")
	if !strings.Contains(js.script, "ov = '--quiet --add Microsoft.VisualStudio.Workload.VCTools'") {
		t.Fatal("override not passed")
	}
	if got, err := f.pool.EnsurePackages(context.Background(), "pc"); err != nil || got != nil {
		t.Fatal("ensured packages are not re-run")
	}
}

func TestHostRunsSeveralJobs(t *testing.T) {
	f := newHostFixture(t)
	f.pool.vms["pc"] = func() config.WindowsVM { v := f.pool.vms["pc"]; v.Slots = 2; return v }()
	f.setUp("pc", time.Now())
	tgt := core.Target{Owner: "o"}
	ctx := context.Background()
	for i, name := range []string{"rw-h1-pc", "rw-h1-pc2"} {
		vm, ok := f.pool.PickNamed("pc", tgt)
		if !ok {
			t.Fatalf("slot %d not free", i+1)
		}
		s := spec(name, vm)
		if _, err := f.pool.Start(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := f.pool.PickNamed("pc", tgt); ok {
		t.Fatal("both slots are taken")
	}
	starts := 0
	for _, c := range f.ex.calls {
		if strings.Contains(c.script, "Register-ScheduledTask") {
			starts++
			if strings.Contains(c.script, "Get-ScheduledTask -TaskName 'rw-*'") {
				t.Fatal("a shared host must not sweep the other job")
			}
			if starts == 2 && !strings.Contains(c.script, `C:\actions-runner-s2`) {
				t.Fatal("second job gets its own runner install")
			}
		}
	}
	if starts != 2 {
		t.Fatalf("starts %d", starts)
	}
	if err := f.pool.Cleanup(ctx, "rw-h1-pc"); err != nil {
		t.Fatal(err)
	}
	if js, _ := f.ex.find("Unregister-ScheduledTask -TaskName $tn"); strings.Contains(js.script, "$env:TEMP") {
		t.Fatal("never wipe the user's TEMP on a host")
	}
	if _, ok := f.pool.PickNamed("pc", tgt); !ok {
		t.Fatal("a freed slot takes the next job")
	}
}
