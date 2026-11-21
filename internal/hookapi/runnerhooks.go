package hookapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// DefaultMaxHookConns caps simultaneous connections across all runner hook
// sockets, so a misbehaving runner cannot exhaust the daemon.
const DefaultMaxHookConns = 256

// RunnerHooks serves one hook socket per runner: <base>/<runner>/hook.sock.
// The runner dir is owned by the runner uid with mode 0700 and the socket is
// 0600, so only that runner (and root) can connect. The handler overrides the
// runner name in every request with the socket's own, so a runner cannot speak
// for another, and allows one job-started waiter per runner.
type RunnerHooks struct {
	base string
	uid  int
	h    Handlers
	sem  chan struct{}

	mu sync.Mutex
	m  map[string]*runnerHook
}

type runnerHook struct {
	srv *http.Server
	dir string
}

// NewRunnerHooks serves hook sockets under base, owned by uid. maxConns <= 0
// means DefaultMaxHookConns.
func NewRunnerHooks(base string, uid int, h Handlers, maxConns int) *RunnerHooks {
	if maxConns <= 0 {
		maxConns = DefaultMaxHookConns
	}
	return &RunnerHooks{base: base, uid: uid, h: h, sem: make(chan struct{}, maxConns), m: map[string]*runnerHook{}}
}

// Dir is the directory (bind-mounted into the runner) holding the socket.
func (r *RunnerHooks) Dir(runner string) string { return filepath.Join(r.base, runner) }

// SocketPath is the runner's hook socket.
func (r *RunnerHooks) SocketPath(runner string) string {
	return filepath.Join(r.Dir(runner), HookSocketName)
}

func safeName(n string) bool {
	return n != "" && n != "." && n != ".." && n == filepath.Base(n)
}

// Start creates and serves the runner's socket, replacing any previous one.
func (r *RunnerHooks) Start(runner string) (string, error) {
	if !safeName(runner) {
		return "", errors.New("hookapi: invalid runner name")
	}
	r.Stop(runner)
	if err := os.MkdirAll(r.base, 0o755); err != nil { //nolint:gosec // traversable base dir; per-runner dirs are 0700
		return "", err
	}
	if err := os.Chmod(r.base, 0o711); err != nil { //nolint:gosec // x-only for others so runners can reach their own 0700 dir
		return "", err
	}
	dir := r.Dir(runner)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // owner-only dir needs the x bit
		return "", err
	}
	sock := r.SocketPath(runner)
	fail := func(err error) (string, error) { _ = os.RemoveAll(dir); return "", err }
	if err := r.chown(dir); err != nil {
		return fail(err)
	}
	ln, err := listenUnix(sock, 0o600, "")
	if err != nil {
		return fail(err)
	}
	if err := r.chown(sock); err != nil {
		_ = ln.Close()
		return fail(err)
	}
	srv := newServer(r.hookMux(runner))
	go func() { _ = srv.Serve(&limitListener{Listener: ln, sem: r.sem}) }()
	r.mu.Lock()
	r.m[runner] = &runnerHook{srv: srv, dir: dir}
	r.mu.Unlock()
	return sock, nil
}

// chown hands p to the runner uid. Only root can; non-root daemons (tests,
// rootless) keep ownership, which still confines the dir to that user.
func (r *RunnerHooks) chown(p string) error {
	if os.Geteuid() != 0 {
		return nil
	}
	return os.Chown(p, r.uid, -1)
}

// Stop closes the runner's socket (cutting waiting hooks) and removes its dir.
func (r *RunnerHooks) Stop(runner string) {
	r.mu.Lock()
	rh := r.m[runner]
	delete(r.m, runner)
	r.mu.Unlock()
	if rh == nil {
		return
	}
	_ = rh.srv.Close()
	_ = os.RemoveAll(rh.dir)
}

// Close stops every socket but leaves the dirs: running runners are adopted
// (and their sockets recreated) by the next daemon.
func (r *RunnerHooks) Close() {
	r.mu.Lock()
	all := r.m
	r.m = map[string]*runnerHook{}
	r.mu.Unlock()
	for _, rh := range all {
		_ = rh.srv.Close()
	}
}

func (r *RunnerHooks) hookMux(runner string) *http.ServeMux {
	var busy atomic.Bool
	h := Handlers{}
	if r.h.Hook != nil {
		h.Hook = func(ctx context.Context, req scheduler.HookRequest) (scheduler.HookResponse, error) {
			req.Runner = runner
			if !busy.CompareAndSwap(false, true) {
				return scheduler.HookResponse{}, ErrConflict
			}
			defer busy.Store(false)
			return r.h.Hook(ctx, req)
		}
	}
	if r.h.HookCompleted != nil {
		h.HookCompleted = func(ctx context.Context, req scheduler.HookRequest) error {
			req.Runner = runner
			return r.h.HookCompleted(ctx, req)
		}
	}
	mux := http.NewServeMux()
	h.registerHooks(mux)
	return mux
}

// limitListener drops connections beyond the shared cap instead of queueing
// them, so excess clients cost no goroutine.
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		select {
		case l.sem <- struct{}{}:
			return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
		default:
			_ = c.Close()
		}
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
