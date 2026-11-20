package admission

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func topo(n int) []Core { // n physical cores, 2 threads each: core i -> cpus i, i+n
	var cs []Core
	for i := 0; i < n; i++ {
		cs = append(cs, Core{ID: i, Threads: []int{i, i + n}})
	}
	return cs
}

func TestLedgerMemory(t *testing.T) {
	l := New(Config{Budget: 100, External: 10})
	tests := []struct {
		name    string
		key     string
		mem     int64
		wantErr error
		free    int64
	}{
		{"fits", "a", 50, nil, 40},
		{"exact", "b", 40, nil, 0},
		{"over", "c", 1, ErrNoMemory, 0},
		{"dup", "a", 1, ErrExists, 0},
	}
	for _, tc := range tests {
		_, err := l.Reserve(tc.key, Request{Mem: tc.mem})
		if !errors.Is(err, tc.wantErr) {
			t.Fatalf("%s: err=%v want %v", tc.name, err, tc.wantErr)
		}
		if l.Free() != tc.free {
			t.Fatalf("%s: free=%d want %d", tc.name, l.Free(), tc.free)
		}
	}
	if !l.Release("a") || l.Release("a") {
		t.Fatal("release should succeed once")
	}
	if l.Free() != 50 {
		t.Fatalf("free=%d", l.Free())
	}
	l.SetExternal(60)
	if l.Free() != 0 {
		t.Fatalf("free=%d", l.Free())
	}
}

