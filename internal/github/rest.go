package gh

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/google/go-github/v93/github"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Typed REST errors.
var (
	// ErrRunnerBusy: deleting a runner that is running a job (HTTP 422).
	ErrRunnerBusy = errors.New("runner is busy")
	// ErrNotFound wraps HTTP 404.
	ErrNotFound = errors.New("not found")
	// ErrJobNotFound: no job with that name in the run.
	ErrJobNotFound = errors.New("job not found in run")
)

// ScopesUnknown is returned by TokenScopes for tokens that do not report
// OAuth scopes (fine-grained PATs, app tokens).
const ScopesUnknown = "unknown"

const workflowsDir = ".github/workflows"

// REST is a thin, typed wrapper over the GitHub REST API.
type REST struct {
	c *github.Client
}

// NewREST builds a REST client. Extra options (for example github.WithURLs in
// tests) are applied after the token.
func NewREST(token string, opts ...github.ClientOptionsFunc) (*REST, error) {
	all := append([]github.ClientOptionsFunc{github.WithAuthToken(token)}, opts...)
	c, err := github.NewClient(all...)
	if err != nil {
		return nil, fmt.Errorf("github client: %w", err)
	}
	return &REST{c: c}, nil
}

func wrap(op string, resp *github.Response, err error) error {
	var accepted *github.AcceptedError
	if err == nil || errors.As(err, &accepted) { // 202: queued, not a failure
		return nil
	}
	if resp != nil && resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w: %w", op, ErrNotFound, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func status(resp *github.Response) int {
	if resp == nil || resp.Response == nil {
		return 0
	}
	return resp.StatusCode
}

// ---- runners ----

// Runner is a registered self-hosted runner.
type Runner struct {
	ID     int64
	Name   string
	OS     string
	Status string // online | offline
	Busy   bool
	Labels []string
}

func toRunner(r *github.Runner) Runner {
	out := Runner{ID: r.GetID(), Name: r.GetName(), OS: r.GetOS(), Status: r.GetStatus(), Busy: r.GetBusy()}
	for _, l := range r.Labels {
		out.Labels = append(out.Labels, l.GetName())
	}
	return out
}

