package linuxhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Commander runs a command line on the host with stdin (an SSH session).
type Commander interface {
	Command(ctx context.Context, cmd, stdin string) (stdout, stderr string, exit int, err error)
}

// HostConfig describes one remote host.
type HostConfig struct {
	Name    string
	Exec    string // runs the rest of the command as root on the Linux side
	Binary  string // runner-warden on the host
	Cmd     Commander
	Agent   AgentConfig
	Version string // this daemon's version; the host's agent must match
	Self    string // this binary, installed on the host when it differs
	Log     *slog.Logger
}

// Host drives one remote host. It is up once it answered a ping with this
// daemon's version and prepared its Docker backend.
type Host struct {
	cfg  HostConfig
	log  *slog.Logger
	up   atomic.Bool
	prep sync.Mutex // one probe at a time
}

// NewHost builds a Host; it starts down until Probe succeeds.
func NewHost(cfg HostConfig) *Host {
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Host{cfg: cfg, log: log}
}

// Name is the host's configured name.
func (h *Host) Name() string { return h.cfg.Name }

// Up reports the last probe's verdict. It never blocks.
func (h *Host) Up() bool { return h.up.Load() }

func (h *Host) agentCmd() string { return h.cfg.Exec + " " + h.cfg.Binary + " agent" }

// errAgent marks a host that answered over SSH but not as a working agent of
// this version (missing binary, old version, garbled output).
var errAgent = errors.New("agent unusable")

func (h *Host) call(ctx context.Context, req Request) (Response, error) {
	req.Config = h.cfg.Agent
	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, err
	}
	out, errs, code, err := h.cfg.Cmd.Command(ctx, h.agentCmd(), string(body))
	if err != nil {
		return Response{}, fmt.Errorf("linux host %s: %w", h.cfg.Name, err)
	}
	var resp Response
	if jerr := json.Unmarshal(lastLine(out), &resp); jerr != nil {
		return resp, fmt.Errorf("%w on %s: exit %d: %s", errAgent, h.cfg.Name, code, tail(errs))
	}
	if resp.Error != "" {
		return resp, fmt.Errorf("linux host %s: %s", h.cfg.Name, resp.Error)
	}
	return resp, nil
}

func lastLine(s string) []byte {
	s = strings.TrimSpace(s)
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		s = s[i+1:]
	}
	return []byte(s)
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}

// Probe checks the host, installs this binary when the agent is missing or
// another version, and prepares the Docker backend. It sets Up.
func (h *Host) Probe(ctx context.Context) error {
	h.prep.Lock()
	defer h.prep.Unlock()
	err := h.probe(ctx)
	was := h.up.Swap(err == nil)
	switch {
	case err != nil && was:
		h.log.Warn("linux host down", "host", h.cfg.Name, "error", err.Error())
	case err != nil:
		h.log.Debug("linux host unavailable", "host", h.cfg.Name, "error", err.Error())
	case !was:
		h.log.Info("linux host up", "host", h.cfg.Name)
	}
	return err
}

func (h *Host) probe(ctx context.Context) error {
	resp, err := h.call(ctx, Request{Op: OpPing})
	if err != nil && !errors.Is(err, errAgent) {
		return err
	}
	if err != nil || resp.Version != h.cfg.Version {
		if h.cfg.Self == "" {
			return fmt.Errorf("linux host %s: agent version %q, want %q", h.cfg.Name, resp.Version, h.cfg.Version)
		}
		if err := h.install(ctx); err != nil {
			return err
		}
	}
	if h.up.Load() {
		return nil // prepared before; the ping is enough
	}
	_, err = h.call(ctx, Request{Op: OpPrepare})
	return err
}

func goArch(uname string) string {
	switch strings.TrimSpace(uname) {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return strings.TrimSpace(uname)
}

// install copies this binary to the host and checks it arrived intact.
func (h *Host) install(ctx context.Context) error {
	out, errs, code, err := h.cfg.Cmd.Command(ctx, h.cfg.Exec+" uname -m", "")
	if err != nil || code != 0 {
		return fmt.Errorf("linux host %s: uname: %w %s", h.cfg.Name, errOr(err, code), tail(errs))
	}
	if a := goArch(out); a != runtime.GOARCH {
		return fmt.Errorf("linux host %s is %s, this binary %s: install runner-warden at %s yourself", h.cfg.Name, a, runtime.GOARCH, h.cfg.Binary)
	}
	bin, err := os.ReadFile(h.cfg.Self)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(bin)
	want := hex.EncodeToString(sum[:])
	dst := h.cfg.Binary
	script := fmt.Sprintf("umask 022 && mkdir -p %s && cat > %s.new && chmod 755 %s.new && sha256sum %s.new && mv -f %s.new %s",
		path.Dir(dst), dst, dst, dst, dst, dst)
	h.log.Info("installing runner-warden on linux host", "host", h.cfg.Name, "version", h.cfg.Version)
	out, errs, code, err = h.cfg.Cmd.Command(ctx, h.cfg.Exec+" sh -c '"+script+"'", string(bin))
	if err != nil || code != 0 {
		return fmt.Errorf("linux host %s: install: %w %s", h.cfg.Name, errOr(err, code), tail(errs))
	}
	if !bytes.HasPrefix([]byte(strings.TrimSpace(out)), []byte(want)) {
		return fmt.Errorf("linux host %s: installed binary checksum mismatch", h.cfg.Name)
	}
	return nil
}

// Caps implements core.Backend.
func (h *Host) Caps() core.Caps { return core.Caps{OS: core.Linux, DockerInJob: true} }

// Prepare implements core.Backend.
func (h *Host) Prepare(ctx context.Context) error { return h.Probe(ctx) }

// Start implements core.Backend. The JIT config travels on stdin.
func (h *Host) Start(ctx context.Context, spec core.RunnerSpec) (core.Handle, error) {
	if !h.Up() {
		return core.Handle{}, fmt.Errorf("linux host %s is down", h.cfg.Name)
	}
	resp, err := h.call(ctx, Request{Op: OpStart, Spec: &spec})
	return resp.Handle, err
}

// Stats implements core.Backend.
func (h *Host) Stats(ctx context.Context, hd core.Handle) (core.Usage, error) {
	resp, err := h.call(ctx, Request{Op: OpStats, Handle: &hd})
	return resp.Usage, err
}

// Stop implements core.Backend.
func (h *Host) Stop(ctx context.Context, hd core.Handle, grace time.Duration) error {
	_, err := h.call(ctx, Request{Op: OpStop, Handle: &hd, Grace: grace})
	return err
}

// Cleanup implements core.Backend.
func (h *Host) Cleanup(ctx context.Context, runner string) error {
	_, err := h.call(ctx, Request{Op: OpCleanup, Runner: runner})
	return err
}

// List implements core.Backend.
func (h *Host) List(ctx context.Context) ([]core.Handle, error) {
	resp, err := h.call(ctx, Request{Op: OpList})
	return resp.Handles, err
}

// Adopt re-checks an adopted runner's Docker proxy.
func (h *Host) Adopt(ctx context.Context, hd core.Handle) error {
	_, err := h.call(ctx, Request{Op: OpAdopt, Handle: &hd})
	return err
}

// errOr is err, or the non-zero exit status as an error.
func errOr(err error, code int) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("exit status %d", code)
}
