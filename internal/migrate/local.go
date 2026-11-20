package migrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LocalRunner is a legacy self-hosted runner installed on this host.
//
// Only runners referenced by a root-owned systemd unit are Eligible for
// retirement; anything found by directory scanning is reporting-only, because
// the files in a user's home are attacker-controlled.
type LocalRunner struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"` // GitHub URL it is registered to
	Pool     string   `json:"pool,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Dir      string   `json:"dir,omitempty"`      // runner directory (logical absolute path)
	Unit     string   `json:"unit,omitempty"`     // systemd unit name
	UnitPath string   `json:"unitPath,omitempty"` // logical absolute path of the unit file
	User     string   `json:"user,omitempty"`     // User= of the unit; empty = root

	Eligible bool   `json:"eligible"`         // may be retired
	Reason   string `json:"reason,omitempty"` // why not Eligible (reason code)
	// Identity of Dir at scan time; retire refuses if it changed.
	DirDev uint64 `json:"dirDev,omitempty"`
	DirIno uint64 `json:"dirIno,omitempty"`
	DirUID uint32 `json:"dirUid,omitempty"`
}

// Reasons a runner is not retire-eligible.
const (
	ReasonNoUnit        = "no_root_unit"
	ReasonBadUnit       = "bad_unit"
	ReasonBadDir        = "bad_dir"
	ReasonBadOwner      = "bad_owner"
	ReasonBadName       = "bad_name"
	ReasonURLMismatch   = "url_mismatch"
	ReasonTargetNotUsed = "target_not_configured"
)

// SystemdDir is where actions.runner.*.service units live.
const SystemdDir = "/etc/systemd/system"

// DefaultHostRoots are searched for runner directories.
var DefaultHostRoots = []string{"/home/*", "/opt", "/srv"}

const (
	maxWalkDepth = 3
	maxSmallFile = 64 << 10
)

var skipDirs = map[string]bool{"_work": true, ".git": true, "node_modules": true, ".cache": true, "proc": true}

// lookupUID resolves a unit's User=; replaceable in tests.
var lookupUID = func(name string) (uint32, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseUint(u.Uid, 10, 32)
	return uint32(n), err == nil
}

// ownerAllowed: the runner dir must belong to root (trustedUID) or to the
// user the unit runs as.
func ownerAllowed(uid uint32, unitUser string, trustedUID int) bool {
	if int64(uid) == int64(trustedUID) {
		return true
	}
	if unitUser == "" {
		return false
	}
	u, ok := lookupUID(unitUser)
	return ok && u == uid
}

var (
	identRe = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	unitRe  = regexp.MustCompile(`^actions\.runner\.[A-Za-z0-9._-]+\.service$`)
)

// ValidIdent reports whether s is a safe runner name or label.
func ValidIdent(s string) bool { return identRe.MatchString(s) }

// ValidUnit reports whether s is an actions.runner.*.service unit name.
func ValidUnit(s string) bool { return unitRe.MatchString(s) }

// ValidDir reports whether d is an absolute, clean path.
func ValidDir(d string) bool {
	return strings.HasPrefix(d, "/") && filepath.Clean(d) == d && !strings.ContainsAny(d, "\x00\n")
}

// expectedUnit is the unit name the runner software generates for a runner
// registered to url as name.
func expectedUnit(url, name string) (string, bool) {
	t, ok := TargetFromURL(url)
	if !ok || !ValidIdent(name) {
		return "", false
	}
	scope := t.Owner
	if t.Repo != "" {
		scope += "-" + t.Repo
	}
	return "actions.runner." + scope + "." + name + ".service", true
}

type runnerFile struct {
	AgentName string `json:"agentName"`
	PoolName  string `json:"poolName"`
	ServerURL string `json:"serverUrl"`
	GitHubURL string `json:"gitHubUrl"`
}

// ScanLocal finds legacy runners under root (a filesystem root; "/" in
// production). Runners referenced by a unit file owned by trustedUID (0 =
// root) whose directory passes the checks are Eligible; runner dirs found by
// walking hostRoots are reported but never Eligible. A runner's .runner file
// is read only for reporting and for cross-checking the unit name.
func ScanLocal(root string, hostRoots []string, trustedUID int) ([]LocalRunner, error) {
	if root == "" {
		root = "/"
	}
	if len(hostRoots) == 0 {
		hostRoots = DefaultHostRoots
	}
	byDir := map[string]*LocalRunner{}
	var all []*LocalRunner

	units, _ := filepath.Glob(filepath.Join(root, SystemdDir, "actions.runner.*.service"))
	sort.Strings(units)
	for _, u := range units {
		unit := filepath.Base(u)
		lr := &LocalRunner{Unit: unit, UnitPath: filepath.Join(SystemdDir, unit), Reason: ReasonBadUnit}
		lr.Name = nameFromUnit(unit)
		all = append(all, lr)
		if !ValidUnit(unit) {
			continue
		}
		text, err := readUnit(root, unit, trustedUID)
		if err != nil {
			lr.Reason = ReasonNoUnit
			continue
		}
		lr.Dir, lr.User = dirFromUnit(text), userFromUnit(text)
		if lr.Dir == "" || !ValidDir(lr.Dir) {
			lr.Reason = ReasonBadDir
			continue
		}
		byDir[lr.Dir] = lr
		fillFromRunnerDir(lr, filepath.Join(root, lr.Dir))
		validateUnitRunner(root, lr, trustedUID)
	}

	for _, pat := range hostRoots {
		matches, _ := filepath.Glob(filepath.Join(root, pat))
		sort.Strings(matches)
		for _, m := range matches {
			walk(m, 0, func(dir string) {
				logical := "/" + strings.TrimPrefix(strings.TrimPrefix(dir, filepath.Clean(root)), "/")
				if byDir[logical] != nil {
					return
				}
				lr := &LocalRunner{Dir: logical, Reason: ReasonNoUnit}
				byDir[logical] = lr
				all = append(all, lr)
				fillFromRunnerDir(lr, dir)
			})
		}
	}
	out := make([]LocalRunner, 0, len(all))
	for _, l := range all {
		out = append(out, *l)
	}
	return out, nil
}

// validateUnitRunner decides Eligible for a unit-backed runner and records the
// identity of its directory.
func validateUnitRunner(root string, lr *LocalRunner, trustedUID int) {
	want, ok := expectedUnit(lr.URL, lr.Name)
	switch {
	case !ok || lr.Unit != want:
		lr.Reason = ReasonURLMismatch
		return
	case !ValidIdent(lr.Name):
		lr.Reason = ReasonBadName
		return
	}
	for _, l := range lr.Labels {
		if !ValidIdent(l) {
			lr.Reason = ReasonBadName
			return
		}
	}
	di, err := statChain(root, lr.Dir)
	if err != nil {
		lr.Reason = ReasonBadDir
		return
	}
	if !ownerAllowed(di.UID, lr.User, trustedUID) {
		lr.Reason = ReasonBadOwner
		return
	}
	lr.DirDev, lr.DirIno, lr.DirUID = di.Dev, di.Ino, di.UID
	lr.Eligible, lr.Reason = true, ""
}

// readUnit reads a unit file that is a regular file owned by trustedUID, under
// a no-follow path.
func readUnit(root, unit string, trustedUID int) (string, error) {
	fd, err := openChain(root, SystemdDir)
	if err != nil {
		return "", err
	}
	defer closeFd(fd)
	uid, err := statRegularAt(fd, unit)
	if err != nil {
		return "", err
	}
	if int64(uid) != int64(trustedUID) {
		return "", errUnsafePath
	}
	b, err := readFileAt(fd, unit, maxSmallFile)
	return string(b), err
}

func walk(dir string, depth int, found func(string)) {
	if isRegular(filepath.Join(dir, ".runner")) {
		found(dir)
		return // runner dirs do not nest
	}
	if depth >= maxWalkDepth {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() && !skipDirs[e.Name()] && e.Type()&os.ModeSymlink == 0 {
			walk(filepath.Join(dir, e.Name()), depth+1, found)
		}
	}
}

func isRegular(p string) bool {
	fi, err := os.Lstat(p)
	return err == nil && fi.Mode().IsRegular()
}

// readSmall reads a small regular file, refusing symlinks.
func readSmall(p string) ([]byte, error) {
	if !isRegular(p) {
		return nil, errUnsafePath
	}
	f, err := os.Open(p) //nolint:gosec // opened relative to a validated runner dir
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxSmallFile))
}

func fillFromRunnerDir(lr *LocalRunner, dir string) {
	if b, err := readSmall(filepath.Join(dir, ".runner")); err == nil {
		var rf runnerFile
		// The runner writes a UTF-8 BOM.
		if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &rf) == nil {
			lr.Name = firstNonEmpty(rf.AgentName, lr.Name)
			lr.Pool = rf.PoolName
			lr.URL = firstNonEmpty(rf.GitHubURL, rf.ServerURL)
		}
	}
	if b, err := readSmall(filepath.Join(dir, ".env")); err == nil {
		if l := labelsFromEnv(b); len(l) > 0 {
			lr.Labels = l
		}
	}
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// nameFromUnit: actions.runner.<scope>.<name>.service -> name.
func nameFromUnit(unit string) string {
	t := strings.TrimSuffix(unit, ".service")
	parts := strings.Split(t, ".")
	if len(parts) < 4 {
		return ""
	}
	return strings.Join(parts[3:], ".")
}

// dirFromUnit reads WorkingDirectory, else the directory of ExecStart.
func dirFromUnit(unit string) string {
	var exec string
	sc := bufio.NewScanner(strings.NewReader(unit))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "WorkingDirectory="):
			return strings.TrimSpace(strings.TrimPrefix(line, "WorkingDirectory="))
		case strings.HasPrefix(line, "ExecStart="):
			exec = strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		}
	}
	if exec == "" {
		return ""
	}
	exec = strings.TrimLeft(exec, "@-+!:")
	if f := strings.Fields(exec); len(f) > 0 {
		return filepath.Dir(f[0])
	}
	return ""
}

// userFromUnit reads User=; empty means the unit runs as root.
func userFromUnit(unit string) string {
	sc := bufio.NewScanner(strings.NewReader(unit))
	for sc.Scan() {
		if v, ok := strings.CutPrefix(strings.TrimSpace(sc.Text()), "User="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

var labelKeys = map[string]bool{"LABELS": true, "RUNNER_LABELS": true, "ACTIONS_RUNNER_LABELS": true}

func labelsFromEnv(b []byte) []string {
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok || !labelKeys[strings.TrimSpace(k)] {
			continue
		}
		var out []string
		for _, l := range strings.Split(strings.Trim(strings.TrimSpace(v), `"'`), ",") {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	return nil
}
