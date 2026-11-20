package migrate

import (
	"context"
	"strings"
	"testing"

	"github.com/kylepantall/runner-warden/internal/core"
)

const wfOld = "jobs:\n  a:\n    runs-on: [self-hosted, general]\n    steps: []\n  b:\n    runs-on: ${{ fromJSON(vars.CI_RUNNER || '[\"self-hosted\"]') }}\n    steps: []\n"
const wfNew = "jobs:\n  a:\n    runs-on: [my-ci-linux]\n    steps: []\n  b:\n    runs-on: ${{ fromJSON(vars.CI_RUNNER || '\"my-ci-linux\"') }}\n    steps: []\n"

func fixture() (*fakeGH, ScanInput) {
	f := newFake()
	f.addRepo("acme", "app", false, map[string]string{".github/workflows/ci.yml": wfOld, ".github/workflows/lint.yml": "jobs:\n  l:\n    runs-on: ubuntu-latest\n"})
	f.addRepo("acme", "old", true, map[string]string{".github/workflows/ci.yml": wfOld})
	f.addRepo("acme", "clean", false, map[string]string{".github/workflows/x.yml": "jobs:\n  l:\n    runs-on: ubuntu-latest\n"})
	f.vars["acme|CI_RUNNER"] = `["self-hosted","isolink"]`
	return f, ScanInput{Targets: []core.Target{{Owner: "acme"}}, Rules: testRules, GH: f, Root: "/nonexistent"}
}

func TestScanOrgSkipsArchivedAndCollectsVars(t *testing.T) {
	f, in := fixture()
	p, err := Scan(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Repos) != 2 {
		t.Fatalf("repos %d (archived must be skipped)", len(p.Repos))
	}
	var app RepoScan
	for _, r := range p.Repos {
		if r.Repo == "app" {
			app = r
		}
	}
	if !app.HasChanges() || len(app.Files) != 2 {
		t.Fatalf("%+v", app)
	}
	if len(p.Vars) != 1 || p.Vars[0].Name != "CI_RUNNER" || p.Vars[0].New != `"my-ci-linux"` || !p.Vars[0].Scope.IsOrg() || len(p.Vars[0].UsedBy) != 1 {
		t.Fatalf("vars %+v", p.Vars)
	}
	_ = f
}

func TestApplyPrintPRPushIdempotent(t *testing.T) {
	ctx := context.Background()
	f, in := fixture()
	plan, err := Scan(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// print: nothing changes
	res, err := Apply(ctx, plan, ApplyOptions{Mode: ModePrint, GH: f, UpdateVars: true})
	if err != nil || len(res.Repos) != 1 || res.Repos[0].Diff == "" || f.commits != 0 || len(res.Vars) != 0 {
		t.Fatalf("print: %+v %v commits=%d", res, err, f.commits)
	}
	if !strings.Contains(res.Repos[0].Diff, "-    runs-on: [self-hosted, general]") || !strings.Contains(res.Repos[0].Diff, "+    runs-on: [my-ci-linux]") {
		t.Fatalf("diff: %s", res.Repos[0].Diff)
	}
	// pr: branch + one commit + PR, vars untouched without the flag
	res, err = Apply(ctx, plan, ApplyOptions{Mode: ModePR, GH: f})
	if err != nil || res.Repos[0].Error != "" || !res.Repos[0].Changed || res.Repos[0].PRNumber != 1 || res.Repos[0].Branch != DefaultBranchName {
		t.Fatalf("pr: %+v %v", res, err)
	}
	if got := f.branches["acme/app@"+DefaultBranchName][".github/workflows/ci.yml"]; got != wfNew {
		t.Fatalf("branch content %q", got)
	}
	if f.files["acme/app"][".github/workflows/ci.yml"] != wfOld {
		t.Fatal("default branch must not change in pr mode")
	}
	if f.vars["acme|CI_RUNNER"] != `["self-hosted","isolink"]` {
		t.Fatal("var changed without flag")
	}
	// second run: same branch/PR reused, no new commit
	commits := f.commits
	res, err = Apply(ctx, plan, ApplyOptions{Mode: ModePR, GH: f})
	if err != nil || res.Repos[0].Changed || res.Repos[0].PRNumber != 1 || f.commits != commits || len(f.prs) != 1 {
		t.Fatalf("rerun: %+v %v", res, err)
	}
	// push + vars
	res, err = Apply(ctx, plan, ApplyOptions{Mode: ModePush, GH: f, UpdateVars: true})
	if err != nil || !res.Repos[0].Changed || f.files["acme/app"][".github/workflows/ci.yml"] != wfNew {
		t.Fatalf("push: %+v %v", res, err)
	}
	if len(res.Vars) != 1 || !res.Vars[0].Updated || f.vars["acme|CI_RUNNER"] != `"my-ci-linux"` {
		t.Fatalf("vars: %+v", res.Vars)
	}
	// var drift is reported, not overwritten
	f.vars["acme|CI_RUNNER"] = "someone-else"
	res, _ = Apply(ctx, plan, ApplyOptions{Mode: ModePush, GH: f, UpdateVars: true})
	if len(res.Vars) != 1 || res.Vars[0].Updated || res.Vars[0].Error == "" || f.vars["acme|CI_RUNNER"] != "someone-else" {
		t.Fatalf("drift: %+v", res.Vars)
	}
}

func TestApplyRejectsUnknownMode(t *testing.T) {
	if _, err := Apply(context.Background(), &Plan{}, ApplyOptions{Mode: "x"}); err == nil {
		t.Fatal("want error")
	}
}

func TestUnifiedDiffShape(t *testing.T) {
	d := unifiedDiff("f", "", "a\nb\nc\nd\n", "a\nB\nc\nd\n")
	for _, want := range []string{"--- a/f", "+++ b/f", "@@ -1,4 +1,4 @@", "-b", "+B"} {
		if !strings.Contains(d, want) {
			t.Errorf("diff missing %q:\n%s", want, d)
		}
	}
	if unifiedDiff("f", "", "x\n", "x\n") != "" {
		t.Error("identical inputs must give empty diff")
	}
}
