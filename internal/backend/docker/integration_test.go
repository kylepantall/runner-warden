package docker

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Needs a Linux host with Docker and root; run with RW_DOCKER_TESTS=1.
// RW_DOCKER_IMAGE overrides the image (it only has to contain /bin/sh).
func TestIntegrationLifecycle(t *testing.T) {
	if os.Getenv("RW_DOCKER_TESTS") != "1" {
		t.Skip("set RW_DOCKER_TESTS=1 to run Docker integration tests")
	}
	img := os.Getenv("RW_DOCKER_IMAGE")
	if img == "" {
		img = "ghcr.io/actions/actions-runner:latest"
	}
	hook := shortDir(t) + "/hook"
	_ = os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755) //nolint:gosec // the hook must be executable
	b, err := New(Config{
		Instance: "rwtest", Prefix: "rwtest", HostID: "t0", StateDir: shortDir(t), Image: img,
		Slice: "runner-warden-rwtest.slice", RunnerUID: 1001, TmpfsSize: 64 << 20, OOMScoreAdj: 500,
		HookBinary: hook, HookDir: shortDir(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := b.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	spec := core.RunnerSpec{Name: "rwtest-t0-0000000a", JITConfig: "x"}
	_ = os.MkdirAll(b.hookDir(spec.Name), 0o700)
	t.Cleanup(func() { _ = b.Cleanup(context.Background(), spec.Name) })
	h, err := b.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	hs, err := b.List(ctx)
	if err != nil || len(hs) == 0 {
		t.Fatalf("list %v %v", hs, err)
	}
	if _, err := b.Stats(ctx, h); err != nil {
		t.Fatal(err)
	}
	if err := b.Stop(ctx, h, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := b.Cleanup(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if hs, _ := b.List(ctx); len(hs) != 0 {
		t.Fatalf("leftover runners %v", hs)
	}
}

// A running `docker exec` stream through the runner's proxy must survive a
// daemon restart. Needs Linux, systemd, Docker and root; RW_DOCKER_TESTS=1.
// The "restart" drops the Backend and builds a new one over the same state,
// then Adopts the runner, as the daemon does on start.
func TestIntegrationExecSurvivesDaemonRestart(t *testing.T) {
	if os.Getenv("RW_DOCKER_TESTS") != "1" {
		t.Skip("set RW_DOCKER_TESTS=1 to run Docker integration tests")
	}
	if !systemdAvailable() {
		t.Skip("needs systemd (transient units)")
	}
	img := os.Getenv("RW_DOCKER_IMAGE")
	if img == "" {
		img = "ghcr.io/actions/actions-runner:latest"
	}
	// The proxy unit runs this binary's `proxy` command.
	bin := filepath.Join(shortDir(t), "runner-warden")
	if out, err := exec.Command("go", "build", "-o", bin, "../../../cmd/runner-warden").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cfg := Config{
		Instance: "rwtest", Prefix: "rwtest", HostID: "t0", StateDir: shortDir(t), Image: img,
		Slice: "runner-warden-rwtest.slice", RunnerUID: 1001, TmpfsSize: 64 << 20, OOMScoreAdj: 500,
		HookBinary: bin, HookDir: shortDir(t),
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if b.launcher == nil {
		t.Fatal("expected the systemd launcher")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := b.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	spec := core.RunnerSpec{Name: "rwtest-t0-0000000b", JITConfig: "x"}
	if err := os.MkdirAll(b.hookDir(spec.Name), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = b.Cleanup(context.Background(), spec.Name) })
	h, err := b.Start(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	unit := b.proxyUnit(spec.Name) + ".service"
	if out, err := exec.Command("systemctl", "is-active", unit).CombinedOutput(); err != nil {
		t.Fatalf("proxy unit not active: %v %s", err, out)
	}

	// Talk to Docker the way the runner does: through dp.sock.
	cli, err := client.NewClientWithOpts(client.WithHost("unix://"+b.proxySock(spec.Name)), client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	created, err := cli.ContainerCreate(ctx, &container.Config{Image: img, Entrypoint: []string{"/bin/sh", "-c", "sleep 300"}}, nil, nil, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		t.Fatal(err)
	}
	ex, err := cli.ContainerExecCreate(ctx, created.ID, container.ExecOptions{Cmd: []string{"/bin/sh", "-c", "sleep 12; echo survived"}, AttachStdout: true})
	if err != nil {
		t.Fatal(err)
	}
	att, err := cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer att.Close()

	// Daemon restart: a new Backend (the old one's proxies are not in its
	// process) adopts the runner.
	b2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := b2.Adopt(ctx, h); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("systemctl", "is-active", unit).CombinedOutput(); err != nil {
		t.Fatalf("proxy unit not active after restart: %v %s", err, out)
	}

	var out bytes.Buffer
	if _, err := stdcopy.StdCopy(&out, io.Discard, att.Reader); err != nil {
		t.Fatalf("exec stream broke: %v", err)
	}
	if !strings.Contains(out.String(), "survived") {
		t.Fatalf("exec stream ended without its output: %q", out.String())
	}

	if err := b2.Cleanup(ctx, spec.Name); err != nil {
		t.Fatal(err)
	}
	if err := exec.Command("systemctl", "is-active", "--quiet", unit).Run(); err == nil {
		t.Fatal("proxy unit still active after Cleanup")
	}
}