func TestLedgerForceAndResize(t *testing.T) {
	l := New(Config{Budget: 100})
	l.Force("vm", Request{Mem: 120})
	if l.Free() != -20 {
		t.Fatalf("free=%d", l.Free())
	}
	l.Release("vm")
	if _, err := l.Reserve("r", Request{Mem: 10}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Resize("r", Request{Mem: 200}); !errors.Is(err, ErrNoMemory) {
		t.Fatalf("err=%v", err)
	}
	if l.Reserved() != 10 {
		t.Fatal("failed resize must not change state")
	}
	if _, err := l.Resize("r", Request{Mem: 90}); err != nil {
		t.Fatal(err)
	}
	if l.Free() != 10 {
		t.Fatalf("free=%d", l.Free())
	}
	if _, err := l.ForceResize("r", Request{Mem: 150}); err != nil || l.Free() != -50 {
		t.Fatalf("err=%v free=%d", err, l.Free())
	}
	if _, err := l.Resize("nope", Request{}); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}

func TestPinnedCoresNeverShared(t *testing.T) {
	l := New(Config{Budget: 1000, Cores: topo(4), ReservedCPUs: []int{0, 4}}) // core 0 reserved
	if l.Usage().TotalCores != 3 {
		t.Fatalf("pinnable=%d", l.Usage().TotalCores)
	}
	a, err := l.Reserve("a", Request{Mem: 1, PinnedCores: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.CPUs, []int{1, 2, 5, 6}) || a.CPUSet() != "1,2,5,6" {
		t.Fatalf("cpus=%v", a.CPUs) // SMT siblings together, reserved core skipped
	}
	if _, err := l.Reserve("b", Request{Mem: 1, PinnedCores: 2}); !errors.Is(err, ErrNoCores) {
		t.Fatalf("err=%v", err)
	}
	b, err := l.Reserve("b", Request{Mem: 1, PinnedCores: 1})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[int]bool{}
	for _, c := range append(append([]int{}, a.CPUs...), b.CPUs...) {
		if seen[c] {
			t.Fatalf("cpu %d shared", c)
		}
		seen[c] = true
	}
	l.Release("a")
	if l.FreeCores() != 2 {
		t.Fatalf("free cores=%d", l.FreeCores())
	}
	if l.CanFit(Request{PinnedCores: 3}) || !l.CanFit(Request{PinnedCores: 2}) {
		t.Fatal("CanFit wrong")
	}
	if !l.FitsEver(Request{PinnedCores: 3}) || l.FitsEver(Request{PinnedCores: 4}) {
		t.Fatal("FitsEver wrong")
	}
}

func TestResizeCores(t *testing.T) {
	l := New(Config{Budget: 100, Cores: topo(4)})
	if _, err := l.Reserve("a", Request{Mem: 1}); err != nil {
		t.Fatal(err)
	}
	r, err := l.Resize("a", Request{Mem: 1, PinnedCores: 2})
	if err != nil || len(r.Cores) != 2 {
		t.Fatalf("%v %v", r, err)
	}
	r, _ = l.Resize("a", Request{Mem: 1})
	if len(r.Cores) != 0 || l.FreeCores() != 4 {
		t.Fatalf("cores not freed: %v", r)
	}
	l.Force("f", Request{PinnedCores: 9})
	if l.FreeCores() != 0 {
		t.Fatal("force should pin what is available")
	}
}

func TestParseCPUList(t *testing.T) {
	tests := []struct {
		in   string
		want []int
		bad  bool
	}{
		{"0-1,4,6-7\n", []int{0, 1, 4, 6, 7}, false},
		{"3", []int{3}, false},
		{"", nil, false},
		{"2-1", nil, true},
		{"x", nil, true},
	}
	for _, tc := range tests {
		got, err := ParseCPUList(tc.in)
		if (err != nil) != tc.bad || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: got %v err=%v", tc.in, got, err)
		}
	}
}

func TestReadTopology(t *testing.T) {
	root := t.TempDir()
	write := func(cpu, siblings string) {
		d := filepath.Join(root, "cpu"+cpu, "topology")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "thread_siblings_list"), []byte(siblings+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("0", "0,4")
	write("4", "0,4")
	write("1", "1,5")
	write("5", "1,5")
	write("2", "2")
	if err := os.MkdirAll(filepath.Join(root, "cpufreq"), 0o755); err != nil { // not a cpuN dir
		t.Fatal(err)
	}
	got, err := ReadTopology(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []Core{{0, []int{0, 4}}, {1, []int{1, 5}}, {2, []int{2}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
	if _, err := ReadTopology(t.TempDir()); err == nil {
		t.Fatal("empty root must error")
	}
}

func TestMatchRule(t *testing.T) {
	rules := []Rule{
		{Match: "acme/app/ci.yml/e2e*", Mem: 8},
		{Match: "acme/*/ci.yml/*", Mem: 2},
		{Match: "*/*/*/*", Mem: 1},
	}
	tests := []struct {
		owner, repo, ref, job string
		want                  int64
	}{
		{"Acme", "App", "acme/app/.github/workflows/ci.yml@refs/heads/main", "e2e shard 1", 8},
		{"acme", "app", "acme/app/.github/workflows/ci.yml@refs/heads/main", "build / test", 2},
		{"acme", "app", "acme/app/.github/workflows/release.yml@refs/tags/v1", "x", 1},
		{"other", "r", "o/r/.github/workflows/ci.yml@main", "j", 1},
	}
	for _, tc := range tests {
		r, ok := MatchRule(rules, tc.owner, tc.repo, tc.ref, tc.job)
		if !ok || r.Mem != tc.want {
			t.Fatalf("%+v: got %+v ok=%v", tc, r, ok)
		}
	}
	if _, ok := MatchRule(rules[:1], "x", "y", "z@r", "j"); ok {
		t.Fatal("should not match")
	}
	if _, ok := MatchRule([]Rule{{Match: "bad"}}, "a", "b", "c@d", "e"); ok {
		t.Fatal("malformed pattern must not match")
	}
	if WorkflowFile("a/b/.github/workflows/x.yml@refs/heads/a@b") != "x.yml" {
		t.Fatal("workflow file")
	}
}

func TestPolicyAndOrdering(t *testing.T) {
	now := time.Unix(10000, 0)
	old := Waiter{ID: "old", Group: "a", Cost: 8, Since: now.Add(-10 * time.Minute)}
	heavy := Waiter{ID: "heavy", Group: "a", Cost: 4, Since: now}
	light := Waiter{ID: "light", Group: "a", Cost: 1, Since: now}
	p := Policy{ReserveAfter: 5 * time.Minute, LightThreshold: 1}
	tests := []struct {
		name string
		now  time.Time
		cand Waiter
		want bool
	}{
		{"before reserve", now.Add(-7 * time.Minute), heavy, false},
		{"heavy blocked", now, heavy, true},
		{"light passes", now, light, false},
		{"self passes", now, old, false},
	}
	for _, tc := range tests {
		if got := p.Blocks(old, tc.cand, tc.now); got != tc.want {
			t.Fatalf("%s: %v", tc.name, got)
		}
	}
	if Oldest(nil) != -1 || Oldest([]Waiter{heavy, old, light}) != 1 {
		t.Fatal("oldest")
	}

	a := Waiter{ID: "a", Group: "A", Since: now.Add(-time.Hour)}
	b := Waiter{ID: "b", Group: "B", Since: now}
	ws := []Waiter{a, b}
	SortFair(ws, map[string]int64{"A": 2, "B": 2}, func(g string) int {
		if g == "B" {
			return 2
		}
		return 1
	})
	if ws[0].ID != "b" {
		t.Fatal("weight 2 group should go first at equal granted")
	}
	ws = []Waiter{a, b}
	SortFair(ws, map[string]int64{"A": 5, "B": 5}, nil)
	if ws[0].ID != "a" {
		t.Fatal("tie goes to the oldest")
	}
	ws = []Waiter{a, b}
	SortFair(ws, map[string]int64{"A": 9, "B": 0}, nil)
	if ws[0].ID != "b" {
		t.Fatal("lowest granted first")
	}
}
