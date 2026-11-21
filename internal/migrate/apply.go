package migrate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

// Mode says what Apply does with the rewritten workflows.
type Mode string

// Apply modes.
const (
	ModePrint Mode = "print" // return unified diffs, change nothing
	ModePR    Mode = "pr"    // one PR per repo on a dedicated branch
	ModePush  Mode = "push"  // commit straight to the default branch
)

// DefaultBranchName is the prefix of the branch used in PR mode; each run adds
// a timestamp so it never reuses a branch someone else may have pre-seeded.
const DefaultBranchName = "runner-warden/labels"

// Writer is the REST surface Apply needs (implemented by *gh.REST).
type Writer interface {
	GitHub
	CreateBranch(ctx context.Context, owner, repo, branch, base string) (string, bool, error)
	CommitFiles(ctx context.Context, owner, repo, branch, message string, files []gh.FileChange) (string, bool, error)
	OpenPR(ctx context.Context, owner, repo, head, base, title, body string) (gh.PR, error)
	UpdateVariable(ctx context.Context, scope core.Target, name, value string) error
}

// PRLister is optionally implemented by a Writer to report older open
// runner-warden/labels* PRs, which Apply leaves alone.
type PRLister interface {
	ListOpenPRs(ctx context.Context, owner, repo, headPrefix string) ([]string, error)
}

// ErrBranchExists means the PR branch is already there; Apply never commits
// onto a branch it did not create.
var ErrBranchExists = errors.New("branch already exists")

// ApplyOptions configures Apply.
type ApplyOptions struct {
	Mode Mode
	// UpdateVars also updates Actions variables holding legacy labels
	// (pr and push modes only). Updating a variable takes effect immediately
	// for every workflow that reads it, so do it only once the new scale sets
	// are live.
	UpdateVars bool
	Branch     string // PR mode; default DefaultBranchName-<yyyymmdd-hhmmss>
	Now        func() time.Time
	Message    string
	Title      string
	GH         Writer
	Log        *slog.Logger
}

// RepoResult is the outcome for one repo.
type RepoResult struct {
	Owner    string   `json:"owner"`
	Repo     string   `json:"repo"`
	Files    []string `json:"files,omitempty"`
	Diff     string   `json:"diff,omitempty"` // print mode
	Branch   string   `json:"branch,omitempty"`
	Commit   string   `json:"commit,omitempty"`
	Changed  bool     `json:"changed"` // a commit was made by this run
	PRNumber int      `json:"prNumber,omitempty"`
	PRURL    string   `json:"prUrl,omitempty"`
	OpenPRs  []string `json:"openPrs,omitempty"` // older runner-warden/labels* PRs left alone
	Error    string   `json:"error,omitempty"`
}

// VarResult is the outcome for one variable.
type VarResult struct {
	Name    string      `json:"name"`
	Scope   core.Target `json:"scope"`
	Updated bool        `json:"updated"`
	Error   string      `json:"error,omitempty"`
}

// ApplyResult is the outcome of Apply.
type ApplyResult struct {
	Repos []RepoResult `json:"repos"`
	Vars  []VarResult  `json:"vars,omitempty"`
}

