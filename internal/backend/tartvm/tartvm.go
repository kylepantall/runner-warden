// Package tartvm runs macOS runners in a persistent tart VM on a separate
// Mac, driven over SSH with `tart exec`. Each job gets a fresh JIT runner,
// cloned (APFS clonefile) from a template runner install and started as a
// launchd job in the guest user's GUI session, so it can use the login
// keychain. The runner dir, and with it the work dir, is removed after the
// job; caches elsewhere in the guest stay warm. One job runs at a time.
package tartvm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// Commander runs a command line on the Mac with stdin (an SSH session).
type Commander interface {
	Command(ctx context.Context, cmd, stdin string) (stdout, stderr string, exit int, err error)
}

// Config describes one Mac and its VM.
type Config struct {
	Name     string // host name in the config
	VM       string // tart VM name
	Tart     string // tart binary on the Mac
	GuestUID int    // guest user owning the GUI session
	Instance string
	Prefix   string
	HostID   string
	// RunnerVersion pins actions/runner; "" uses the latest release.
	RunnerVersion string
	Cmd           Commander
	HTTP          *http.Client // release lookups; nil = default
	ReleaseAPI    string       // default the actions/runner latest-release API
	Now           func() time.Time
	Log           *slog.Logger
}

// Host drives one Mac's VM. It is up once the guest answered and holds a
// template runner of the wanted version.
type Host struct {
	cfg  Config
	log  *slog.Logger
	up   atomic.Bool
	prep sync.Mutex

	verMu   sync.Mutex
	version string
}

