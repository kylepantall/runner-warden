// Package hookapi is the daemon's local HTTP-over-unix-socket protocol. Two
// servers share one set of handlers: the control server (CLI, all endpoints)
// and the hook servers (only the runner hook endpoints; one per runner, see
// RunnerHooks, each directory bind-mounted into that runner alone).
package hookapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"time"

	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// Endpoint paths.
const (
	PathJobStarted   = "/v1/hook/job-started"
	PathJobCompleted = "/v1/hook/job-completed"
	PathStatus       = "/v1/status"
	PathDrain        = "/v1/drain"
	PathKillFmt      = "/v1/runners/%s/kill"
)

// HookSocketName is the socket file inside a runner's hook directory.
const HookSocketName = "hook.sock"

// ErrNotFound lets a Kill handler report an unknown runner (HTTP 404).
var ErrNotFound = errors.New("hookapi: not found")

// ErrConflict lets a handler refuse a request that clashes with one already in
// flight (HTTP 409).
var ErrConflict = errors.New("hookapi: conflict")

// Handlers are implemented by the daemon; nil funcs answer 501.
type Handlers struct {
	Hook          func(ctx context.Context, req scheduler.HookRequest) (scheduler.HookResponse, error)
	HookCompleted func(ctx context.Context, req scheduler.HookRequest) error
	Status        func(ctx context.Context) (any, error)
	Drain         func(on bool) error
	Kill          func(ctx context.Context, runner string) error
}

// DrainRequest is the body of POST /v1/drain.
type DrainRequest struct {
	Drain bool `json:"drain"`
}

// NewHookServer serves only the runner hook endpoints.
func NewHookServer(h Handlers) *http.Server {
	mux := http.NewServeMux()
	h.registerHooks(mux)
	return newServer(mux)
}

// NewControlServer serves every endpoint.
func NewControlServer(h Handlers) *http.Server {
	mux := http.NewServeMux()
	h.registerHooks(mux)
	mux.HandleFunc("GET "+PathStatus, func(w http.ResponseWriter, r *http.Request) {
		if h.Status == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		st, err := h.Status(r.Context())
		reply(w, st, err)
	})
	mux.HandleFunc("POST "+PathDrain, func(w http.ResponseWriter, r *http.Request) {
		var req DrainRequest
		if !decode(w, r, &req) {
			return
		}
		if h.Drain == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		reply(w, req, h.Drain(req.Drain))
	})
	mux.HandleFunc("POST /v1/runners/{name}/kill", func(w http.ResponseWriter, r *http.Request) {
		if h.Kill == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		err := h.Kill(r.Context(), r.PathValue("name"))
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "unknown runner", http.StatusNotFound)
			return
		}
		reply(w, map[string]bool{"ok": err == nil}, err)
	})
	return newServer(mux)
}

func (h Handlers) registerHooks(mux *http.ServeMux) {
	mux.HandleFunc("POST "+PathJobStarted, func(w http.ResponseWriter, r *http.Request) {
		var req scheduler.HookRequest
		if !decode(w, r, &req) {
			return
		}
		if h.Hook == nil {
			http.Error(w, "not implemented", http.StatusNotImplemented)
			return
		}
		resp, err := h.Hook(r.Context(), req)
		reply(w, resp, err)
	})
	mux.HandleFunc("POST "+PathJobCompleted, func(w http.ResponseWriter, r *http.Request) {
		var req scheduler.HookRequest
		if !decode(w, r, &req) {
			return
		}
		var err error
		if h.HookCompleted != nil {
			err = h.HookCompleted(r.Context(), req)
		}
		reply(w, map[string]bool{"ok": err == nil}, err)
	})
}

func newServer(mux *http.ServeMux) *http.Server {
	// No Read/WriteTimeout: the job-started hook blocks for up to the
	// admission timeout.
	return &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if errors.Is(err, ErrConflict) {
		http.Error(w, "conflict", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// Serve serves srv on ln until ctx ends, then shuts down (hooks still waiting
// are cut after a short grace). It returns nil on a clean shutdown.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil {
			_ = srv.Close()
		}
		<-errc
		return nil
	}
}

// ListenControl opens the control socket: mode 0600 (root only) when group is
// empty, otherwise 0660 and owned by that group.
func ListenControl(path, group string) (net.Listener, error) {
	mode := os.FileMode(0o600)
	if group != "" {
		mode = 0o660
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	return listenUnix(path, mode, group)
}

func listenUnix(path string, mode os.FileMode, group string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (net.Listener, error) { ln.Close(); os.Remove(path); return nil, err }
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return fail(fmt.Errorf("control group: %w", err))
		}
		gid, err := strconv.Atoi(g.Gid)
		if err != nil {
			return fail(err)
		}
		if err := os.Chown(path, -1, gid); err != nil {
			return fail(err)
		}
	}
	if err := os.Chmod(path, mode); err != nil {
		return fail(err)
	}
	return ln, nil
}
