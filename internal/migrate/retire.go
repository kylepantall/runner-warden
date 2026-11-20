package migrate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
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
	// Allowed is the explicit allow-list of targets whose runners may be
	// retired; a runner registered anywhere else is refused. Empty refuses all.
	Allowed []core.Target
	// TrustedUID owns the unit files and runner dirs we trust (default 0 = root).
	TrustedUID int
}

// RunnerStatus is a runner's progress in a manifest.
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

// Steps, in order (idle check, deregister, then the local steps).
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

// Refusal is a runner Retire declined to touch.
type Refusal struct {
	Name   string `json:"name"`
	Dir    string `json:"dir,omitempty"`
	URL    string `json:"url,omitempty"`
	Reason string `json:"reason"`
}

// ReasonNotEligible and friends are Refusal reasons beyond the scan reasons.
const (
	ReasonNotEligible  = "not_eligible"
	ReasonNotAllowed   = "target_not_allowed"
	ReasonUnitMismatch = "unit_mismatch"
	ReasonDirChanged   = "dir_changed"
)

// Manifest describes a retire session (manifest.json in the session dir).
type Manifest struct {
	Session   string         `json:"session"`
	CreatedAt time.Time      `json:"createdAt"`
	Runners   []RunnerRecord `json:"runners"`
	Refused   []Refusal      `json:"refused,omitempty"` // from the latest Retire call
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
	b, err := os.ReadFile(path) //nolint:gosec // manifest path under the root-owned state dir
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
	var accepted []LocalRunner
	var refused []Refusal
	for _, lr := range runners {
		if reason := r.vet(lr); reason != "" {
			refused = append(refused, Refusal{Name: lr.Name, Dir: lr.Dir, URL: lr.URL, Reason: reason})
			continue
		}
		accepted = append(accepted, lr)
	}
	if m == nil {
		if len(accepted) == 0 {
			return &Manifest{Refused: refused}, nil
		}
		ts := r.Now().UTC().Format("20060102T150405Z")
		m = &Manifest{Session: filepath.Join(r.migrationDir(), ts), CreatedAt: r.Now().UTC()}
		if err := os.MkdirAll(m.Session, 0o700); err != nil {
			return nil, fmt.Errorf("create session dir: %w", err)
		}
	}
	m.Refused = refused
	for _, lr := range accepted {
		if m.find(lr) >= 0 {
			continue
		}
		rec := RunnerRecord{LocalRunner: lr, Status: StatusPending}
		rec.ArchiveDir = filepath.Join(m.Session, safeName(lr))
		rec.Target, _ = TargetFromURL(lr.URL)
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

// vet decides whether a runner may be retired: it must come from a root-owned
// unit (Eligible), carry safe names, and be registered to an allowed target.
func (r *Retirer) vet(lr LocalRunner) string {
	switch {
	case !lr.Eligible || lr.DirIno == 0:
		return ReasonNotEligible
	case !ValidIdent(lr.Name) || !ValidUnit(lr.Unit) || !ValidDir(lr.Dir):
		return ReasonBadName
	case lr.UnitPath != filepath.Join(SystemdDir, lr.Unit):
		return ReasonUnitMismatch
	}
	if want, ok := expectedUnit(lr.URL, lr.Name); !ok || want != lr.Unit {
		return ReasonUnitMismatch
	}
	t, ok := TargetFromURL(lr.URL)
	if !ok || !r.allowed(t) {
		return ReasonNotAllowed
	}
	return ""
}

func (r *Retirer) allowed(t core.Target) bool {
	for _, a := range r.Allowed {
		if strings.EqualFold(a.Owner, t.Owner) && strings.EqualFold(a.Repo, t.Repo) {
			return true
		}
	}
	return false
}

// checkDir confirms an opened directory is the one recorded at scan time and
// is owned by root or the unit's user.
func (r *Retirer) checkDir(rec *RunnerRecord, fd int) error {
	di, err := fstatDir(fd)
	if err != nil {
		return err
	}
	if rec.DirIno == 0 || di.Dev != rec.DirDev || di.Ino != rec.DirIno || di.UID != rec.DirUID ||
		!ownerAllowed(di.UID, rec.User, r.TrustedUID) {
		return errors.New(ReasonDirChanged)
	}
	return nil
}

// verifyUnit re-reads the root-owned unit and checks it still points at the
// recorded dir and user.
func (r *Retirer) verifyUnit(rec *RunnerRecord) error {
	text, err := readUnit(r.Root, rec.Unit, r.TrustedUID)
	if errors.Is(err, fs.ErrNotExist) {
		if exists(filepath.Join(rec.ArchiveDir, "unit", rec.Unit)) {
			rec.UnitArchived = true // moved by an interrupted earlier run
			return nil
		}
	}
	if err != nil {
		return err
	}
	if dirFromUnit(text) != rec.Dir || userFromUnit(text) != rec.User {
		return errors.New(ReasonUnitMismatch)
	}
	return nil
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

// errPending stops a runner's steps without failing it.
var errPending = errors.New("pending")

func (r *Retirer) retireOne(ctx context.Context, _ *Manifest, rec *RunnerRecord, j *Journal) {
	name := rec.Name
	if name == "" {
		name = rec.Dir
	}
	run := func(s string, always bool, fn func() (string, error)) bool {
		if !always && j.Done(name, s) {
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
	step := func(s string, fn func() (string, error)) bool { return run(s, false, fn) }
	rec.Status, rec.Error = StatusPending, ""

	if reason := r.vet(rec.LocalRunner); reason != "" {
		rec.Status, rec.Error = StatusFailed, reason
		return
	}
	if rec.Unit != "" && !rec.UnitArchived {
		if err := r.verifyUnit(rec); err != nil {
			rec.Status, rec.Error = StatusFailed, err.Error()
			return
		}
	}

	// The idle check repeats on every attempt until the runner is deregistered
	// (after which GitHub assigns it no new jobs), so a resumed retire never
	// acts on a stale "idle". Deregistering comes before stopping: GitHub
	// refuses to delete a busy runner, which closes the check-then-stop gap.
	if !j.Done(name, StepDeregister) {
		if !run(StepIdleCheck, true, func() (string, error) { return r.idleCheck(ctx, rec) }) {
			if rec.Status != StatusFailed {
				rec.Status = StatusBusy
			}
			return
		}
	}
	if !step(StepDeregister, func() (string, error) { return r.deregister(ctx, rec) }) {
		if rec.Status != StatusFailed {
			rec.Status = StatusDeregister
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
	if rec.UnitArchived {
		return nil
	}
	ufd, err := openChain(r.Root, SystemdDir)
	if err != nil {
		return err
	}
	defer closeFd(ufd)
	dfd, err := r.openArchive(rec, "unit")
	if err != nil {
		return err
	}
	defer closeFd(dfd)
	err = renameAt(ufd, rec.Unit, dfd, rec.Unit)
	if errors.Is(err, fs.ErrNotExist) {
		return nil // already moved
	}
	if err == nil {
		rec.UnitArchived = true
	}
	return err
}

// openArchive opens (creating) <ArchiveDir>/<sub>, which lives in the state dir.
func (r *Retirer) openArchive(rec *RunnerRecord, sub string) (int, error) {
	p := filepath.Join(rec.ArchiveDir, sub)
	if err := os.MkdirAll(p, 0o700); err != nil {
		return -1, err
	}
	return openPlainDir(p)
}

func (r *Retirer) archiveDir(rec *RunnerRecord) error {
	dirfd, err := openChain(r.Root, rec.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer closeFd(dirfd)
	if err := r.checkDir(rec, dirfd); err != nil {
		return err
	}
	names, err := listDirAt(dirfd)
	if err != nil {
		return err
	}
	sort.Strings(names)
	afd, err := r.openArchive(rec, "dir")
	if err != nil {
		return err
	}
	defer closeFd(afd)
	for _, n := range names {
		if n == "_work" {
			continue
		}
		if err := renameAt(dirfd, n, afd, n); err != nil {
			return err
		}
		rec.MovedEntries = append(rec.MovedEntries, n)
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
	Unit string   `json:"unit,omitempty"` // systemd unit to start afterwards
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
		if err := r.validateRecord(m, rec); err != nil {
			return out, fmt.Errorf("manifest record of %s: %w", name, err)
		}
		if rec.Status != StatusRolledBack {
			if rec.UnitArchived && !j.Done(rname, "restore_unit") {
				if err := r.restoreUnit(rec); err != nil {
					_ = j.Append(rname, "restore_unit", StepError, err.Error())
					return out, fmt.Errorf("restore unit of %s: %w", name, err)
				}
				_ = j.Append(rname, "restore_unit", StepOK, "")
				reload = true
			}
			if !j.Done(rname, "restore_dir") {
				if err := r.restoreDir(rec); err != nil {
					_ = j.Append(rname, "restore_dir", StepError, err.Error())
					return out, fmt.Errorf("restore dir of %s: %w", name, err)
				}
				_ = j.Append(rname, "restore_dir", StepOK, "")
			}
		}
		rr := ReRegister{Name: rec.Name, Dir: rec.Dir, Unit: rec.Unit, URL: rec.URL, Labels: rec.Labels, NeedsRegistration: rec.Deregistered}
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

func (r *Retirer) validateRecord(m *Manifest, rec *RunnerRecord) error {
	bad := errors.New("unsafe manifest record")
	if rec.Name != "" && !ValidIdent(rec.Name) {
		return bad
	}
	for _, l := range rec.Labels {
		if !ValidIdent(l) {
			return bad
		}
	}
	if rec.Unit != "" && (!ValidUnit(rec.Unit) || rec.UnitPath != filepath.Join(SystemdDir, rec.Unit)) {
		return bad
	}
	if rec.Dir != "" && !ValidDir(rec.Dir) {
		return bad
	}
	for _, e := range rec.MovedEntries {
		if !plainName(e) {
			return bad
		}
	}
	if rec.ArchiveDir != "" && filepath.Dir(rec.ArchiveDir) != filepath.Clean(m.Session) {
		return bad
	}
	return nil
}

// restoreUnit puts the archived unit back. The destination directory is
// opened without following symlinks and nothing is created or overwritten.
func (r *Retirer) restoreUnit(rec *RunnerRecord) error {
	dst, err := openChain(r.Root, SystemdDir)
	if err != nil {
		return err
	}
	defer closeFd(dst)
	src, err := openPlainDir(filepath.Join(rec.ArchiveDir, "unit"))
	if err != nil {
		return err
	}
	defer closeFd(src)
	return renameAt(src, rec.Unit, dst, rec.Unit)
}

// restoreDir puts archived entries back into the original runner dir, but only
// if that dir is still the one recorded at scan time (same inode, no symlinks
// on the path, expected owner). It never creates directories.
func (r *Retirer) restoreDir(rec *RunnerRecord) error {
	if len(rec.MovedEntries) == 0 {
		return nil
	}
	src, err := openPlainDir(filepath.Join(rec.ArchiveDir, "dir"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil // already restored on a previous run
	}
	if err != nil {
		return err
	}
	defer closeFd(src)
	dst, err := openChain(r.Root, rec.Dir)
	if err != nil {
		return err
	}
	defer closeFd(dst)
	if err := r.checkDir(rec, dst); err != nil {
		return err
	}
	for _, e := range rec.MovedEntries {
		if err := renameAt(src, e, dst, e); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}
