package dockerproxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Spec is everything `runner-warden proxy --spec <file>` needs to serve one
// runner's proxy in its own process (outside the daemon, so it survives a
// daemon restart).
type Spec struct {
	Config Config `json:"config"`
	// RunnerContainer is the runner container's name or ID. The proxy exits
	// once that container no longer exists.
	RunnerContainer string `json:"runner_container"`
	LogLevel        string `json:"log_level,omitempty"`
}

// WriteSpec writes the spec as a 0600 file, atomically.
func WriteSpec(path string, s Spec) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ReadSpec reads a spec written by WriteSpec.
func ReadSpec(path string) (Spec, error) {
	var s Spec
	b, err := os.ReadFile(path) //nolint:gosec // the operator-supplied spec path is the point
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return s, fmt.Errorf("proxy spec: %w", err)
	}
	if s.Config.Socket == "" || s.Config.Upstream == "" || s.RunnerContainer == "" {
		return s, errors.New("proxy spec: socket, upstream and runner container are required")
	}
	return s, nil
}

// ServeOptions are injectable for tests.
type ServeOptions struct {
	Log *slog.Logger
	// PollEvery is how often the runner container is checked; 0 = 10s.
	PollEvery time.Duration
	// StartGrace is how long the container may stay missing before it was
	// ever seen (the proxy starts before the runner is created); 0 = 5m.
	StartGrace time.Duration
}

// Serve runs the proxy until ctx is done or the runner container is gone, and
// removes the socket on the way out.
func Serve(ctx context.Context, s Spec, o ServeOptions) error {
	if o.PollEvery <= 0 {
		o.PollEvery = 10 * time.Second
	}
	if o.StartGrace <= 0 {
		o.StartGrace = 5 * time.Minute
	}
	cfg := s.Config
	cfg.Log = o.Log
	p, err := New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = p.Close() }()
	log := p.log
	log.Info("proxy serving", "runner", cfg.Runner, "socket", cfg.Socket)

	began := time.Now()
	seen := false
	t := time.NewTicker(o.PollEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		exists, err := p.containerExists(ctx, s.RunnerContainer)
		switch {
		case err != nil:
			// Docker unreachable says nothing about the runner: keep serving.
			log.Warn("proxy runner check failed", "runner", cfg.Runner, "error", err.Error())
		case exists:
			seen = true
		case seen || time.Since(began) > o.StartGrace:
			log.Info("proxy exiting: runner container gone", "runner", cfg.Runner)
			return nil
		}
	}
}

// containerExists asks the real daemon whether the container exists.
func (p *Proxy) containerExists(ctx context.Context, name string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/containers/"+url.PathEscape(name)+"/json", nil)
	if err != nil {
		return false, err
	}
	resp, err := p.up.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	default:
		return false, fmt.Errorf("inspect %s: status %d", name, resp.StatusCode)
	}
}
