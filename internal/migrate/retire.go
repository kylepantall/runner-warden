package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

// CommandRunner runs an external command (systemctl); injectable for tests.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// ExecRunner runs commands on the host.
func ExecRunner(ctx context.Context, name string, args ...string) ([]byte, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return out, nil
}

// RunnerAPI is the REST surface Retire and Rollback need (*gh.REST).
type RunnerAPI interface {
	ListRunners(ctx context.Context, t core.Target) ([]gh.Runner, error)
	DeleteRunner(ctx context.Context, t core.Target, id int64) error
	CreateRegistrationToken(ctx context.Context, t core.Target) (string, error)
}

// Retirer retires legacy local runners.
type Retirer struct {
	Root     string // filesystem root, default "/"
	StateDir string // archives live in <StateDir>/migration/<timestamp>/
	Run      CommandRunner
	API      RunnerAPI
	Now      func() time.Time
	Log      *slog.Logger
}

// Runner status in a manifest.
type RunnerStatus string

// Statuses.
const (
	StatusPending    RunnerStatus = "pending"      // not started
	StatusBusy       RunnerStatus = "pending_busy" // runner is running a job; retry later
	StatusDeregister RunnerStatus = "pending_deregister"
	StatusDone       RunnerStatus = "done"
	StatusFailed     RunnerStatus = "failed"
	StatusRolledBack RunnerStatus = "rolled_back"
)

// Steps, in order.
const (
	StepIdleCheck    = "idle_check"
	StepStop         = "stop"
	StepDisable      = "disable"
	StepArchiveUnit  = "archive_unit"
	StepDaemonReload = "daemon_reload"
	StepArchiveDir   = "archive_dir"
	StepDeregister   = "deregister"
)

// RunnerRecord is one runner in a manifest.
type RunnerRecord struct {
	LocalRunner
	Target       core.Target  `json:"target"`
	RunnerID     int64        `json:"runnerId,omitempty"`
	ArchiveDir   string       `json:"archiveDir"`             // <session>/<name>
	MovedEntries []string     `json:"movedEntries,omitempty"` // names moved out of Dir
	UnitArchived bool         `json:"unitArchived,omitempty"`
	Deregistered bool         `json:"deregistered"`
	Status       RunnerStatus `json:"status"`
	Error        string       `json:"error,omitempty"`
}

// Manifest describes a retire session (manifest.json in the session dir).
type Manifest struct {
	Session   string         `json:"session"`
	CreatedAt time.Time      `json:"createdAt"`
	Runners   []RunnerRecord `json:"runners"`
}

func (m *Manifest) open() bool {
	for _, r := range m.Runners {
		if r.Status != StatusDone && r.Status != StatusRolledBack {
			return true
		}
	}
	return false
}

func (r *Retirer) defaults() {
	if r.Root == "" {
		r.Root = "/"
	}
	if r.Run == nil {
		r.Run = ExecRunner
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Log == nil {
		r.Log = slog.New(slog.DiscardHandler)
	}
}

func (r *Retirer) real(logical string) string { return filepath.Join(r.Root, logical) }

func (r *Retirer) migrationDir() string { return filepath.Join(r.StateDir, "migration") }

// TargetFromURL maps a GitHub URL to a target (owner or owner/repo).
func TargetFromURL(raw string) (core.Target, bool) {
	u, err := url.Parse(raw)
	if err != nil {
		return core.Target{}, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return core.Target{Owner: parts[0]}, true
	case len(parts) >= 2 && parts[0] != "" && parts[1] != "":
		return core.Target{Owner: parts[0], Repo: parts[1]}, true
	}
	return core.Target{}, false
}

// findSession returns the latest session that still has work to do.
func (r *Retirer) findSession() (*Manifest, error) {
	dirs, _ := filepath.Glob(filepath.Join(r.migrationDir(), "*", "manifest.json"))
	sort.Strings(dirs)
	for i := len(dirs) - 1; i >= 0; i-- {
		m, err := loadManifest(dirs[i])
		if err == nil && m.open() {
			return m, nil
		}
	}
	return nil, nil
}

func loadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("manifest %s: %w", path, err)
	}
	return &m, nil
}

