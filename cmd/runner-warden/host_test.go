package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/hostsetup"
)

// fakeMachine answers the wizard's SSH commands.
type fakeMachine struct {
	cmds  []string
	fixed bool
}

func (m *fakeMachine) Command(_ context.Context, cmd, stdin string) (string, string, int, error) {
	m.cmds = append(m.cmds, cmd+"\n"+stdin)
	switch {
	case cmd == "echo ok":
		return "ok\n", "", 0, nil
	case strings.HasSuffix(cmd, " list --format json"):
		return `[{"Name":"stopped-vm","Source":"local","Running":false},{"Name":"runner","Source":"local","Running":true}]`, "", 0, nil
	case strings.Contains(cmd, " exec -i runner /bin/sh -s"):
		return "502\nXcode 27.2\n", "", 0, nil
	case cmd == "wsl -l -q":
		return "U\x00b\x00u\x00n\x00t\x00u\x00\r\x00\n\x00", "", 0, nil
	case strings.HasSuffix(cmd, " sh -s") && strings.Contains(stdin, "apt-get install"):
		m.fixed = true
		return "", "", 0, nil
	case strings.HasSuffix(cmd, " sh -s"):
		docker := "docker=down"
		if m.fixed {
			docker = "29.1.3 systemd"
		}
		return "0\nrunning\ndockerd=yes\n" + docker + "\nx86_64\n", "", 0, nil
	case strings.HasPrefix(cmd, "powershell "):
		return "present\n", "", 0, nil
	}
	return "", "unexpected " + cmd, 1, nil
}

const wizCfg = `prefix = "my-ci"
[limits]
memory_budget = "8GiB"
[linux]
enabled = true
[[target]]
org = "o"
`

func newWizard(t *testing.T, in string, f hostAddFlags, m *fakeMachine) (*hostWizard, *bytes.Buffer, *bool) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	cfg := strings.Replace(wizCfg, `prefix = "my-ci"`, "prefix = \"my-ci\"\nstate_dir = "+`"`+dir+`"`, 1)
	if err := os.WriteFile(p, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	out := &bytes.Buffer{}
	restarted := false
	return &hostWizard{
		out: out, in: bufio.NewReader(strings.NewReader(in)), f: f, path: p, inst: "main",
		dial:         func(string, string, string, string) (hostsetup.Commander, error) { return m, nil },
		restart:      func(context.Context) error { restarted = true; return nil },
		setupWindows: func(context.Context, *config.Config, string) error { return nil },
		waitUp:       func(context.Context, string, time.Duration) error { return nil },
	}, out, &restarted
}

func TestWizardLinksAMac(t *testing.T) {
	m := &fakeMachine{}
	// kind 4 (macos), address, user, name (default), Enter after the key step,
	// targets (default), confirm.
	w, out, restarted := newWizard(t, "4\nmac.tail\nme\n\n\n\n\n", hostAddFlags{tart: "/opt/homebrew/bin/tart"}, m)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	c, err := config.Load(w.path)
	if err != nil {
		t.Fatal(err)
	}
	if !c.MacOS.Enabled || len(c.MacOS.Hosts) != 1 {
		t.Fatalf("%+v", c.MacOS)
	}
	h := c.MacOS.Hosts[0]
	if h.Name != "mac" || h.VM != "runner" || h.GuestUID != 502 || h.SSHUser != "me" {
		t.Fatalf("%+v", h)
	}
	if got := strings.Join(c.Targets[0].OS, ","); got != "linux,macos" {
		t.Fatalf("target os %s", got)
	}
	if !*restarted || !strings.Contains(out.String(), "Remote Login") {
		t.Fatalf("restarted=%v\n%s", *restarted, out)
	}
	if bs, _ := filepath.Glob(w.path + ".bak-*"); len(bs) != 1 {
		t.Fatal("no backup of the old config")
	}
}

func TestWizardUnattendedWSLWithFix(t *testing.T) {
	m := &fakeMachine{}
	w, out, _ := newWizard(t, "", hostAddFlags{kind: "wsl", address: "pc.tail", user: "kyle", yes: true, noRestart: true}, m)
	if err := w.run(context.Background()); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !m.fixed {
		t.Fatal("--yes should install Docker Engine")
	}
	c, err := config.Load(w.path)
	if err != nil {
		t.Fatal(err)
	}
	h := c.Linux.Hosts[0]
	if h.Name != "pc-wsl" || h.Exec != "wsl -d Ubuntu -u root --" || h.MaxConcurrent != 2 {
		t.Fatalf("%+v", h)
	}
	if _, err := os.Stat(filepath.Join(c.StateDir, "pc-wsl-setup.ps1")); err != nil {
		t.Fatal("the PowerShell key step is saved for copying")
	}
}

func TestWizardRefusesBadInput(t *testing.T) {
	for name, f := range map[string]hostAddFlags{
		"bad kind": {kind: "bsd", yes: true},
		"no kind":  {yes: true},
		"bad name": {kind: "linux", address: "a", user: "root", name: "Bad Name", yes: true},
	} {
		w, _, _ := newWizard(t, "", f, &fakeMachine{})
		if err := w.run(context.Background()); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}