// Apply rewrites the workflows named in plan. It re-reads each file from the
// default branch and re-runs the rewrite, so drift since Scan is harmless, and
// re-running it is idempotent (existing branch and PR are reused).
func Apply(ctx context.Context, plan *Plan, opts ApplyOptions) (*ApplyResult, error) {
	switch opts.Mode {
	case ModePrint, ModePR, ModePush:
	default:
		return nil, fmt.Errorf("apply: unknown mode %q", opts.Mode)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Branch == "" {
		opts.Branch = BranchName(opts.Now())
	}
	if opts.Message == "" {
		opts.Message = "Use " + plan.Prefix + " runner labels"
	}
	if opts.Title == "" {
		opts.Title = opts.Message
	}
	log := opts.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	res := &ApplyResult{}
	for _, r := range plan.Repos {
		if !r.HasChanges() {
			continue
		}
		rr := applyRepo(ctx, plan, r, opts)
		if rr.Error != "" {
			log.Warn("apply repo failed", "repo", r.Owner+"/"+r.Repo, "error", rr.Error)
		}
		res.Repos = append(res.Repos, rr)
		if err := ctx.Err(); err != nil {
			return res, err
		}
	}
	if opts.UpdateVars && opts.Mode != ModePrint {
		for _, v := range plan.Vars {
			if v.New == "" {
				continue
			}
			res.Vars = append(res.Vars, applyVar(ctx, opts, v))
		}
	}
	return res, nil
}

func applyRepo(ctx context.Context, plan *Plan, r RepoScan, opts ApplyOptions) RepoResult {
	rr := RepoResult{Owner: r.Owner, Repo: r.Repo}
	var files []gh.FileChange
	var diffs []string
	for _, f := range r.Files {
		if len(f.Changes) == 0 {
			continue
		}
		src, _, err := opts.GH.GetFile(ctx, r.Owner, r.Repo, f.Path, r.DefaultBranch)
		if err != nil {
			rr.Error = err.Error()
			return rr
		}
		out, changes, _ := Rewrite(src, plan.Rules)
		if len(changes) == 0 {
			continue
		}
		files = append(files, gh.FileChange{Path: f.Path, Content: out})
		rr.Files = append(rr.Files, f.Path)
		diffs = append(diffs, unifiedDiff(f.Path, "", string(src), string(out)))
	}
	if len(files) == 0 {
		return rr
	}
	switch opts.Mode {
	case ModePrint:
		rr.Diff = strings.Join(diffs, "")
	case ModePush:
		rr.Branch = r.DefaultBranch
		sha, changed, err := opts.GH.CommitFiles(ctx, r.Owner, r.Repo, r.DefaultBranch, opts.Message, files)
		rr.Commit, rr.Changed = sha, changed
		if err != nil {
			rr.Error = err.Error()
		}
	case ModePR:
		rr.Branch = opts.Branch
		if l, ok := opts.GH.(PRLister); ok {
			if urls, err := l.ListOpenPRs(ctx, r.Owner, r.Repo, DefaultBranchName); err == nil {
				rr.OpenPRs = urls
			}
		}
		_, created, err := opts.GH.CreateBranch(ctx, r.Owner, r.Repo, opts.Branch, r.DefaultBranch)
		if err != nil {
			rr.Error = err.Error()
			return rr
		}
		if !created {
			rr.Error = ErrBranchExists.Error()
			return rr
		}
		sha, changed, err := opts.GH.CommitFiles(ctx, r.Owner, r.Repo, opts.Branch, opts.Message, files)
		if err != nil {
			rr.Error = err.Error()
			return rr
		}
		rr.Commit, rr.Changed = sha, changed
		pr, err := opts.GH.OpenPR(ctx, r.Owner, r.Repo, opts.Branch, r.DefaultBranch, opts.Title, prBody(plan, r))
		if err != nil {
			rr.Error = err.Error()
			return rr
		}
		rr.PRNumber, rr.PRURL = pr.Number, pr.URL
	}
	return rr
}

// BranchName is the per-run PR branch name.
func BranchName(t time.Time) string {
	return DefaultBranchName + "-" + t.UTC().Format("20060102-150405")
}

// prBody is a compact table of what changed and what was left alone.
func prBody(plan *Plan, r RepoScan) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Moves `runs-on` to the `%s-*` runner labels. Only the label text changes.\n\n", plan.Prefix)
	sb.WriteString("| File | Job | Old | New |\n|---|---|---|---|\n")
	for _, f := range r.Files {
		for _, c := range f.Changes {
			fmt.Fprintf(&sb, "| %s:%d | %s | `%s` | `%s` |\n", f.Path, c.Line, c.Job, md(c.Old), md(c.New))
		}
	}
	var left []string
	for _, f := range r.Files {
		for _, s := range f.Skipped {
			if s.Reason == ReasonVariable {
				left = append(left, fmt.Sprintf("| %s:%d | %s | variable `%s` | review |", f.Path, s.Line, s.Job, s.Var))
				continue
			}
			left = append(left, fmt.Sprintf("| %s:%d | %s | %s | left as is |", f.Path, s.Line, s.Job, s.Reason))
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		sb.WriteString("\nNot rewritten:\n\n| File | Job | Why | Action |\n|---|---|---|---|\n")
		sb.WriteString(strings.Join(left, "\n") + "\n")
	}
	return sb.String()
}

func md(s string) string { return strings.ReplaceAll(s, "`", "'") }

func applyVar(ctx context.Context, opts ApplyOptions, v VarUpdate) VarResult {
	vr := VarResult{Name: v.Name, Scope: v.Scope}
	cur, found, err := opts.GH.FindVariable(ctx, v.Scope.Owner, v.Scope.Repo, v.Name)
	switch {
	case err != nil:
		vr.Error = err.Error()
	case !found:
		vr.Error = string(ReasonVarNotFound)
	case cur.Value == v.New:
		vr.Updated = false // already done
	case cur.Value != v.Old:
		vr.Error = "value changed since scan"
	default:
		if err := opts.GH.UpdateVariable(ctx, v.Scope, v.Name, v.New); err != nil {
			vr.Error = err.Error()
		} else {
			vr.Updated = true
		}
	}
	return vr
}
