package hookapi

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kylepantall/runner-warden/internal/scheduler"
)

func shortDir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

type rig struct {
	hookSock, ctlSock string
	mu                sync.Mutex
	started           []scheduler.HookRequest
	completed         int
	drain             []bool
	killed            []string
	verdict           scheduler.HookResponse
}

func newRig(t *testing.T) *rig {
	r := &rig{verdict: scheduler.HookResponse{Allow: true, Reason: scheduler.ReasonGranted}}
	h := Handlers{
		Hook: func(_ context.Context, q scheduler.HookRequest) (scheduler.HookResponse, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.started = append(r.started, q)
			return r.verdict, nil
		},
		HookCompleted: func(context.Context, scheduler.HookRequest) error {
			r.mu.Lock()
			r.completed++
			r.mu.Unlock()
			return nil
		},
		Status: func(context.Context) (any, error) { return scheduler.Status{Draining: true, Held: 3}, nil },
		Drain:  func(on bool) error { r.drain = append(r.drain, on); return nil },
		Kill: func(_ context.Context, n string) error {
			if n == "ghost" {
				return ErrNotFound
			}
			r.killed = append(r.killed, n)
			return nil
		},
	}
	dir := shortDir(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	hl, hp, err := ListenHook(filepath.Join(dir, "hooks"))
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ListenControl(filepath.Join(dir, "ctl.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	r.hookSock, r.ctlSock = hp, filepath.Join(dir, "ctl.sock")
	go Serve(ctx, NewHookServer(h), hl)
	go Serve(ctx, NewControlServer(h), cl)
	return r
}

func TestSocketModes(t *testing.T) {
	r := newRig(t)
	for p, want := range map[string]os.FileMode{r.hookSock: 0o666, r.ctlSock: 0o600} {
		fi, err := os.Stat(p)
		if err != nil || fi.Mode().Perm() != want {
			t.Fatalf("%s: %v %v", p, fi, err)
		}
	}
}

func TestHookServerExposesOnlyHookEndpoints(t *testing.T) {
	r := newRig(t)
	hc := NewClient(r.hookSock)
	ctx := context.Background()
	if err := hc.Status(ctx, nil); err == nil {
		t.Fatal("status must not be on the hook socket")
	} else if se := (*StatusError)(nil); !errors.As(err, &se) || se.Code == http.StatusOK {
		t.Fatal(err)
	}
	if err := hc.Drain(ctx, true); err == nil {
		t.Fatal("drain must not be on the hook socket")
	}
	if err := hc.Kill(ctx, "x"); err == nil {
		t.Fatal("kill must not be on the hook socket")
	}
	if resp, err := hc.JobStarted(ctx, scheduler.HookRequest{Runner: "r"}); err != nil || !resp.Allow {
		t.Fatalf("%+v %v", resp, err)
	}
	if err := hc.JobCompleted(ctx, scheduler.HookRequest{Runner: "r"}); err != nil {
		t.Fatal(err)
	}
}

func TestControlEndpoints(t *testing.T) {
	r := newRig(t)
	c := NewClient(r.ctlSock)
	ctx := context.Background()
	var st scheduler.Status
	if err := c.Status(ctx, &st); err != nil || !st.Draining || st.Held != 3 {
		t.Fatalf("%+v %v", st, err)
	}
	if err := c.Drain(ctx, true); err != nil || len(r.drain) != 1 || !r.drain[0] {
		t.Fatal("drain")
	}
	if err := c.Kill(ctx, "rn-1"); err != nil || len(r.killed) != 1 || r.killed[0] != "rn-1" {
		t.Fatalf("%v %v", err, r.killed)
	}
	var se *StatusError
	if err := c.Kill(ctx, "ghost"); !errors.As(err, &se) || se.Code != http.StatusNotFound {
		t.Fatalf("%v", err)
	}
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunHookExitCodes(t *testing.T) {
	r := newRig(t)
	e := env(map[string]string{EnvSocket: r.hookSock, EnvRunner: "rn-7", "GITHUB_RUN_ID": "42", "GITHUB_JOB": "build",
		"GITHUB_WORKFLOW": "CI", "GITHUB_REPOSITORY": "o/r"})
	var errb bytes.Buffer
	if code := RunHook(context.Background(), KindJobStarted, e, &errb, HookOptions{}); code != 0 {
		t.Fatalf("code=%d", code)
	}
	got := r.started[0]
	if got.Runner != "rn-7" || got.RunID != 42 || got.JobKey != "build" || got.Workflow != "CI" || got.Repository != "o/r" {
		t.Fatalf("%+v", got)
	}
	r.verdict = scheduler.HookResponse{Allow: false, Reason: scheduler.ReasonAdmissionTimeout}
	errb.Reset()
	if code := RunHook(context.Background(), KindJobStarted, e, &errb, HookOptions{}); code != 1 || errb.Len() == 0 {
		t.Fatalf("code=%d stderr=%q", code, errb.String())
	}
	if code := RunHook(context.Background(), KindJobCompleted, e, &errb, HookOptions{}); code != 0 || r.completed != 1 {
		t.Fatalf("code=%d", code)
	}
}

func TestRunHookFailsOpenAfterUnreachable(t *testing.T) {
	dead := filepath.Join(shortDir(t), "none.sock")
	now := time.Unix(0, 0)
	sleeps := 0
	o := HookOptions{
		Now:   func() time.Time { return now },
		Sleep: func(_ context.Context, d time.Duration) { sleeps++; now = now.Add(d) },
	}
	var errb bytes.Buffer
	code := RunHook(context.Background(), KindJobStarted, env(map[string]string{EnvSocket: dead}), &errb, o)
	if code != 0 || sleeps < 25 || sleeps > 40 {
		t.Fatalf("code=%d sleeps=%d", code, sleeps)
	}
	if errb.Len() != 0 {
		t.Fatal("fail open is silent on stderr (logged instead)")
	}
}

func TestRunHookNoSocketConfigured(t *testing.T) {
	if code := RunHook(context.Background(), KindJobStarted, env(nil), &bytes.Buffer{}, HookOptions{}); code != 0 {
		t.Fatal(code)
	}
}

func TestRunHookRetriesUntilDaemonAppears(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "late.sock")
	h := Handlers{Hook: func(context.Context, scheduler.HookRequest) (scheduler.HookResponse, error) {
		return scheduler.HookResponse{Allow: false, Reason: "no"}, nil
	}}
	n := 0
	o := HookOptions{Sleep: func(ctx context.Context, d time.Duration) {
		n++
		if n == 2 {
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Error(err)
				return
			}
			go Serve(ctx, NewHookServer(h), ln)
		}
	}}
	var errb bytes.Buffer
	if code := RunHook(context.Background(), KindJobStarted, env(map[string]string{EnvSocket: sock}), &errb, o); code != 1 {
		t.Fatalf("daemon appearing mid-wait must be honoured, code=%d", code)
	}
}