// New builds a Host; it starts down until Probe succeeds.
func New(cfg Config) *Host {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.ReleaseAPI == "" {
		cfg.ReleaseAPI = "https://api.github.com/repos/actions/runner/releases/latest"
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Host{cfg: cfg, log: cfg.Log, version: strings.TrimPrefix(cfg.RunnerVersion, "v")}
}

// Name is the host's configured name.
func (h *Host) Name() string { return h.cfg.Name }

// Up reports the last probe's verdict; it never blocks.
func (h *Host) Up() bool { return h.up.Load() }

// base is the guest dir holding the template and the runners.
const base = `"$HOME/rw/`

func (h *Host) base() string     { return base + h.cfg.Instance + `"` }
func (h *Host) template() string { return base + h.cfg.Instance + `/template"` }
func (h *Host) dir(runner string) string {
	return base + h.cfg.Instance + `/runners/` + runner + `"`
}
// tools is the tool cache shared by every job (setup-* actions, kept SDKs):
// the runner dir, and its default _work/_tool, go after each job.
func (h *Host) tools() string { return base + h.cfg.Instance + `/_tool"` }

func (h *Host) label(runner string) string { return "rw." + h.cfg.Instance + "." + runner }
func (h *Host) domain() string             { return "gui/" + strconv.Itoa(h.cfg.GuestUID) }

// guest runs a POSIX sh script in the VM; stdin carries it (and any secret).
func (h *Host) guest(ctx context.Context, script string) (string, error) {
	out, errs, code, err := h.cfg.Cmd.Command(ctx, h.cfg.Tart+" exec -i "+h.cfg.VM+" /bin/sh -s", "set -u\n"+script)
	if err != nil {
		return out, fmt.Errorf("mac host %s: %w", h.cfg.Name, err)
	}
	if code != 0 {
		return out, fmt.Errorf("mac host %s: guest exit %d: %s", h.cfg.Name, code, tail(errs+out))
	}
	return out, nil
}

func tail(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}

// Probe checks the guest, fixes clock drift (a Mac that slept leaves the
// guest behind, and the runner's token then looks expired) and makes sure the
// template runner is the wanted version. It sets Up.
func (h *Host) Probe(ctx context.Context) error {
	h.prep.Lock()
	defer h.prep.Unlock()
	err := h.probe(ctx)
	was := h.up.Swap(err == nil)
	switch {
	case err != nil && was:
		h.log.Warn("mac host down", "host", h.cfg.Name, "error", err.Error())
	case err != nil:
		h.log.Debug("mac host unavailable", "host", h.cfg.Name, "error", err.Error())
	case !was:
		h.log.Info("mac host up", "host", h.cfg.Name)
	}
	return err
}

// clockSkew is how far the guest clock may drift before it is synced.
const clockSkew = 30 * time.Second

func (h *Host) probe(ctx context.Context) error {
	out, err := h.guest(ctx, `date -u +%s; uname -m
T=`+h.template()+`
[ -x "$T/bin/Runner.Listener" ] && "$T/bin/Runner.Listener" --version || echo none`)
	if err != nil {
		return err
	}
	f := strings.Fields(out)
	if len(f) < 3 {
		return fmt.Errorf("mac host %s: unexpected probe output %q", h.cfg.Name, tail(out))
	}
	if t, err := strconv.ParseInt(f[0], 10, 64); err == nil {
		if skew := h.cfg.Now().Sub(time.Unix(t, 0)); skew > clockSkew || skew < -clockSkew {
			h.log.Info("syncing mac guest clock", "host", h.cfg.Name, "skew", skew.String())
			if _, err := h.guest(ctx, "sudo -n sntp -sS time.apple.com >/dev/null"); err != nil {
				h.log.Warn("mac guest clock sync failed", "host", h.cfg.Name, "error", err.Error())
			}
		}
	}
	want, err := h.runnerVersion(ctx)
	if err != nil {
		return err
	}
	if f[2] == want {
		return nil
	}
	arch := "osx-arm64"
	if f[1] == "x86_64" {
		arch = "osx-x64"
	}
	h.log.Info("installing runner template on mac host", "host", h.cfg.Name, "version", want)
	url := fmt.Sprintf("https://github.com/actions/runner/releases/download/v%s/actions-runner-%s-%s.tar.gz", want, arch, want)
	_, err = h.guest(ctx, `set -e
T=`+h.template()+`
rm -rf "$T.new" && mkdir -p "$T.new"
curl -fsSL "`+url+`" | tar -xz -C "$T.new"
rm -rf "$T" && mv "$T.new" "$T"
"$T/bin/Runner.Listener" --version`)
	return err
}

func (h *Host) runnerVersion(ctx context.Context) (string, error) {
	h.verMu.Lock()
	defer h.verMu.Unlock()
	if h.version != "" {
		return h.version, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.cfg.ReleaseAPI, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := h.cfg.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("runner release lookup: status %d", resp.StatusCode)
	}
	var rel struct {
		Tag string `json:"tag_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", err
	}
	v := strings.TrimPrefix(strings.TrimSpace(rel.Tag), "v")
	if v == "" {
		return "", errors.New("runner release lookup: no tag")
	}
	h.version = v
	return v, nil
}

// Caps implements core.Backend.
func (h *Host) Caps() core.Caps { return core.Caps{OS: core.MacOS, MaxConcurrent: 1} }

// Prepare implements core.Backend.
func (h *Host) Prepare(ctx context.Context) error { return h.Probe(ctx) }

var errBadRunner = errors.New("tartvm: invalid runner name")

func (h *Host) valid(n string) bool { return naming.ValidRunnerName(h.cfg.Prefix, h.cfg.HostID, n) }

// runCmd is the launchd job's command: $0 is the runner dir. The JIT config
// is read from a 0600 file and removed before the runner starts.
const runCmd = `cd "$0" || exit 1; J=$(cat .jit); rm -f .jit; exec ./run.sh --jitconfig "$J"`

// Start implements core.Backend.
func (h *Host) Start(ctx context.Context, spec core.RunnerSpec) (core.Handle, error) {
	if !h.valid(spec.Name) || spec.JITConfig == "" || strings.ContainsAny(spec.JITConfig, "'\n") {
		return core.Handle{}, errBadRunner
	}
	if !h.Up() {
		return core.Handle{}, fmt.Errorf("mac host %s is down", h.cfg.Name)
	}
	esc := strings.ReplaceAll(runCmd, "$", `\$`) // the heredoc expands only $D
	script := `set -e
D=` + h.dir(spec.Name) + `
TOOLS=` + h.tools() + `
mkdir -p "$(dirname "$D")" "$TOOLS" && rm -rf "$D"
cp -Rc ` + h.template() + ` "$D" 2>/dev/null || cp -R ` + h.template() + ` "$D"
umask 077
printf '%s' '` + spec.JITConfig + `' > "$D/.jit"
cat > "$D/job.plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>` + h.label(spec.Name) + `</string>
<key>RunAtLoad</key><true/>
<key>EnvironmentVariables</key><dict>
<key>RUNNER_TOOL_CACHE</key><string>$TOOLS</string>
<key>AGENT_TOOLSDIRECTORY</key><string>$TOOLS</string>
</dict>
<key>ProgramArguments</key><array><string>/bin/sh</string><string>-c</string><string>` + esc + `</string><string>$D</string></array>
<key>StandardOutPath</key><string>$D/runner.log</string>
<key>StandardErrorPath</key><string>$D/runner.log</string>
</dict></plist>
PLIST
launchctl bootstrap ` + h.domain() + ` "$D/job.plist"`
	if _, err := h.guest(ctx, script); err != nil {
		_ = h.Cleanup(context.WithoutCancel(ctx), spec.Name)
		return core.Handle{}, err
	}
	return core.Handle{RunnerName: spec.Name, BackendID: spec.Name, StartedAt: h.cfg.Now()}, nil
}

// Stats implements core.Backend: the runner's launchd state, and whole-guest
// CPU and memory (the VM runs one job).
func (h *Host) Stats(ctx context.Context, hd core.Handle) (core.Usage, error) {
	u := core.Usage{SampledAt: h.cfg.Now()}
	if !h.valid(hd.RunnerName) {
		return u, errBadRunner
	}
	out, err := h.guest(ctx, `D=`+h.dir(hd.RunnerName)+`
a=0; launchctl print `+h.domain()+`/`+h.label(hd.RunnerName)+` 2>/dev/null | grep -q "state = running" && a=1
cpu=$(ps -A -o time= | awk -F: '{s=0; for(i=1;i<=NF;i++) s=s*60+$i; t+=s} END {printf "%d", t}')
mem=$(ps -A -o rss= | awk '{t+=$1} END {printf "%d", t*1024}')
log=$(du -sk "$D/_diag" 2>/dev/null | cut -f1)
echo "alive=$a cpu=$cpu mem=$mem log=${log:-0}"`)
	if err != nil {
		return u, err
	}
	for _, kv := range strings.Fields(out) {
		k, v, _ := strings.Cut(kv, "=")
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "alive":
			u.Alive = n == 1
		case "cpu":
			u.CPUSeconds = float64(n)
		case "mem":
			u.MemCurrent = n
		case "log":
			u.LogBytes = n * 1024
		}
	}
	return u, nil
}

// Stop implements core.Backend; launchd sends SIGTERM, then SIGKILL.
func (h *Host) Stop(ctx context.Context, hd core.Handle, _ time.Duration) error {
	if !h.valid(hd.RunnerName) {
		return errBadRunner
	}
	_, err := h.guest(ctx, `launchctl bootout `+h.domain()+`/`+h.label(hd.RunnerName)+` 2>/dev/null; true`)
	return err
}

// Cleanup implements core.Backend: stop the job and remove its runner dir.
func (h *Host) Cleanup(ctx context.Context, runner string) error {
	if !h.valid(runner) {
		return errBadRunner
	}
	_, err := h.guest(ctx, `launchctl bootout `+h.domain()+`/`+h.label(runner)+` 2>/dev/null
rm -rf `+h.dir(runner))
	return err
}

// List implements core.Backend: runner dirs in the guest.
func (h *Host) List(ctx context.Context) ([]core.Handle, error) {
	out, err := h.guest(ctx, `cd `+h.base()+`/runners 2>/dev/null || exit 0
for d in *; do [ -d "$d" ] && echo "$d $(stat -f %B "$d")"; done; true`)
	if err != nil {
		return nil, err
	}
	var hs []core.Handle
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		name, at, _ := strings.Cut(strings.TrimSpace(line), " ")
		if !h.valid(name) {
			continue
		}
		t, _ := strconv.ParseInt(at, 10, 64)
		hs = append(hs, core.Handle{RunnerName: name, BackendID: name, StartedAt: time.Unix(t, 0)})
	}
	return hs, nil
}

// Adopt implements hostpool.Member; a launchd job needs no re-plumbing.
func (h *Host) Adopt(context.Context, core.Handle) error { return nil }
