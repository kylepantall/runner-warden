package docker

import (
	"context"
	"os"
	"testing"
	"time"

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
	_ = os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755)
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
