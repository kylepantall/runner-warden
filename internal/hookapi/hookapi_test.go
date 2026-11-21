package hookapi

import (
	"bytes"
	"context"
	"errors"
	"io"
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
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

const rigRunner = "rn-7"

type rig struct {
	hookSock, ctlSock string
	mu                sync.Mutex
	started           []scheduler.HookRequest
	completed         int
	completedReqs     []scheduler.HookRequest
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
		HookCompleted: func(_ context.Context, q scheduler.HookRequest) error {
			r.mu.Lock()
			r.completed++
			r.completedReqs = append(r.completedReqs, q)
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
	rh := NewRunnerHooks(filepath.Join(dir, "hooks"), os.Getuid(), h, 0)
	t.Cleanup(rh.Close)
	hp, err := rh.Start(rigRunner)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := ListenControl(filepath.Join(dir, "ctl.sock"), "")
	if err != nil {
		t.Fatal(err)
	}
	r.hookSock, r.ctlSock = hp, filepath.Join(dir, "ctl.sock")
	go func() { _ = Serve(ctx, NewControlServer(h), cl) }()
	return r
}

func TestSocketModes(t *testing.T) {
	r := newRig(t)
	for p, want := range map[string]os.FileMode{r.hookSock: 0o600, r.ctlSock: 0o600} {
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
	o := HookOptions{Sleep: func(ctx context.Context, _ time.Duration) {
		n++
		if n == 2 {
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Error(err)
				return
			}
			go func() { _ = Serve(ctx, NewHookServer(h), ln) }()
		}
	}}
	var errb bytes.Buffer
	if code := RunHook(context.Background(), KindJobStarted, env(map[string]string{EnvSocket: sock}), &errb, o); code != 1 {
		t.Fatalf("daemon appearing mid-wait must be honoured, code=%d", code)
	}
}

func TestHookRequestAttributedToSocketRunner(t *testing.T) {
	r := newRig(t)
	c := NewClient(r.hookSock)
	spoof := scheduler.HookRequest{Runner: "victim", RunID: 5}
	if _, err := c.JobStarted(context.Background(), spoof); err != nil {
		t.Fatal(err)
	}
	if err := c.JobCompleted(context.Background(), spoof); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.started) != 1 || r.started[0].Runner != rigRunner || r.started[0].RunID != 5 {
		t.Fatalf("%+v", r.started)
	}
	if len(r.completedReqs) != 1 || r.completedReqs[0].Runner != rigRunner {
		t.Fatalf("%+v", r.completedReqs)
	}
}

func TestSecondConcurrentWaiterRejected(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{}, 4)
	h := Handlers{Hook: func(context.Context, scheduler.HookRequest) (scheduler.HookResponse, error) {
		entered <- struct{}{}
		<-release
		return scheduler.HookResponse{Allow: true}, nil
	}}
	rh := NewRunnerHooks(filepath.Join(shortDir(t), "h"), os.Getuid(), h, 0)
	defer rh.Close()
	sock, err := rh.Start("a")
	if err != nil {
		t.Fatal(err)
	}
	other, err := rh.Start("b")
	if err != nil {
		t.Fatal(err)
	}
	first := make(chan error, 1)
	go func() {
		_, err := NewClient(sock).JobStarted(context.Background(), scheduler.HookRequest{})
		first <- err
	}()
	<-entered
	var se *StatusError
	if _, err := NewClient(sock).JobStarted(context.Background(), scheduler.HookRequest{}); !errors.As(err, &se) || se.Code != http.StatusConflict {
		t.Fatalf("%v", err)
	}
	// Another runner's waiter is independent.
	second := make(chan error, 1)
	go func() {
		_, err := NewClient(other).JobStarted(context.Background(), scheduler.HookRequest{})
		second <- err
	}()
	<-entered
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if err := <-second; err != nil {
		t.Fatal(err)
	}
	// Slot is free again.
	if _, err := NewClient(sock).JobStarted(context.Background(), scheduler.HookRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestHookConnectionCap(t *testing.T) {
	rh := NewRunnerHooks(filepath.Join(shortDir(t), "h"), os.Getuid(), Handlers{}, 2)
	defer rh.Close()
	sock, err := rh.Start("a")
	if err != nil {
		t.Fatal(err)
	}
	var held []net.Conn
	for i := 0; i < 2; i++ {
		c, err := net.Dial("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, c)
		defer c.Close()
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("over-cap connection must be dropped, got %v", err)
	}
	// Freeing a slot admits new connections.
	_ = held[0].Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := NewClient(sock).JobCompleted(context.Background(), scheduler.HookRequest{})
		if err == nil {
			break // admitted
		}
		if time.Now().After(deadline) {
			t.Fatalf("slot never freed: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunnerHookStopRemovesSocketAndRejectsBadNames(t *testing.T) {
	rh := NewRunnerHooks(filepath.Join(shortDir(t), "h"), os.Getuid(), Handlers{}, 0)
	defer rh.Close()
	for _, bad := range []string{"", ".", "..", "a/b", "../x"} {
		if _, err := rh.Start(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	sock, err := rh.Start("a")
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Dir(sock))
	if err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("%v %v", fi, err)
	}
	rh.Stop("a")
	if _, err := os.Stat(filepath.Dir(sock)); !os.IsNotExist(err) {
		t.Fatal("runner hook dir not removed")
	}
}
