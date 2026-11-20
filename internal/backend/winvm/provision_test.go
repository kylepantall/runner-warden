package winvm

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/kylepantall/runner-warden/internal/config"
)

func TestOEMFilesEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := OEMFiles(dir, "ssh-ed25519 AAAA test"); err != nil {
		t.Fatal(err)
	}
	read := func(n string) string {
		b, err := os.ReadFile(filepath.Join(dir, n)) //nolint:gosec // test path under a temp dir
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if !strings.Contains(read("install.bat"), "install.ps1") {
		t.Error("install.bat must call install.ps1")
	}
	ps := read("install.ps1")
	for _, want := range []string{
		"OpenSSH.Server", "sshd", "administrators_authorized_keys", "icacls.exe", "Administrators:F", "SYSTEM:F",
		"DefaultShell", "NoAutoUpdate", "wuauserv", "Set-MpPreference", "powercfg", "LongPathsEnabled",
		"AllowDevelopmentWithoutDevLicense", "New-NetFirewallRule", "authorized_key.pub", "rw-install.done",
	} {
		if !strings.Contains(ps, want) {
			t.Errorf("install.ps1 lacks %s", want)
		}
	}
	if strings.TrimSpace(read("authorized_key.pub")) != "ssh-ed25519 AAAA test" {
		t.Error("public key not written")
	}
	bl := Embedded("baseline.ps1")
	for _, want := range []string{"git-for-windows/git", `\bin`, "jq", "PowerShell/PowerShell", "Inno Setup", "VCTools", "VC.ATL"} {
		if !strings.Contains(bl, want) {
			t.Errorf("baseline lacks %s", want)
		}
	}
	if len(encodePS("$ErrorActionPreference='Stop'\n"+bl)) > maxEncoded {
		t.Error("baseline too large for one ssh command")
	}
}

func TestRandomPassword(t *testing.T) {
	a, err := randomPassword()
	b, _ := randomPassword()
	if err != nil || a == b || len(a) != 24 {
		t.Fatal(a, b, err)
	}
	var up, lo, dg bool
	for _, c := range a {
		up, lo, dg = up || (c >= 'A' && c <= 'Z'), lo || (c >= 'a' && c <= 'z'), dg || (c >= '0' && c <= '9')
	}
	if !up || !lo || !dg {
		t.Fatal("complexity")
	}
}

func TestProvisionFlow(t *testing.T) {
	f := newFixture(t, 3)
	f.pool.cfg.Windows.SSHKeyFile = filepath.Join(f.dir, "key")
	ctx := context.Background()
	if err := f.pool.Provision(ctx, "a", "Write-Output 'custom-step'"); err != nil {
		t.Fatal(err)
	}
	v := f.pool.vms["a"]
	if _, err := os.Stat(filepath.Join(v.OEMDir, "install.bat")); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f.pool.passwordFile("a"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("password file %v %v", st, err)
	}
	pw := f.pool.readPassword("a")
	env := strings.Join(f.api.ctrs["main-win-a"].cfg.Env, " ")
	if !strings.Contains(env, "PASSWORD="+pw) || !strings.Contains(env, "USERNAME=runner") || !strings.Contains(env, "RAM_SIZE=8G") {
		t.Fatalf("env %s", env)
	}
	order := []string{"rw-install.done", "baseline=done", "actions-runner-win-x64", "custom-step"}
	last := -1
	for _, want := range order {
		idx := -1
		for i, s := range f.ex.scripts() {
			if strings.Contains(s, want) {
				idx = i
				break
			}
		}
		if idx <= last {
			t.Fatalf("step %q out of order or missing (%d <= %d)", want, idx, last)
		}
		last = idx
	}
	// Existing disk: refuse, use adopt.
	if err := os.MkdirAll(v.StorageDir, 0o750); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(v.StorageDir, "data.img"), nil, 0o600)
	if err := f.pool.Provision(ctx, "a", ""); !errors.Is(err, ErrStorageNotEmpty) {
		t.Fatalf("got %v", err)
	}
}

func TestAdoptFlow(t *testing.T) {
	f := newFixture(t, 3)
	ctx := context.Background()
	v := f.pool.vms["a"]
	f.pool.cfg.Windows.SSHKeyFile = filepath.Join(f.dir, "key")
	pwFile := filepath.Join(f.dir, "pw")
	_ = os.WriteFile(pwFile, []byte("guest-pass\n"), 0o600)

	f.api.ctrs["windows"] = &fakeCtr{
		id: "foreign", running: true,
		cfg: &container.Config{Env: []string{"RAM_SIZE=12G", "CPU_CORES=6", "DISK_SIZE=96G", "VERSION=11", "USERNAME=runner", "PASSWORD=x"}},
		mounts: []container.MountPoint{
			{Destination: "/storage", Source: v.StorageDir}, {Destination: "/oem", Source: v.OEMDir},
		},
	}
	// Order of auth: password exec first, key exec afterwards.
	pass := &fakeExec{}
	var passUsed string
	f.pool.passExec = func(_ config.WindowsVM, pw string) Exec { passUsed = pw; return pass }

	if _, err := f.pool.Adopt(ctx, "a", "windows", pwFile, false); !errors.Is(err, ErrForeignAlive) {
		t.Fatalf("got %v", err)
	}
	if len(f.api.stopped) != 0 {
		t.Fatal("foreign stopped without confirmation")
	}
	res, err := f.pool.Adopt(ctx, "a", "windows", pwFile, true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Foreign.Name != "windows" || res.Foreign.Mem != 12<<30 || res.Foreign.CPUs != 6 {
		t.Fatalf("%+v", res.Foreign)
	}
	if len(res.RunnerServices) != 1 || res.RunnerServices[0] != "actions.runner.org.old" {
		t.Fatalf("services %v", res.RunnerServices)
	}
	if passUsed != "guest-pass" {
		t.Fatal("password not used")
	}
	if _, ok := pass.find("administrators_authorized_keys"); !ok {
		t.Fatal("key must be installed over the password session")
	}
	if _, ok := f.ex.find("administrators_authorized_keys"); ok {
		t.Fatal("key session must not reinstall")
	}
	if _, ok := f.ex.find("actions.runner.*"); !ok {
		t.Fatal("services disabled over key auth")
	}
	if len(f.api.stopped) != 1 || f.api.stopped[0] != "windows" || len(f.api.removed) != 0 {
		t.Fatalf("stopped=%v removed=%v: foreign container must be stopped, never removed", f.api.stopped, f.api.removed)
	}
	c := f.api.ctrs["main-win-a"]
	if c == nil || c.cfg.Labels["io.github.kylepantall.runner-warden.role"] != "vm" || c.hc.Binds[0] != v.StorageDir+":/storage" {
		t.Fatal("our container must reuse the storage")
	}
	if !f.pool.Up("a") {
		t.Fatal("not up")
	}
	// Mismatched storage path is refused with a hint rather than copied.
	f.api.ctrs["windows"].mounts[0].Source = "/elsewhere"
	if _, err := f.pool.Adopt(ctx, "b", "windows", pwFile, true); err == nil {
		t.Fatal("storage mismatch must fail")
	}
}