func (r *Retirer) save(m *Manifest) error {
	p := filepath.Join(m.Session, "manifest.json")
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Retire stops, disables and archives the given legacy runners and
// deregisters them from GitHub. It never touches a busy runner and never
// forces a deregistration: a busy runner stays pending and the next call
// (with the same or an overlapping runner list) resumes the open session.
func (r *Retirer) Retire(ctx context.Context, runners []LocalRunner) (*Manifest, error) {
	r.defaults()
	m, err := r.findSession()
	if err != nil {
		return nil, err
	}
	if m == nil {
		ts := r.Now().UTC().Format("20060102T150405Z")
		m = &Manifest{Session: filepath.Join(r.migrationDir(), ts), CreatedAt: r.Now().UTC()}
		if err := os.MkdirAll(m.Session, 0o700); err != nil {
			return nil, fmt.Errorf("create session dir: %w", err)
		}
	}
	for _, lr := range runners {
		if m.find(lr) >= 0 {
			continue
		}
		rec := RunnerRecord{LocalRunner: lr, Status: StatusPending}
		rec.ArchiveDir = filepath.Join(m.Session, safeName(lr))
		if t, ok := TargetFromURL(lr.URL); ok {
			rec.Target = t
		}
		m.Runners = append(m.Runners, rec)
	}
	j, err := OpenJournal(filepath.Join(m.Session, "journal.jsonl"), r.Now)
	if err != nil {
		return nil, err
	}
	if err := r.save(m); err != nil {
		return nil, err
	}
	for i := range m.Runners {
		rec := &m.Runners[i]
		if rec.Status == StatusDone || rec.Status == StatusRolledBack {
			continue
		}
		r.retireOne(ctx, m, rec, j)
		if err := r.save(m); err != nil {
			return m, err
		}
		if err := ctx.Err(); err != nil {
			return m, err
		}
	}
	return m, nil
}

func (m *Manifest) find(lr LocalRunner) int {
	for i, x := range m.Runners {
		if x.Dir == lr.Dir && x.Name == lr.Name && x.Unit == lr.Unit {
			return i
		}
	}
	return -1
}

func safeName(lr LocalRunner) string {
	n := lr.Name
	if n == "" {
		n = strings.ReplaceAll(strings.Trim(lr.Dir, "/"), "/", "_")
	}
	return strings.Map(func(r rune) rune {
		if r == '/' || r == 0 {
			return '_'
		}
		return r
	}, n)
}

// errPending stops a runner's steps without failing it.
var errPending = errors.New("pending")

func (r *Retirer) retireOne(ctx context.Context, m *Manifest, rec *RunnerRecord, j *Journal) {
	name := rec.Name
	if name == "" {
		name = rec.Dir
	}
	step := func(s string, fn func() (string, error)) bool {
		if j.Done(name, s) {
			return true
		}
		detail, err := fn()
		switch {
		case errors.Is(err, errPending):
			_ = j.Append(name, s, StepPending, detail)
			return false
		case err != nil:
			rec.Status, rec.Error = StatusFailed, err.Error()
			r.Log.Warn("retire step failed", "runner", name, "step", s, "error", err.Error())
			_ = j.Append(name, s, StepError, err.Error())
			return false
		}
		_ = j.Append(name, s, StepOK, detail)
		return true
	}
	rec.Status, rec.Error = StatusPending, ""

	if !step(StepIdleCheck, func() (string, error) { return r.idleCheck(ctx, rec) }) {
		if rec.Status != StatusFailed {
			rec.Status = StatusBusy
		}
		return
	}
	if rec.Unit != "" {
		if !step(StepStop, func() (string, error) { _, err := r.Run(ctx, "systemctl", "stop", rec.Unit); return "", err }) {
			return
		}
		if !step(StepDisable, func() (string, error) { _, err := r.Run(ctx, "systemctl", "disable", rec.Unit); return "", err }) {
			return
		}
	}
	if rec.UnitPath != "" {
		if !step(StepArchiveUnit, func() (string, error) { return "", r.archiveUnit(rec) }) {
			return
		}
		if !step(StepDaemonReload, func() (string, error) { _, err := r.Run(ctx, "systemctl", "daemon-reload"); return "", err }) {
			return
		}
	}
	if rec.Dir != "" {
		if !step(StepArchiveDir, func() (string, error) { return "", r.archiveDir(rec) }) {
			return
		}
	}
	if !step(StepDeregister, func() (string, error) { return r.deregister(ctx, rec) }) {
		if rec.Status != StatusFailed {
			rec.Status = StatusDeregister
		}
		return
	}
	rec.Status = StatusDone
}

func (r *Retirer) lookup(ctx context.Context, rec *RunnerRecord) (gh.Runner, bool, error) {
	if rec.Target.Owner == "" || r.API == nil {
		return gh.Runner{}, false, nil
	}
	rs, err := r.API.ListRunners(ctx, rec.Target)
	if err != nil {
		return gh.Runner{}, false, err
	}
	for _, x := range rs {
		if x.Name == rec.Name {
			return x, true, nil
		}
	}
	return gh.Runner{}, false, nil
}

// idleCheck refuses to touch a runner that is running a job. If GitHub cannot
// be asked, it fails rather than guess.
func (r *Retirer) idleCheck(ctx context.Context, rec *RunnerRecord) (string, error) {
	x, found, err := r.lookup(ctx, rec)
	if err != nil {
		return "", fmt.Errorf("cannot verify runner is idle: %w", err)
	}
	if !found {
		return "not registered", nil
	}
	rec.RunnerID = x.ID
	if x.Busy {
		return "busy", errPending
	}
	return "idle", nil
}

func (r *Retirer) archiveUnit(rec *RunnerRecord) error {
	src := r.real(rec.UnitPath)
	if _, err := os.Lstat(src); errors.Is(err, fs.ErrNotExist) {
		return nil // already moved
	}
	if err := move(src, filepath.Join(rec.ArchiveDir, "unit", filepath.Base(rec.UnitPath))); err != nil {
		return err
	}
	rec.UnitArchived = true
	return nil
}

func (r *Retirer) archiveDir(rec *RunnerRecord) error {
	src := r.real(rec.Dir)
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Name() == "_work" {
			continue
		}
		if err := move(filepath.Join(src, e.Name()), filepath.Join(rec.ArchiveDir, "dir", e.Name())); err != nil {
			return err
		}
		rec.MovedEntries = append(rec.MovedEntries, e.Name())
	}
	return nil
}

