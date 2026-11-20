package hookapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// Client talks to a unix-socket server.
type Client struct {
	Socket string
	http   *http.Client
}

// StatusError is a non-200 answer.
type StatusError struct{ Code int }

func (e *StatusError) Error() string { return "hookapi: http status " + strconv.Itoa(e.Code) }

// NewClient builds a client for the socket path. There is no overall timeout:
// the job-started call blocks until admission; use the context.
func NewClient(socket string) *Client {
	return &Client{Socket: socket, http: &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", socket)
		},
		DisableKeepAlives: true,
	}}}
}

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://unix"+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return &StatusError{Code: resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// JobStarted asks for admission; it blocks until the verdict.
func (c *Client) JobStarted(ctx context.Context, req scheduler.HookRequest) (scheduler.HookResponse, error) {
	var r scheduler.HookResponse
	err := c.do(ctx, http.MethodPost, PathJobStarted, req, &r)
	return r, err
}

// JobCompleted notifies the daemon the job finished.
func (c *Client) JobCompleted(ctx context.Context, req scheduler.HookRequest) error {
	return c.do(ctx, http.MethodPost, PathJobCompleted, req, nil)
}

// Status fetches the status document into out.
func (c *Client) Status(ctx context.Context, out any) error {
	return c.do(ctx, http.MethodGet, PathStatus, nil, out)
}

// Drain turns drain mode on or off.
func (c *Client) Drain(ctx context.Context, on bool) error {
	return c.do(ctx, http.MethodPost, PathDrain, DrainRequest{Drain: on}, nil)
}

// Kill stops a runner; a *StatusError with Code 404 means it is unknown.
func (c *Client) Kill(ctx context.Context, runner string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf(PathKillFmt, url.PathEscape(runner)), struct{}{}, nil)
}

// ---- runner hook entry point ---------------------------------------------

// Environment variables read by the hook command.
const (
	EnvSocket = "RUNNER_WARDEN_SOCKET"
	EnvRunner = "RUNNER_WARDEN_RUNNER"
)

// Hook kinds.
const (
	KindJobStarted   = "job-started"
	KindJobCompleted = "job-completed"
)

// FailOpenAfter is how long the daemon may stay unreachable before the hook
// lets the job proceed.
const FailOpenAfter = 30 * time.Second

// HookOptions are injectable for tests.
type HookOptions struct {
	Log           *slog.Logger
	Now           func() time.Time
	Sleep         func(context.Context, time.Duration)
	FailOpenAfter time.Duration // default FailOpenAfter
	Retry         time.Duration // default 1s
}

// RequestFromEnv builds the hook request from the runner's environment.
func RequestFromEnv(getenv func(string) string) scheduler.HookRequest {
	id, _ := strconv.ParseInt(getenv("GITHUB_RUN_ID"), 10, 64)
	return scheduler.HookRequest{
		Runner:     getenv(EnvRunner),
		RunID:      id,
		JobKey:     getenv("GITHUB_JOB"),
		Workflow:   getenv("GITHUB_WORKFLOW"),
		Repository: getenv("GITHUB_REPOSITORY"),
	}
}

// RunHook implements `runner-warden hook <kind>` and returns the exit code:
// 0 to let the job run (granted, or fail open), 1 on an explicit rejection
// (with a line on stderr). job-completed is best effort and always exits 0.
func RunHook(ctx context.Context, kind string, getenv func(string) string, stderr io.Writer, o HookOptions) int {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Sleep == nil {
		o.Sleep = func(ctx context.Context, d time.Duration) {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-t.C:
			case <-ctx.Done():
			}
		}
	}
	if o.FailOpenAfter <= 0 {
		o.FailOpenAfter = FailOpenAfter
	}
	if o.Retry <= 0 {
		o.Retry = time.Second
	}
	sock := getenv(EnvSocket)
	if sock == "" {
		o.Log.Warn("hook socket not configured; failing open", "kind", kind)
		return 0
	}
	c := NewClient(sock)
	req := RequestFromEnv(getenv)
	var firstFail time.Time
	for {
		var (
			resp scheduler.HookResponse
			err  error
		)
		if kind == KindJobCompleted {
			err = c.JobCompleted(ctx, req)
		} else {
			resp, err = c.JobStarted(ctx, req)
		}
		if err == nil {
			if kind == KindJobStarted && !resp.Allow {
				fmt.Fprintf(stderr, "runner-warden: job rejected (%s)\n", resp.Reason)
				o.Log.Error("job rejected", "reason", resp.Reason, "waited", resp.Waited)
				return 1
			}
			return 0
		}
		if ctx.Err() != nil {
			o.Log.Warn("hook interrupted; failing open", "kind", kind)
			return 0
		}
		var se *StatusError
		if errors.As(err, &se) && se.Code >= 400 && se.Code < 500 {
			o.Log.Error("daemon refused hook request; failing open", "kind", kind, "status", se.Code)
			return 0
		}
		now := o.Now()
		if firstFail.IsZero() {
			firstFail = now
		}
		if now.Sub(firstFail) > o.FailOpenAfter || kind == KindJobCompleted && now.Sub(firstFail) > 5*time.Second {
			o.Log.Warn("daemon unreachable; failing open", "kind", kind, "err", err.Error())
			return 0
		}
		o.Sleep(ctx, o.Retry)
	}
}
