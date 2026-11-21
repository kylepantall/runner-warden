package migrate

import (
	"context"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

// GitHub is the read side of the REST API used by Scan (implemented by
// *gh.REST).
type GitHub interface {
	ListRunners(ctx context.Context, t core.Target) ([]gh.Runner, error)
	ListOrgRepos(ctx context.Context, org string) ([]gh.Repo, error)
	DefaultBranch(ctx context.Context, owner, repo string) (string, error)
	ListWorkflowFiles(ctx context.Context, owner, repo, ref string) ([]gh.WorkflowFile, error)
	GetFile(ctx context.Context, owner, repo, path, ref string) ([]byte, string, error)
	FindVariable(ctx context.Context, owner, repo, name string) (gh.Variable, bool, error)
}

// Plan is the JSON-serialisable result of Scan.
type Plan struct {
	Prefix    string        `json:"prefix"`
	Rules     Rules         `json:"rules"`
	CreatedAt time.Time     `json:"createdAt"`
	Targets   []TargetScan  `json:"targets"`
	Local     []LocalRunner `json:"local"`
	Repos     []RepoScan    `json:"repos"`
	Vars      []VarUpdate   `json:"vars,omitempty"`
}

// TargetScan lists the runners registered at a target.
type TargetScan struct {
	Target  core.Target `json:"target"`
	Runners []gh.Runner `json:"runners"`
	Error   string      `json:"error,omitempty"`
}

// RepoScan is every workflow file of one repo's default branch.
type RepoScan struct {
	Owner         string     `json:"owner"`
	Repo          string     `json:"repo"`
	DefaultBranch string     `json:"defaultBranch"`
	Files         []FileScan `json:"files,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// FileScan holds the runs-on occurrences of one workflow file.
type FileScan struct {
	Path      string      `json:"path"`
	SHA       string      `json:"sha"`
	Changes   []Change    `json:"changes,omitempty"`
	Skipped   []Skip      `json:"skipped,omitempty"`
	Untouched []Untouched `json:"untouched,omitempty"`
}

// VarUpdate is a proposed Actions-variable update.
type VarUpdate struct {
	Name  string      `json:"name"`
	Scope core.Target `json:"scope"`
	Old   string      `json:"old"`
	New   string      `json:"new,omitempty"`
	// Reason is set when no update is proposed (value not ours / not found).
	Reason Reason   `json:"reason,omitempty"`
	UsedBy []VarUse `json:"usedBy"`
}

// VarUse is a workflow that references a variable.
type VarUse struct {
	Owner string `json:"owner"`
	Repo  string `json:"repo"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

// HasChanges reports whether any file in the repo would be rewritten.
func (r RepoScan) HasChanges() bool {
	for _, f := range r.Files {
		if len(f.Changes) > 0 {
			return true
		}
	}
	return false
}
