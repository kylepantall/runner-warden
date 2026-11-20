package config

import (
	"os"
	"path/filepath"
	"testing"
)

const sample = `
prefix = "my-ci"

[limits]
memory_budget = "22GiB"

[linux]
enabled = true

[[cache]]
name = "pub"
host_path = "/opt/ci-cache/flutter-pub"
env = "PUB_CACHE"

[[cost]]
match = "acme/app/*/maestro*"
mem = "15GiB"

[[target]]
org = "acme"

[[target]]
repo = "me/tool"
`

func write(t *testing.T, s string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	if c.Instance != "my-ci" || c.StateDir != "/var/lib/runner-warden/my-ci" {
		t.Fatalf("instance defaults: %+v", c)
	}
	if c.Limits.DefaultJobMem == 0 || c.Hang.Idle == 0 {
		t.Fatal("limits/hang defaults missing")
	}
	if c.Caches[0].Mode != "shared" || c.Caches[0].ContainerPath != c.Caches[0].HostPath || c.Caches[0].UID != 1001 {
		t.Fatalf("cache defaults: %+v", c.Caches[0])
	}
	if !c.Targets[0].Core().IsOrg() || c.Targets[1].Core().Repo != "tool" {
		t.Fatal("target conversion")
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"no prefix":          `[limits]` + "\nmemory_budget=\"1GiB\"\n[linux]\nenabled=true\n",
		"no budget":          `prefix="my-ci"` + "\n[linux]\nenabled=true\n",
		"windows no licence": `prefix="my-ci"` + "\n[limits]\nmemory_budget=\"1GiB\"\n[windows]\nenabled=true\n",
		"bad target":         `prefix="my-ci"` + "\n[limits]\nmemory_budget=\"1GiB\"\n[linux]\nenabled=true\n[[target]]\norg=\"a\"\nrepo=\"a/b\"\n",
		"bad cost":           `prefix="my-ci"` + "\n[limits]\nmemory_budget=\"1GiB\"\n[linux]\nenabled=true\n[[cost]]\nmatch=\"a/b\"\nmem=\"1GiB\"\n",
		"unknown key":        `prefix="my-ci"` + "\nbogus=1\n[limits]\nmemory_budget=\"1GiB\"\n[linux]\nenabled=true\n",
	}
	for name, s := range cases {
		if _, err := Load(write(t, s)); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestSaveRoundTrip(t *testing.T) {
	c, err := Load(write(t, sample))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "out.toml")
	if err := c.Save(p); err != nil {
		t.Fatal(err)
	}
	c2, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Limits.MemoryBudget != c.Limits.MemoryBudget || len(c2.Targets) != 2 {
		t.Fatal("round trip mismatch")
	}
}
