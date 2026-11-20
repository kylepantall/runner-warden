package winvm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// Needs a Linux host with KVM, Docker and a provisioned VM. Run with
// RW_WINVM_TESTS=1 and RW_WINVM_CONFIG=<config.toml>; RW_WINVM_NAME picks the
// VM (default: the first). It boots the VM, runs a guest command, and checks
// that cleanup of an unknown runner is a no-op, then stops the VM.
func TestIntegrationVM(t *testing.T) {
	if os.Getenv("RW_WINVM_TESTS") != "1" {
		t.Skip("set RW_WINVM_TESTS=1 to run Windows VM integration tests")
	}
	cfg, err := config.Load(os.Getenv("RW_WINVM_CONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Config{Instance: cfg.Instance, HostID: "it", StateDir: cfg.StateDir, Slice: "runner-warden-" + cfg.Instance + ".slice", Windows: cfg.Windows})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := p.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	vm := os.Getenv("RW_WINVM_NAME")
	if vm == "" {
		vm = cfg.Windows.VMs[0].Name
	}
	if err := p.EnsureUp(ctx, vm); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = p.StopNow(context.Background(), vm) }()
	ex, err := p.execFor(p.vms[vm])
	if err != nil {
		t.Fatal(err)
	}
	out, _, code, err := ex.Run(ctx, "git --version; pwsh -v")
	if err != nil || code != 0 || !strings.Contains(out, "git version") {
		t.Fatalf("baseline missing: %q %d %v", out, code, err)
	}
	if err := p.Cleanup(ctx, "does-not-exist"); err != nil {
		t.Fatal(err)
	}
	_ = core.Windows
}