// ListRunners lists runners registered at a target (repo or org).
func (r *REST) ListRunners(ctx context.Context, t core.Target) ([]Runner, error) {
	var out []Runner
	opts := &github.ListRunnersOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		var rs *github.Runners
		var resp *github.Response
		var err error
		if t.IsOrg() {
			rs, resp, err = r.c.Actions.ListOrganizationRunners(ctx, t.Owner, opts)
		} else {
			rs, resp, err = r.c.Actions.ListRunners(ctx, t.Owner, t.Repo, opts)
		}
		if err != nil {
			return nil, wrap("list runners", resp, err)
		}
		for _, x := range rs.Runners {
			out = append(out, toRunner(x))
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// DeleteRunner deregisters a runner. A busy runner yields ErrRunnerBusy; a
// runner that is already gone is not an error.
func (r *REST) DeleteRunner(ctx context.Context, t core.Target, id int64) error {
	var resp *github.Response
	var err error
	if t.IsOrg() {
		resp, err = r.c.Actions.RemoveOrganizationRunner(ctx, t.Owner, id)
	} else {
		resp, err = r.c.Actions.RemoveRunner(ctx, t.Owner, t.Repo, id)
	}
	switch {
	case err == nil, status(resp) == http.StatusNotFound:
		return nil
	case status(resp) == http.StatusUnprocessableEntity:
		return fmt.Errorf("delete runner %d: %w", id, ErrRunnerBusy)
	}
	return wrap("delete runner", resp, err)
}

// CreateRegistrationToken returns a runner registration token for config.sh.
func (r *REST) CreateRegistrationToken(ctx context.Context, t core.Target) (string, error) {
	var tok *github.RegistrationToken
	var resp *github.Response
	var err error
	if t.IsOrg() {
		tok, resp, err = r.c.Actions.CreateOrganizationRegistrationToken(ctx, t.Owner)
	} else {
		tok, resp, err = r.c.Actions.CreateRegistrationToken(ctx, t.Owner, t.Repo)
	}
	if err != nil {
		return "", wrap("registration token", resp, err)
	}
	return tok.GetToken(), nil
}

// RunnerGroup is an org runner group.
type RunnerGroup struct {
	ID      int64
	Name    string
	Default bool
}

// ListOrgRunnerGroups lists an org's runner groups.
func (r *REST) ListOrgRunnerGroups(ctx context.Context, org string) ([]RunnerGroup, error) {
	var out []RunnerGroup
	opts := &github.ListOrgRunnerGroupOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		gs, resp, err := r.c.Actions.ListOrganizationRunnerGroups(ctx, org, opts)
		if err != nil {
			return nil, wrap("list runner groups", resp, err)
		}
		for _, g := range gs.RunnerGroups {
			out = append(out, RunnerGroup{ID: g.GetID(), Name: g.GetName(), Default: g.GetDefault()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// ---- runs and jobs ----

// Run is the subset of a workflow run the manager needs.
type Run struct {
	ID         int64
	Event      string
	HeadBranch string
	HeadSHA    string
	PRs        []int
	Status     string
	Conclusion string
	CreatedAt  time.Time
}

// GetRun fetches a workflow run.
func (r *REST) GetRun(ctx context.Context, owner, repo string, runID int64) (Run, error) {
	w, resp, err := r.c.Actions.GetWorkflowRunByID(ctx, owner, repo, runID)
	if err != nil {
		return Run{}, wrap("get run", resp, err)
	}
	out := Run{ID: w.GetID(), Event: w.GetEvent(), HeadBranch: w.GetHeadBranch(), HeadSHA: w.GetHeadSHA(),
		Status: w.GetStatus(), Conclusion: w.GetConclusion(), CreatedAt: w.GetCreatedAt().Time}
	for _, p := range w.PullRequests {
		out.PRs = append(out.PRs, p.GetNumber())
	}
	return out, nil
}

// CancelRun asks GitHub to cancel a run.
func (r *REST) CancelRun(ctx context.Context, owner, repo string, runID int64) error {
	resp, err := r.c.Actions.CancelWorkflowRunByID(ctx, owner, repo, runID)
	return wrap("cancel run", resp, err)
}

// ForceCancelRun force-cancels a run (use after CancelRun did not take).
func (r *REST) ForceCancelRun(ctx context.Context, owner, repo string, runID int64) error {
	u := fmt.Sprintf("repos/%s/%s/actions/runs/%d/force-cancel", owner, repo, runID)
	req, err := r.c.NewRequest(ctx, http.MethodPost, u, nil)
	if err != nil {
		return fmt.Errorf("force cancel run: %w", err)
	}
	resp, err := r.c.Do(req, nil)
	return wrap("force cancel run", resp, err)
}

// RerunJob re-runs the job named jobName of run runID. The scale-set JobID is
// a GUID, so the numeric REST id is found by listing the run's jobs.
func (r *REST) RerunJob(ctx context.Context, owner, repo string, runID int64, jobName string) error {
	opts := &github.ListWorkflowJobsOptions{Filter: "latest", ListOptions: github.ListOptions{PerPage: 100}}
	var jobID int64
	for jobID == 0 {
		jobs, resp, err := r.c.Actions.ListWorkflowJobs(ctx, owner, repo, runID, opts)
		if err != nil {
			return wrap("list jobs", resp, err)
		}
		for _, j := range jobs.Jobs {
			if j.GetName() == jobName {
				jobID = j.GetID()
				break
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if jobID == 0 {
		return fmt.Errorf("%w: run %d job %q", ErrJobNotFound, runID, jobName)
	}
	resp, err := r.c.Actions.RerunJobByID(ctx, owner, repo, jobID)
	return wrap("rerun job", resp, err)
}

// DispatchWorkflow triggers a workflow_dispatch run (smoke test).
func (r *REST) DispatchWorkflow(ctx context.Context, owner, repo, file, ref string, inputs map[string]any) error {
	_, resp, err := r.c.Actions.CreateWorkflowDispatchEventByFileName(ctx, owner, repo, file,
		github.CreateWorkflowDispatchEventRequest{Ref: ref, Inputs: inputs})
	return wrap("dispatch workflow", resp, err)
}

// ---- pull requests, branches, repos ----

// PRState is the lifecycle state of a pull request.
type PRState struct {
	Number   int
	Merged   bool
	Closed   bool
	ClosedAt time.Time // merge or close time; zero while open
}

// GetPRState reports whether a PR is merged or closed.
func (r *REST) GetPRState(ctx context.Context, owner, repo string, number int) (PRState, error) {
	p, resp, err := r.c.PullRequests.Get(ctx, owner, repo, number)
	if err != nil {
		return PRState{}, wrap("get pull request", resp, err)
	}
	return PRState{Number: number, Merged: p.GetMerged(), Closed: p.GetState() == "closed", ClosedAt: p.GetClosedAt().Time}, nil
}

// PRsForCommit lists PRs (open, closed or merged) whose head is sha and whose
// head branch is branch. A run's own pull_requests list is emptied once its
// PR merges, so stale-run checks use this instead.
func (r *REST) PRsForCommit(ctx context.Context, owner, repo, sha, branch string) ([]PRState, error) {
	prs, resp, err := r.c.PullRequests.ListPullRequestsWithCommit(ctx, owner, repo, sha, &github.ListOptions{PerPage: 100})
	if err != nil {
		return nil, wrap("list pull requests for commit", resp, err)
	}
	var out []PRState
	for _, p := range prs {
		if branch != "" && p.GetHead().GetRef() != branch {
			continue
		}
		out = append(out, PRState{Number: p.GetNumber(), Merged: !p.GetMergedAt().IsZero(), Closed: p.GetState() == "closed", ClosedAt: p.GetClosedAt().Time})
	}
	return out, nil
}

// BranchExists reports whether a branch exists.
func (r *REST) BranchExists(ctx context.Context, owner, repo, branch string) (bool, error) {
	_, resp, err := r.c.Repositories.GetBranch(ctx, owner, repo, branch, 0)
	if err == nil {
		return true, nil
	}
	if status(resp) == http.StatusNotFound {
		return false, nil
	}
	return false, wrap("get branch", resp, err)
}

// TagExists reports whether a tag exists.
func (r *REST) TagExists(ctx context.Context, owner, repo, tag string) (bool, error) {
	_, resp, err := r.c.Git.GetRef(ctx, owner, repo, "tags/"+tag)
	if err == nil {
		return true, nil
	}
	if status(resp) == http.StatusNotFound {
		return false, nil
	}
	return false, wrap("get tag", resp, err)
}

// Repo is the subset of repository metadata needed for scanning.
type Repo struct {
	Owner         string
	Name          string
	DefaultBranch string
	Archived      bool
	Private       bool
	Fork          bool
}

// ListOrgRepos lists every repository of an org (including archived).
func (r *REST) ListOrgRepos(ctx context.Context, org string) ([]Repo, error) {
	var out []Repo
	opts := &github.RepositoryListByOrgOptions{ListOptions: github.ListOptions{PerPage: 100}}
	for {
		rs, resp, err := r.c.Repositories.ListByOrg(ctx, org, opts)
		if err != nil {
			return nil, wrap("list org repos", resp, err)
		}
		for _, x := range rs {
			out = append(out, Repo{Owner: org, Name: x.GetName(), DefaultBranch: x.GetDefaultBranch(),
				Archived: x.GetArchived(), Private: x.GetPrivate(), Fork: x.GetFork()})
		}
		if resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// GetRepo returns repository metadata (visibility, default branch).
func (r *REST) GetRepo(ctx context.Context, owner, repo string) (Repo, error) {
	x, resp, err := r.c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return Repo{}, wrap("get repo", resp, err)
	}
	return Repo{Owner: owner, Name: x.GetName(), DefaultBranch: x.GetDefaultBranch(), Archived: x.GetArchived(), Private: x.GetPrivate(), Fork: x.GetFork()}, nil
}

// DefaultBranch returns a repository's default branch.
func (r *REST) DefaultBranch(ctx context.Context, owner, repo string) (string, error) {
	x, resp, err := r.c.Repositories.Get(ctx, owner, repo)
	if err != nil {
		return "", wrap("get repo", resp, err)
	}
	return x.GetDefaultBranch(), nil
}

// TokenScopes returns the OAuth scopes of the token (X-OAuth-Scopes). Tokens
// that do not report scopes (fine-grained PATs) yield []string{ScopesUnknown}.
func (r *REST) TokenScopes(ctx context.Context) ([]string, error) {
	req, err := r.c.NewRequest(ctx, http.MethodGet, "user", nil)
	if err != nil {
		return nil, fmt.Errorf("token scopes: %w", err)
	}
	resp, err := r.c.Do(req, nil)
	if err != nil {
		return nil, wrap("token scopes", resp, err)
	}
	hv, ok := resp.Header["X-Oauth-Scopes"]
	if !ok {
		return []string{ScopesUnknown}, nil
	}
	var out []string
	for _, part := range strings.Split(strings.Join(hv, ","), ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out, nil
}

// ---- files and commits ----

// WorkflowFile is a workflow file on a ref.
type WorkflowFile struct {
	Path string
	SHA  string
}

// ListWorkflowFiles lists .yml/.yaml files in .github/workflows at ref. A
// missing directory yields no files.
func (r *REST) ListWorkflowFiles(ctx context.Context, owner, repo, ref string) ([]WorkflowFile, error) {
	_, dir, resp, err := r.c.Repositories.GetContents(ctx, owner, repo, workflowsDir, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		if status(resp) == http.StatusNotFound {
			return nil, nil
		}
		return nil, wrap("list workflow files", resp, err)
	}
	var out []WorkflowFile
	for _, f := range dir {
		if f.GetType() != "file" {
			continue
		}
		if e := path.Ext(f.GetName()); e == ".yml" || e == ".yaml" {
			out = append(out, WorkflowFile{Path: f.GetPath(), SHA: f.GetSHA()})
		}
	}
	return out, nil
}

// GetFile returns a file's content and blob SHA at ref.
func (r *REST) GetFile(ctx context.Context, owner, repo, p, ref string) ([]byte, string, error) {
	f, _, resp, err := r.c.Repositories.GetContents(ctx, owner, repo, p, &github.RepositoryContentGetOptions{Ref: ref})
	if err != nil {
		return nil, "", wrap("get file", resp, err)
	}
	if f == nil {
		return nil, "", fmt.Errorf("get file: %q is a directory", p)
	}
	s, err := f.GetContent()
	if err != nil {
		return nil, "", fmt.Errorf("decode file: %w", err)
	}
	return []byte(s), f.GetSHA(), nil
}

// FileChange is one file written by CommitFiles.
type FileChange struct {
	Path    string
	Content []byte
}

// CreateBranch creates branch from the tip of base and returns the new tip
// SHA. If the branch already exists it is left alone and its tip is returned
// with created=false.
func (r *REST) CreateBranch(ctx context.Context, owner, repo, branch, base string) (sha string, created bool, err error) {
	if ref, resp, gerr := r.c.Git.GetRef(ctx, owner, repo, "heads/"+branch); gerr == nil {
		return ref.GetObject().GetSHA(), false, nil
	} else if status(resp) != http.StatusNotFound {
		return "", false, wrap("get branch ref", resp, gerr)
	}
	b, resp, err := r.c.Git.GetRef(ctx, owner, repo, "heads/"+base)
	if err != nil {
		return "", false, wrap("get base ref", resp, err)
	}
	sha = b.GetObject().GetSHA()
	if _, resp, err = r.c.Git.CreateRef(ctx, owner, repo, github.CreateRef{Ref: "refs/heads/" + branch, SHA: sha}); err != nil {
		return "", false, wrap("create branch", resp, err)
	}
	return sha, true, nil
}

// CommitFiles writes files to branch as ONE commit (git data API) and returns
// the commit SHA. If every file already matches the branch tip, nothing is
// committed and the tip SHA is returned with changed=false.
func (r *REST) CommitFiles(ctx context.Context, owner, repo, branch, message string, files []FileChange) (sha string, changed bool, err error) {
	ref, resp, err := r.c.Git.GetRef(ctx, owner, repo, "heads/"+branch)
	if err != nil {
		return "", false, wrap("get branch ref", resp, err)
	}
	tip := ref.GetObject().GetSHA()
	commit, resp, err := r.c.Git.GetCommit(ctx, owner, repo, tip)
	if err != nil {
		return "", false, wrap("get tip commit", resp, err)
	}
	var entries []*github.TreeEntry
	for _, f := range files {
		cur, _, gerr := r.GetFile(ctx, owner, repo, f.Path, branch)
		if gerr == nil && string(cur) == string(f.Content) {
			continue
		}
		if gerr != nil && !errors.Is(gerr, ErrNotFound) {
			return "", false, gerr
		}
		entries = append(entries, &github.TreeEntry{
			Path: github.Ptr(f.Path), Mode: github.Ptr("100644"), Type: github.Ptr("blob"),
			Content: github.Ptr(string(f.Content)),
		})
	}
	if len(entries) == 0 {
		return tip, false, nil
	}
	tree, resp, err := r.c.Git.CreateTree(ctx, owner, repo, commit.GetTree().GetSHA(), entries)
	if err != nil {
		return "", false, wrap("create tree", resp, err)
	}
	nc, resp, err := r.c.Git.CreateCommit(ctx, owner, repo, github.Commit{
		Message: github.Ptr(message), Tree: tree, Parents: []*github.Commit{{SHA: github.Ptr(tip)}},
	}, nil)
	if err != nil {
		return "", false, wrap("create commit", resp, err)
	}
	if _, resp, err = r.c.Git.UpdateRef(ctx, owner, repo, "heads/"+branch, github.UpdateRef{SHA: nc.GetSHA()}); err != nil {
		return "", false, wrap("update branch", resp, err)
	}
	return nc.GetSHA(), true, nil
}

// PR is an opened or reused pull request.
type PR struct {
	Number  int
	URL     string
	Created bool
}

// OpenPR opens a PR from head into base, or returns the open one for head.
func (r *REST) OpenPR(ctx context.Context, owner, repo, head, base, title, body string) (PR, error) {
	prs, resp, err := r.c.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		State: "open", Head: owner + ":" + head, Base: base,
	})
	if err != nil {
		return PR{}, wrap("list pull requests", resp, err)
	}
	if len(prs) > 0 {
		return PR{Number: prs[0].GetNumber(), URL: prs[0].GetHTMLURL()}, nil
	}
	p, resp, err := r.c.PullRequests.Create(ctx, owner, repo, github.CreatePullRequest{
		Title: github.Ptr(title), Head: head, Base: base, Body: github.Ptr(body),
	})
	if err != nil {
		return PR{}, wrap("create pull request", resp, err)
	}
	return PR{Number: p.GetNumber(), URL: p.GetHTMLURL(), Created: true}, nil
}

// ---- Actions variables ----

// Variable is an Actions variable and where it was found.
type Variable struct {
	Name  string
	Value string
	Scope core.Target
}

// FindVariable looks a variable up the way a workflow does: repo first, then
// the owner's org. found=false when neither has it.
func (r *REST) FindVariable(ctx context.Context, owner, repo, name string) (Variable, bool, error) {
	if repo != "" {
		v, resp, err := r.c.Actions.GetRepoVariable(ctx, owner, repo, name)
		if err == nil {
			return Variable{Name: name, Value: v.Value, Scope: core.Target{Owner: owner, Repo: repo}}, true, nil
		}
		if status(resp) != http.StatusNotFound {
			return Variable{}, false, wrap("get repo variable", resp, err)
		}
	}
	v, resp, err := r.c.Actions.GetOrgVariable(ctx, owner, name)
	if err == nil {
		return Variable{Name: name, Value: v.Value, Scope: core.Target{Owner: owner}}, true, nil
	}
	if status(resp) == http.StatusNotFound || status(resp) == http.StatusForbidden {
		return Variable{}, false, nil
	}
	return Variable{}, false, wrap("get org variable", resp, err)
}

// UpdateVariable sets a variable's value at its scope (repo or org).
func (r *REST) UpdateVariable(ctx context.Context, scope core.Target, name, value string) error {
	var resp *github.Response
	var err error
	if scope.IsOrg() {
		resp, err = r.c.Actions.UpdateOrgVariable(ctx, scope.Owner, name, github.ActionsUpdateOrgVariableRequest{Value: github.Ptr(value)})
	} else {
		resp, err = r.c.Actions.UpdateRepoVariable(ctx, scope.Owner, scope.Repo, name, github.ActionsUpdateVariableRequest{Value: github.Ptr(value)})
	}
	return wrap("update variable", resp, err)
}