func (r *Retirer) deregister(ctx context.Context, rec *RunnerRecord) (string, error) {
	if rec.Target.Owner == "" || r.API == nil {
		return "no github url", nil
	}
	if rec.RunnerID == 0 {
		x, found, err := r.lookup(ctx, rec)
		if err != nil {
			return "", err
		}
		if !found {
			return "already gone", nil
		}
		rec.RunnerID = x.ID
	}
	err := r.API.DeleteRunner(ctx, rec.Target, rec.RunnerID)
	switch {
	case errors.Is(err, gh.ErrRunnerBusy):
		return "busy", errPending
	case err != nil:
		return "", err
	}
	rec.Deregistered = true
	return "deleted", nil
}

// ---- rollback ----

// ReRegister is what is needed to register a restored runner again.
type ReRegister struct {
	Name   string   `json:"name"`
	Dir    string   `json:"dir"`
	URL    string   `json:"url"`
	Labels []string `json:"labels"`
	Token  string   `json:"-"` // registration token (short-lived); never journaled or persisted
	// Args is the config.sh command line to run in Dir (contains the token).
	Args []string `json:"-"`
	// NeedsRegistration is false when the runner was never deregistered.
	NeedsRegistration bool `json:"needsRegistration"`
}

// Rollback restores archived units and runner directories from a manifest and
// returns the re-registration commands. It does not start services. A restore
// never overwrites: an existing destination is an error.
func (r *Retirer) Rollback(ctx context.Context, manifestPath string) ([]ReRegister, error) {
	r.defaults()
	m, err := loadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	m.Session = filepath.Dir(manifestPath)
	j, err := OpenJournal(filepath.Join(m.Session, "journal.jsonl"), r.Now)
	if err != nil {
		return nil, err
	}
	var out []ReRegister
	reload := false
	for i := range m.Runners {
		rec := &m.Runners[i]
		name := rec.Name
		if name == "" {
			name = rec.Dir
		}
		rname := "rollback:" + name
		if rec.Status != StatusRolledBack {
			if rec.UnitArchived && !j.Done(rname, "restore_unit") {
				err := move(filepath.Join(rec.ArchiveDir, "unit", filepath.Base(rec.UnitPath)), r.real(rec.UnitPath))
				if err != nil {
					_ = j.Append(rname, "restore_unit", StepError, err.Error())
					return out, fmt.Errorf("restore unit of %s: %w", name, err)
				}
				_ = j.Append(rname, "restore_unit", StepOK, "")
				reload = true
			}
			if !j.Done(rname, "restore_dir") {
				for _, e := range rec.MovedEntries {
					if err := move(filepath.Join(rec.ArchiveDir, "dir", e), filepath.Join(r.real(rec.Dir), e)); err != nil {
						if errors.Is(err, fs.ErrNotExist) {
							continue // already restored on a previous run
						}
						_ = j.Append(rname, "restore_dir", StepError, err.Error())
						return out, fmt.Errorf("restore dir of %s: %w", name, err)
					}
				}
				_ = j.Append(rname, "restore_dir", StepOK, "")
			}
		}
		rr := ReRegister{Name: rec.Name, Dir: rec.Dir, URL: rec.URL, Labels: rec.Labels, NeedsRegistration: rec.Deregistered}
		if rec.Deregistered && r.API != nil && rec.Target.Owner != "" {
			tok, err := r.API.CreateRegistrationToken(ctx, rec.Target)
			if err != nil {
				return out, fmt.Errorf("registration token for %s: %w", name, err)
			}
			rr.Token = tok
			rr.Args = []string{"./config.sh", "--url", rec.URL, "--token", tok, "--name", rec.Name,
				"--labels", strings.Join(rec.Labels, ","), "--unattended", "--replace"}
		}
		out = append(out, rr)
		rec.Status = StatusRolledBack
	}
	if reload {
		if _, err := r.Run(ctx, "systemctl", "daemon-reload"); err != nil {
			return out, err
		}
	}
	return out, r.save(m)
}

// ---- file moves ----

// move renames src to dst, falling back to copy+remove across filesystems. It
// refuses to overwrite.
func move(src, dst string) error {
	if _, err := os.Lstat(src); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		return fmt.Errorf("destination exists: %w", fs.ErrExist)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	var le *os.LinkError
	if errors.As(err, &le) && errors.Is(le.Err, syscall.EXDEV) {
		if err := copyTree(src, dst); err != nil {
			_ = os.RemoveAll(dst)
			return err
		}
		return os.RemoveAll(src)
	}
	return err
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		t := filepath.Join(dst, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(t, info.Mode().Perm())
		case info.Mode()&os.ModeSymlink != 0:
			l, err := os.Readlink(p)
			if err != nil {
				return err
			}
			return os.Symlink(l, t)
		case info.Mode().IsRegular():
			in, err := os.Open(p)
			if err != nil {
				return err
			}
			defer in.Close()
			out, err := os.OpenFile(t, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			return out.Close()
		}
		return nil
	})
}
