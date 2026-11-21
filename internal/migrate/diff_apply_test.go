package migrate

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The printed diff must apply cleanly with git (users pipe it to `git apply`).
func TestUnifiedDiffAppliesWithGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	a := "name: ci\njobs:\n  a:\n    runs-on: [self-hosted, general]\n    steps: []\n  b:\n    runs-on: ubuntu-latest\n"
	b := "name: ci\njobs:\n  a:\n    runs-on: [my-ci-linux]\n    steps: []\n  b:\n    runs-on: ubuntu-latest\n"
	f := filepath.Join(dir, "w.yml")
	if err := os.WriteFile(f, []byte(a), 0o600); err != nil {
		t.Fatal(err)
	}
	patch := filepath.Join(dir, "p.diff")
	if err := os.WriteFile(patch, []byte(unifiedDiff("w.yml", "", a, b)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "apply", "--unsafe-paths", "--directory=.", patch)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git apply: %v\n%s", err, out)
	}
	got, _ := os.ReadFile(f) //nolint:gosec // test temp file
	if string(got) != b {
		t.Fatalf("applied result differs")
	}
}
