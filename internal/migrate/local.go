package migrate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// LocalRunner is a legacy self-hosted runner installed on this host.
type LocalRunner struct {
	Name     string   `json:"name"`
	URL      string   `json:"url"` // GitHub URL it is registered to
	Pool     string   `json:"pool,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Dir      string   `json:"dir,omitempty"`      // runner directory (logical absolute path)
	Unit     string   `json:"unit,omitempty"`     // systemd unit name
	UnitPath string   `json:"unitPath,omitempty"` // logical absolute path of the unit file
}

// SystemdDir is where actions.runner.*.service units live.
const SystemdDir = "/etc/systemd/system"

// DefaultHostRoots are searched for runner directories.
var DefaultHostRoots = []string{"/home/*", "/opt", "/srv"}

const maxWalkDepth = 3

var skipDirs = map[string]bool{"_work": true, ".git": true, "node_modules": true, ".cache": true, "proc": true}

type runnerFile struct {
	AgentName string `json:"agentName"`
	PoolName  string `json:"poolName"`
	ServerURL string `json:"serverUrl"`
	GitHubURL string `json:"gitHubUrl"`
}

// ScanLocal finds legacy runners under root (a filesystem root; "/" in
// production): systemd units and runner dirs holding a .runner file.
func ScanLocal(root string, hostRoots []string) ([]LocalRunner, error) {
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
		lr := &LocalRunner{Unit: filepath.Base(u), UnitPath: filepath.Join(SystemdDir, filepath.Base(u))}
		lr.Name = nameFromUnit(lr.Unit)
		if b, err := os.ReadFile(u); err == nil {
			lr.Dir = dirFromUnit(string(b))
		}
		all = append(all, lr)
		if lr.Dir != "" {
			byDir[lr.Dir] = lr
		}
	}

	for _, pat := range hostRoots {
		matches, _ := filepath.Glob(filepath.Join(root, pat))
		sort.Strings(matches)
		for _, m := range matches {
			walk(m, 0, func(dir string) {
				logical := "/" + strings.TrimPrefix(strings.TrimPrefix(dir, filepath.Clean(root)), "/")
				lr := byDir[logical]
				if lr == nil {
					lr = &LocalRunner{Dir: logical}
					byDir[logical] = lr
					all = append(all, lr)
				}
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

func walk(dir string, depth int, found func(string)) {
	if _, err := os.Stat(filepath.Join(dir, ".runner")); err == nil {
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

func fillFromRunnerDir(lr *LocalRunner, dir string) {
	if b, err := os.ReadFile(filepath.Join(dir, ".runner")); err == nil {
		var rf runnerFile
		// The runner writes a UTF-8 BOM.
		if json.Unmarshal(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), &rf) == nil {
			lr.Name = firstNonEmpty(rf.AgentName, lr.Name)
			lr.Pool = rf.PoolName
			lr.URL = firstNonEmpty(rf.GitHubURL, rf.ServerURL)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, ".env")); err == nil {
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
