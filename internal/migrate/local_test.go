package migrate

import (
	"os"
	"path/filepath"
	"testing"
)

func mk(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const unitText = "[Service]\nExecStart=/home/ci/actions-runner/runsvc.sh\nWorkingDirectory=/home/ci/actions-runner\n"

func TestScanLocal(t *testing.T) {
	root := t.TempDir()
	mk(t, root, "etc/systemd/system/actions.runner.acme-app.box-1.service", unitText)
	mk(t, root, "etc/systemd/system/unrelated.service", "x")
	mk(t, root, "home/ci/actions-runner/.runner", "\xef\xbb\xbf"+`{"agentName":"box-1","poolName":"Default","gitHubUrl":"https://github.com/acme/app"}`)
	mk(t, root, "home/ci/actions-runner/.env", "LANG=C\nRUNNER_LABELS=self-hosted, general\n")
	mk(t, root, "home/ci/actions-runner/_work/x/.runner", `{"agentName":"nested"}`)
	mk(t, root, "opt/r2/.runner", `{"agentName":"r2","gitHubUrl":"https://github.com/acme"}`)
	mk(t, root, "srv/a/b/c/d/.runner", `{"agentName":"too-deep"}`)

	got, err := ScanLocal(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]LocalRunner{}
	for _, l := range got {
		by[l.Name] = l
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	b1 := by["box-1"]
	if b1.Unit != "actions.runner.acme-app.box-1.service" || b1.Dir != "/home/ci/actions-runner" || b1.URL != "https://github.com/acme/app" ||
		b1.Pool != "Default" || len(b1.Labels) != 2 || b1.Labels[1] != "general" || b1.UnitPath != "/etc/systemd/system/"+b1.Unit {
		t.Fatalf("%+v", b1)
	}
	if r2 := by["r2"]; r2.Dir != "/opt/r2" || r2.Unit != "" {
		t.Fatalf("%+v", r2)
	}
}

func TestTargetFromURL(t *testing.T) {
	if tg, ok := TargetFromURL("https://github.com/acme"); !ok || !tg.IsOrg() || tg.Owner != "acme" {
		t.Fatal(tg, ok)
	}
	if tg, ok := TargetFromURL("https://github.com/acme/app"); !ok || tg.IsOrg() || tg.Repo != "app" {
		t.Fatal(tg, ok)
	}
	if _, ok := TargetFromURL("https://github.com/"); ok {
		t.Fatal("empty path must fail")
	}
}
