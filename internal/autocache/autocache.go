// Package autocache keeps Linux jobs' tool cache and package-manager caches
// on the machine: one directory per scope (a repo, or an owner), mounted into
// the runner at MountPath with the tools' cache variables pointing into it.
// Usage is recorded per scope, and the least recently used scopes are removed
// when the caches outgrow their budget.
package autocache

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

// MountPath is where a scope's directory appears in the runner.
const MountPath = "/opt/rw-cache"

// usedFile marks a scope's last use; its mtime is the time.
const usedFile = ".rw-last-used"

// Env is what each tool is told; values are under MountPath.
var Env = map[string]string{
	// The cache root itself, so a workflow can tell a warm local cache is
	// there and skip actions/cache (whose restore downloads even on a hit).
	"RW_CACHE_DIR": MountPath,
	// The runner's tool cache: setup-dotnet/node/java/go/python/dart/... keep
	// each version they download.
	"RUNNER_TOOL_CACHE":    MountPath + "/tool",
	"AGENT_TOOLSDIRECTORY": MountPath + "/tool",
	"NUGET_PACKAGES":       MountPath + "/nuget",
	"npm_config_cache":     MountPath + "/npm",
	"YARN_CACHE_FOLDER":    MountPath + "/yarn",
	"PIP_CACHE_DIR":        MountPath + "/pip",
	"GOMODCACHE":           MountPath + "/go/mod",
	"GOCACHE":              MountPath + "/go/build",
	"GRADLE_USER_HOME":     MountPath + "/gradle",
	"PUB_CACHE":            MountPath + "/pub",
	"TF_PLUGIN_CACHE_DIR":  MountPath + "/terraform",
}

// Scope is the cache a job uses: "owner" or "owner/repo".
func Scope(mode string, t core.Target, j core.Job) string {
	owner, repo := strings.ToLower(j.Owner), strings.ToLower(j.Repo)
	if owner == "" {
		owner, repo = strings.ToLower(t.Owner), strings.ToLower(t.Repo)
	}
	if owner == "" {
		return ""
	}
	if mode == "org" || repo == "" {
		return owner
	}
	return owner + "/" + repo
}

// Dir is the scope's directory under base.
func Dir(base, scope string) string {
	return filepath.Join(base, filepath.FromSlash(strings.ReplaceAll(scope, "/", "__")))
}

// Mount is the bind for a scope's directory.
func Mount(base, scope string) core.Mount {
	return core.Mount{HostPath: Dir(base, scope), ContainerPath: MountPath}
}

// EnvFor is Env without the variables keep names (set by [[cache]] entries,
// which win).
func EnvFor(keep map[string]bool) map[string]string {
	out := map[string]string{}
	for k, v := range Env {
		if !keep[k] {
			out[k] = v
		}
	}
	return out
}

// Touch records a use of the scope now, creating its directory for uid.
func Touch(base, scope string, uid int) error { return TouchDir(Dir(base, scope), uid) }

// TouchDir is Touch for a scope's directory.
func TouchDir(dir string, uid int) error {
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // the runner user must reach it
		return err
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(dir, uid, uid)
	}
	// Terraform ignores a plugin cache dir that does not exist.
	tf := filepath.Join(dir, "terraform")
	if err := os.MkdirAll(tf, 0o755); err == nil && os.Geteuid() == 0 { //nolint:gosec // the runner user must reach it
		_ = os.Chown(tf, uid, uid)
	}
	f := filepath.Join(dir, usedFile)
	now := time.Now()
	if err := os.Chtimes(f, now, now); errors.Is(err, fs.ErrNotExist) {
		return os.WriteFile(f, nil, 0o644) //nolint:gosec // a timestamp marker
	} else if err != nil {
		return err
	}
	return nil
}

// Usage is one scope's cache.
type Usage struct {
	Machine  string    `json:"machine,omitempty"` // "" for this host
	Scope    string    `json:"scope"`
	Bytes    int64     `json:"bytes"`
	LastUsed time.Time `json:"last_used"`
}

// Scan lists every scope under base, most recently used first.
func Scan(base string) ([]Usage, error) {
	ents, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Usage
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(base, e.Name())
		u := Usage{Scope: strings.ReplaceAll(e.Name(), "__", "/")}
		if st, err := os.Stat(filepath.Join(dir, usedFile)); err == nil {
			u.LastUsed = st.ModTime()
		}
		_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if fi, err := d.Info(); err == nil {
					u.Bytes += fi.Size()
				}
			}
			return nil
		})
		out = append(out, u)
	}
	slices.SortFunc(out, func(a, b Usage) int { return b.LastUsed.Compare(a.LastUsed) })
	return out, nil
}

// Evict removes the least recently used scopes not in use until the rest fit
// in limit bytes, and returns what it removed. us is Scan's result.
func Evict(base string, us []Usage, limit int64, inUse map[string]bool) ([]Usage, error) {
	var total int64
	for _, u := range us {
		total += u.Bytes
	}
	var gone []Usage
	for i := len(us) - 1; i >= 0 && total > limit; i-- { // oldest last
		u := us[i]
		if inUse[u.Scope] {
			continue
		}
		if err := os.RemoveAll(Dir(base, u.Scope)); err != nil {
			return gone, err
		}
		total -= u.Bytes
		gone = append(gone, u)
	}
	return gone, nil
}
