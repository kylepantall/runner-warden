package migrate

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kylepantall/runner-warden/internal/core"
)

var update = flag.Bool("update", false, "rewrite golden files")

var testRules = Rules{
	Prefix:        "my-ci",
	LegacyLabels:  []string{"home-ci", "general", "isolink", "kscript", "tart"},
	BareHost:      true,
	WindowsLabels: []string{"isolink-windows"},
}

func golden(t *testing.T, name string) (Result, []byte) {
	t.Helper()
	in, err := os.ReadFile(filepath.Join("testdata", name+".in.yml")) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	res := Analyze(in, testRules)
	gp := filepath.Join("testdata", name+".golden.yml")
	if *update {
		if err := os.WriteFile(gp, res.Out, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(gp) //nolint:gosec // test fixture path
	if err != nil {
		t.Fatal(err)
	}
	if string(res.Out) != string(want) {
		t.Fatalf("output differs from %s:\n%s", gp, unifiedDiff(gp, "got", string(want), string(res.Out)))
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(res.Out, &doc); err != nil {
		t.Fatalf("output is not valid YAML: %v", err)
	}
	return res, in
}

func changeFor(res Result, job string) []Change {
	var out []Change
	for _, c := range res.Changes {
		if c.Job == job {
			out = append(out, c)
		}
	}
	return out
}

func skipsFor(res Result, job string) []Skip {
	var out []Skip
	for _, c := range res.Skipped {
		if c.Job == job {
			out = append(out, c)
		}
	}
	return out
}

func TestGoldenForms(t *testing.T) {
	res, in := golden(t, "forms")
	want := map[string]core.OS{
		"flow4": core.Linux, "flow2": core.Linux, "plain": core.Linux, "quoted": core.Linux,
		"windows": core.Windows, "block": core.Linux, "mapping": core.Linux,
	}
	for job, os := range want {
		cs := changeFor(res, job)
		if len(cs) != 1 || cs[0].OS != os {
			t.Errorf("job %s: changes %+v", job, cs)
		}
	}
	if c := changeFor(res, "flow4")[0]; c.Form != FormSequence || c.New != "my-ci-linux" {
		t.Errorf("flow4 %+v", c)
	}
	if c := changeFor(res, "windows")[0]; c.New != "my-ci-windows" {
		t.Errorf("windows %+v", c)
	}
	for job, r := range map[string]Reason{"mac": ReasonMacOS, "grouponly": ReasonGroupOnly, "alias": ReasonUnsupportedForm} {
		sk := skipsFor(res, job)
		if len(sk) != 1 || sk[0].Reason != r || len(changeFor(res, job)) != 0 {
			t.Errorf("job %s: skips %+v", job, sk)
		}
	}
	un := map[string]bool{}
	for _, u := range res.Untouched {
		un[u.Job] = true
	}
	for _, job := range []string{"hosted", "hostedwin", "migrated"} {
		if !un[job] {
			t.Errorf("job %s should be untouched", job)
		}
	}
	// Everything outside the edited spans is byte-identical: re-analysing the
	// output changes nothing.
	again := Analyze(res.Out, testRules)
	if len(again.Changes) != 0 {
		t.Errorf("not idempotent: %+v", again.Changes)
	}
	if len(res.Out) >= len(in)+1000 {
		t.Error("output grew implausibly")
	}
}

func TestGoldenExpressions(t *testing.T) {
	res, _ := golden(t, "expr")
	if c := changeFor(res, "fork"); len(c) != 1 || c[0].Form != FormExpression || c[0].Old != "'home-ci'" || c[0].New != "'my-ci-linux'" {
		t.Errorf("fork %+v", c)
	}
	c := changeFor(res, "fromjson")
	if len(c) != 1 || c[0].Form != FormExpressionJSON || c[0].New != `'"my-ci-linux"'` {
		t.Errorf("fromjson %+v", c)
	}
	sk := skipsFor(res, "fromjson")
	if len(sk) != 1 || sk[0].Reason != ReasonVariable || sk[0].Var != "CI_RUNNER" {
		t.Errorf("fromjson skips %+v", sk)
	}
	if len(changeFor(res, "varonly")) != 0 {
		t.Error("varonly must stay untouched")
	}
	if sk := skipsFor(res, "varonly"); len(sk) != 1 || sk[0].Var != "TEST_RUNNER_NAME" {
		t.Errorf("varonly skips %+v", sk)
	}
	for _, job := range []string{"compare", "fncall"} {
		if len(changeFor(res, job)) != 0 {
			t.Errorf("%s must not change", job)
		}
	}
	if sk := skipsFor(res, "compare"); len(sk) != 1 || sk[0].Reason != ReasonUnsupportedExpr {
		t.Errorf("compare skips %+v", sk)
	}
	if c := changeFor(res, "winjson"); len(c) != 1 || c[0].OS != core.Windows || c[0].New != `'"my-ci-windows"'` {
		t.Errorf("winjson %+v", c)
	}
}

func TestGoldenMatrix(t *testing.T) {
	res, _ := golden(t, "matrix")
	c := changeFor(res, "test")
	if len(c) != 2 {
		t.Fatalf("test changes %+v", c)
	}
	for _, x := range c {
		if x.Form != FormMatrixJSON || x.New != `'"my-ci-linux"'` || x.OS != core.Linux {
			t.Errorf("%+v", x)
		}
	}
	if sk := skipsFor(res, "test"); len(sk) != 1 || sk[0].Reason != ReasonMacOS {
		t.Errorf("test skips %+v", sk)
	}
	dim := changeFor(res, "dim")
	if len(dim) != 1 || dim[0].Form != FormMatrix || dim[0].New != "my-ci-linux" {
		t.Errorf("dim %+v", dim)
	}
	if sk := skipsFor(res, "dyn"); len(sk) != 1 || sk[0].Reason != ReasonMatrixUnresolved {
		t.Errorf("dyn %+v", sk)
	}
	if c := changeFor(res, "dq"); len(c) != 1 || c[0].New != `"\"my-ci-linux\""` {
		t.Errorf("dq %+v", c)
	}
}

func TestParseErrorIsSkipped(t *testing.T) {
	in := []byte("jobs: [unclosed")
	out, ch, sk := Rewrite(in, testRules)
	if string(out) != string(in) || len(ch) != 0 || len(sk) != 1 || sk[0].Reason != ReasonParse {
		t.Fatalf("%v %v", ch, sk)
	}
}

func TestMultibyteColumns(t *testing.T) {
	in := "jobs:\n  a:\n    runs-on: [self-hosted, general] # é\n  b:\n    runs-on: ${{ 'é' && 'home-ci' }}\n"
	out, ch, _ := Rewrite([]byte(in), testRules)
	want := strings.NewReplacer("[self-hosted, general]", "[my-ci-linux]", "'home-ci'", "'my-ci-linux'").Replace(in)
	if string(out) != want || len(ch) != 2 {
		t.Fatalf("out=%q changes=%+v", out, ch)
	}
}

func TestRewriteVarValue(t *testing.T) {
	for in, want := range map[string]string{
		`["self-hosted","isolink"]`:         `"my-ci-linux"`,
		`["self-hosted","isolink-windows"]`: `"my-ci-windows"`,
		"home-ci":                           "my-ci-linux",
	} {
		if got, ok := RewriteVarValue(in, testRules); !ok || got != want {
			t.Errorf("%s -> %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"ubuntu-latest", `["self-hosted","macOS"]`, ""} {
		if _, ok := RewriteVarValue(in, testRules); ok {
			t.Errorf("%q should not rewrite", in)
		}
	}
}

func TestForeignLabelsAreNotRewritten(t *testing.T) {
	src := "jobs:\n  gpu:\n    runs-on: [self-hosted, general, gpu]\n  arm:\n    runs-on: [self-hosted, ARM64]\n  ok:\n    runs-on: [self-hosted, general]\n  bare:\n    runs-on: [self-hosted, Linux, X64]\n"
	res := Analyze([]byte(src), testRules)
	for _, job := range []string{"gpu", "arm"} {
		if sk := skipsFor(res, job); len(sk) != 1 || sk[0].Reason != ReasonForeignLabels || len(changeFor(res, job)) != 0 {
			t.Fatalf("%s: %+v", job, sk)
		}
	}
	if len(changeFor(res, "ok")) != 1 || len(changeFor(res, "bare")) != 1 {
		t.Fatalf("%+v", res.Changes)
	}
	// bare self-hosted only counts when a local runner carries just defaults
	rules := testRules
	rules.BareHost = false
	res = Analyze([]byte(src), rules)
	if sk := skipsFor(res, "bare"); len(sk) != 1 || sk[0].Reason != ReasonForeignLabels {
		t.Fatalf("%+v", sk)
	}
	if _, ok := RewriteVarValue(`["self-hosted","general","gpu"]`, testRules); ok {
		t.Fatal("foreign var value rewritten")
	}
}

func TestMatrixExcludeAndOtherRefs(t *testing.T) {
	src := "jobs:\n  t:\n    strategy:\n      matrix:\n        include:\n          - runner: general\n        exclude:\n          - runner: general\n    if: matrix.runner == 'general'\n    runs-on: ${{ matrix.runner }}\n"
	res := Analyze([]byte(src), testRules)
	if len(changeFor(res, "t")) != 2 {
		t.Fatalf("%+v", res.Changes)
	}
	found := false
	for _, s := range skipsFor(res, "t") {
		found = found || s.Reason == ReasonMatrixKeyUsed
	}
	if !found {
		t.Fatalf("%+v", res.Skipped)
	}
}
