package main

import (
	"os"
	"strings"
	"testing"

	"github.com/kylepantall/runner-warden/internal/assets"
	"github.com/kylepantall/runner-warden/internal/migrate"
)

func TestShQuoteNeutralisesHostileInput(t *testing.T) {
	for _, in := range []string{"a'b", "$(touch /tmp/x)", "`id`", "a b;c", "\"$(x)\"", "'; rm -rf /; '"} {
		q := shQuote(in)
		if !strings.HasPrefix(q, "'") || !strings.HasSuffix(q, "'") {
			t.Fatalf("%q -> %q", in, q)
		}
		// outside the quotes only the `'\''` splice may appear
		inner := strings.ReplaceAll(q[1:len(q)-1], `'\''`, "")
		if strings.Contains(inner, "'") {
			t.Fatalf("unescaped quote: %q -> %q", in, q)
		}
	}
}

func TestRollbackLineRejectsHostileNames(t *testing.T) {
	good := migrate.ReRegister{Name: "box-1", Dir: "/home/ci/r", Unit: "actions.runner.acme-app.box-1.service",
		Labels: []string{"self-hosted"}, Args: []string{"./config.sh", "--name", "box-1"}}
	if _, err := rollbackLine(good); err != nil {
		t.Fatal(err)
	}
	for _, mut := range []func(*migrate.ReRegister){
		func(r *migrate.ReRegister) { r.Name = "$(id)" },
		func(r *migrate.ReRegister) { r.Labels = []string{`"$(id)"`} },
		func(r *migrate.ReRegister) { r.Unit = "x; reboot" },
		func(r *migrate.ReRegister) { r.Dir = "relative" },
	} {
		x := good
		mut(&x)
		if _, err := rollbackLine(x); err == nil {
			t.Fatalf("accepted %+v", x)
		}
	}
	// the start target is the recorded unit
	line, _ := rollbackLine(good)
	if !strings.HasSuffix(line, shQuote(good.Unit)) {
		t.Fatal(line)
	}
}

func TestWritePlanTempIsPrivateAndUnique(t *testing.T) {
	a, err := writePlanTemp(map[string]int{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := writePlanTemp(map[string]int{"x": 1})
	defer func() { _ = os.Remove(a) }()
	defer func() { _ = os.Remove(b) }()
	fi, err := os.Stat(a)
	if err != nil || fi.Mode().Perm() != 0o600 || a == b {
		t.Fatal(fi, err, a, b)
	}
}

func TestServiceCommandsRestartOnlyOnChange(t *testing.T) {
	last := func(c [][]string) string { return c[len(c)-1][0] }
	if last(serviceCommands("i", false, true)) != "start" || last(serviceCommands("i", true, true)) != "restart" {
		t.Fatal("verb")
	}
}

func TestServiceCommandsTimer(t *testing.T) {
	for _, on := range []bool{true, false} {
		want := map[bool]string{true: "enable", false: "disable"}[on]
		found := false
		for _, c := range serviceCommands("i", false, on) {
			if c[len(c)-1] == assets.UpdateTimer {
				found = c[0] == want
			}
		}
		if !found {
			t.Fatalf("autoUpdate=%v: want %s of the timer", on, want)
		}
	}
}

func TestRemainingLabelsListsEveryChange(t *testing.T) {
	p := &migrate.Plan{Repos: []migrate.RepoScan{
		{Owner: "o", Repo: "a", Files: []migrate.FileScan{{Path: "ci.yml", Changes: []migrate.Change{{Job: "x", Line: 3}, {Job: "y", Line: 9}}}}},
		{Owner: "o", Repo: "b"},
	}}
	got := remainingLabels(p)
	if len(got) != 2 || got[0].Repo != "o/a" || got[1].Line != 9 {
		t.Fatalf("%+v", got)
	}
	if remainingLabels(&migrate.Plan{}) == nil {
		t.Fatal("empty result must encode as [] not null")
	}
}
