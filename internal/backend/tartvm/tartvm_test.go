package tartvm

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

type call struct{ cmd, stdin string }

type fakeMac struct {
	calls   []call
	guestAt time.Time
	version string
}

func (f *fakeMac) Command(_ context.Context, cmd, stdin string) (string, string, int, error) {
	f.calls = append(f.calls, call{cmd, stdin})
	switch {
	case strings.Contains(stdin, "date -u +%s"):
		return strings.Join([]string{itoa(f.guestAt.Unix()), "arm64", f.version}, "\n"), "", 0, nil
	case strings.Contains(stdin, "actions-runner-osx-arm64"):
		f.version = "2.400.0"
		return "2.400.0\n", "", 0, nil
	case strings.Contains(stdin, `echo "alive=`):
		return "alive=1 cpu=42 mem=1024 log=3\n", "", 0, nil
	}
	return "", "", 0, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

const runner = "rw-ci-h1-0000abcd"

func newTestHost(t *testing.T, f *fakeMac, now time.Time) *Host {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"v2.400.0"}`))
	}))
	t.Cleanup(srv.Close)
	return New(Config{Name: "mac", VM: "vm1", Tart: "/opt/homebrew/bin/tart", GuestUID: 501, Instance: "main",
		Prefix: "rw-ci", HostID: "h1", Cmd: f, ReleaseAPI: srv.URL, Now: func() time.Time { return now }})
}

func TestProbeInstallsTemplateAndSyncsClock(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeMac{guestAt: now.Add(-5 * time.Minute), version: "none"}
	h := newTestHost(t, f, now)
	if err := h.Probe(context.Background()); err != nil || !h.Up() {
		t.Fatalf("%v %v", err, h.Up())
	}
	var synced, installed bool
	for _, c := range f.calls {
		if !strings.HasPrefix(c.cmd, "/opt/homebrew/bin/tart exec -i vm1 ") {
			t.Fatalf("not a guest command: %q", c.cmd)
		}
		synced = synced || strings.Contains(c.stdin, "sntp")
		installed = installed || strings.Contains(c.stdin, "v2.400.0/actions-runner-osx-arm64-2.400.0.tar.gz")
	}
	if !synced || !installed {
		t.Fatalf("synced=%v installed=%v", synced, installed)
	}
	n := len(f.calls)
	f.guestAt = now
	if err := h.Probe(context.Background()); err != nil || len(f.calls) != n+1 {
		t.Fatalf("a current guest needs one call: %v %d", err, len(f.calls)-n)
	}
}

func TestStartRunsLaunchdJobWithJITOnStdin(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	f := &fakeMac{guestAt: now, version: "2.400.0"}
	h := newTestHost(t, f, now)
	ctx := context.Background()
	if _, err := h.Start(ctx, core.RunnerSpec{Name: runner, JITConfig: "SkVU"}); err == nil {
		t.Fatal("a down host must refuse")
	}
	_ = h.Probe(ctx)
	for _, bad := range []core.RunnerSpec{{Name: "other-name", JITConfig: "x"}, {Name: runner, JITConfig: "a'b"}} {
		if _, err := h.Start(ctx, bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
	hd, err := h.Start(ctx, core.RunnerSpec{Name: runner, JITConfig: "SkVU"})
	if err != nil || hd.BackendID != runner {
		t.Fatalf("%+v %v", hd, err)
	}
	last := f.calls[len(f.calls)-1]
	for _, want := range []string{"launchctl bootstrap gui/501", "<string>rw.main." + runner + "</string>", "'SkVU'", `\$(cat .jit)`, "cp -Rc"} {
		if !strings.Contains(last.stdin, want) {
			t.Errorf("start script lacks %q", want)
		}
	}
	plist := last.stdin[strings.Index(last.stdin, "<plist"):strings.Index(last.stdin, "</plist>")]
	if strings.Contains(last.cmd, "SkVU") || strings.Contains(plist, "&") {
		t.Error("JIT must not be on the command line; plist must stay valid XML")
	}
	u, err := h.Stats(ctx, hd)
	if err != nil || !u.Alive || u.CPUSeconds != 42 || u.MemCurrent != 1024 || u.LogBytes != 3*1024 {
		t.Fatalf("%+v %v", u, err)
	}
}
