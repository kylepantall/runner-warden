package hostsetup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kylepantall/runner-warden/internal/config"
)

const base = `# my config
prefix = "my-ci"

[limits]
memory_budget = "8GiB"

[linux]
enabled = true # the box

[[target]]
org = "o"
os = ["linux"]

[[target]]
repo = "me/tool"

[[target]]
repo = "x/other"
os = ["linux"] # only linux

[windows]
enabled = false
`

func load(t *testing.T, s string) *config.Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("%v\n%s", err, s)
	}
	return c
}

func TestAddMacHost(t *testing.T) {
	out, err := AddHost(base, Host{Kind: KindMac, Name: "mac", Address: "mac.tail", SSHUser: "me", VM: "runner", GuestUID: 501, Targets: []string{"o", "me/tool"}})
	if err != nil {
		t.Fatal(err)
	}
	c := load(t, out)
	if !c.MacOS.Enabled || len(c.MacOS.Hosts) != 1 || c.MacOS.Hosts[0].VM != "runner" {
		t.Fatalf("%+v", c.MacOS)
	}
	got := map[string]string{}
	for _, tg := range c.Targets {
		got[tg.Org+tg.Repo] = strings.Join(tg.OS, ",")
	}
	if got["o"] != "linux,macos" || got["me/tool"] != "linux,macos" || got["x/other"] != "linux" {
		t.Fatalf("target os: %v", got)
	}
	if !strings.Contains(out, "# my config") || !strings.Contains(out, "enabled = true # the box") || !strings.Contains(out, "# only linux") {
		t.Fatal("comments must survive")
	}
	if _, err := AddHost(out, Host{Kind: KindMac, Name: "mac", Address: "b", SSHUser: "u", VM: "v", Targets: []string{"o"}}); err == nil {
		t.Fatal("duplicate name accepted")
	}
}

func TestAddWindowsAndLinuxHosts(t *testing.T) {
	out, err := AddHost(base, Host{Kind: KindWindows, Name: "pc", Address: "pc.tail", SSHUser: "me", Targets: []string{"*"}})
	if err != nil {
		t.Fatal(err)
	}
	out, err = AddHost(out, Host{Kind: KindWSL, Name: "pc-wsl", Address: "pc.tail", SSHUser: "me", Exec: "wsl -d Ubuntu -u root --", MaxConcurrent: 2, Targets: []string{"o"}})
	if err != nil {
		t.Fatal(err)
	}
	c := load(t, out)
	if !c.Windows.Enabled || len(c.Windows.Hosts) != 1 || len(c.Linux.Hosts) != 1 || c.Linux.Hosts[0].Exec != "wsl -d Ubuntu -u root --" {
		t.Fatalf("%+v %+v", c.Windows, c.Linux.Hosts)
	}
	for _, tg := range c.Targets {
		if !strings.Contains(strings.Join(tg.OS, ","), "windows") {
			t.Fatalf("every target serves windows now: %+v", tg)
		}
	}
}

func TestEnableAddsMissingSection(t *testing.T) {
	out := enableSection("prefix = \"p\"\n", "macos")
	if !strings.HasSuffix(out, "[macos]\nenabled = true\n") {
		t.Fatalf("%q", out)
	}
	out = enableSection("[macos]\nssh_key_file = \"/k\"\n", "macos")
	if out != "[macos]\nenabled = true\nssh_key_file = \"/k\"\n" {
		t.Fatalf("%q", out)
	}
}
