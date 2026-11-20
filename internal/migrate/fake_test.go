package migrate

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

// fakeGH is an in-memory GitHub implementing GitHub, Writer and RunnerAPI.
type fakeGH struct {
	mu       sync.Mutex
	repos    []gh.Repo
	files    map[string]map[string]string // repo -> path -> content (default branch)
	branches map[string]map[string]string // repo/branch -> path -> content
	prs      map[string]gh.PR
	vars     map[string]string // "scope|name" -> value
	runners  map[string][]gh.Runner
	deleted  []int64
	busyDel  map[int64]bool
	commits  int
	tokens   int
}

func newFake() *fakeGH {
	return &fakeGH{files: map[string]map[string]string{}, branches: map[string]map[string]string{},
		prs: map[string]gh.PR{}, vars: map[string]string{}, runners: map[string][]gh.Runner{}, busyDel: map[int64]bool{}}
}

func (f *fakeGH) addRepo(owner, name string, archived bool, files map[string]string) {
	f.repos = append(f.repos, gh.Repo{Owner: owner, Name: name, DefaultBranch: "main", Archived: archived})
	f.files[owner+"/"+name] = files
}

func (f *fakeGH) ListRunners(_ context.Context, t core.Target) ([]gh.Runner, error) {
	return f.runners[t.String()], nil
}
func (f *fakeGH) ListOrgRepos(_ context.Context, org string) ([]gh.Repo, error) {
	var out []gh.Repo
	for _, r := range f.repos {
		if r.Owner == org {
			out = append(out, r)
		}
	}
	return out, nil
}
func (f *fakeGH) DefaultBranch(_ context.Context, _, _ string) (string, error) {
	return "main", nil
}
func (f *fakeGH) ListWorkflowFiles(_ context.Context, owner, repo, _ string) ([]gh.WorkflowFile, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []gh.WorkflowFile
	for p := range f.files[owner+"/"+repo] {
		out = append(out, gh.WorkflowFile{Path: p, SHA: "sha-" + p})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}
func (f *fakeGH) GetFile(_ context.Context, owner, repo, p, ref string) ([]byte, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if b, ok := f.branches[owner+"/"+repo+"@"+ref]; ok {
		if c, ok := b[p]; ok {
			return []byte(c), "sha", nil
		}
	}
	c, ok := f.files[owner+"/"+repo][p]
	if !ok {
		return nil, "", gh.ErrNotFound
	}
	return []byte(c), "sha-" + p, nil
}
func (f *fakeGH) FindVariable(_ context.Context, owner, repo, name string) (gh.Variable, bool, error) {
	if v, ok := f.vars[owner+"/"+repo+"|"+name]; ok {
		return gh.Variable{Name: name, Value: v, Scope: core.Target{Owner: owner, Repo: repo}}, true, nil
	}
	if v, ok := f.vars[owner+"|"+name]; ok {
		return gh.Variable{Name: name, Value: v, Scope: core.Target{Owner: owner}}, true, nil
	}
	return gh.Variable{}, false, nil
}
func (f *fakeGH) UpdateVariable(_ context.Context, scope core.Target, name, value string) error {
	f.vars[scope.String()+"|"+name] = value
	return nil
}
func (f *fakeGH) CreateBranch(_ context.Context, owner, repo, branch, _ string) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := owner + "/" + repo + "@" + branch
	if _, ok := f.branches[k]; ok {
		return "tip", false, nil
	}
	f.branches[k] = map[string]string{}
	return "tip", true, nil
}
func (f *fakeGH) CommitFiles(_ context.Context, owner, repo, branch, _ string, files []gh.FileChange) (string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := owner + "/" + repo + "@" + branch
	target := f.branches[k]
	if branch == "main" {
		target = f.files[owner+"/"+repo]
	} else if target == nil {
		return "", false, errors.New("no branch")
	}
	changed := false
	for _, fc := range files {
		cur := target[fc.Path]
		if cur == "" && branch != "main" {
			cur = f.files[owner+"/"+repo][fc.Path]
		}
		if cur != string(fc.Content) {
			target[fc.Path] = string(fc.Content)
			changed = true
		}
	}
	if changed {
		f.commits++
	}
	return "c", changed, nil
}
func (f *fakeGH) OpenPR(_ context.Context, owner, repo, head, _, _, _ string) (gh.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := owner + "/" + repo + "@" + head
	if p, ok := f.prs[k]; ok {
		return p, nil
	}
	p := gh.PR{Number: len(f.prs) + 1, URL: "https://x/" + k, Created: true}
	f.prs[k] = p
	return p, nil
}
func (f *fakeGH) DeleteRunner(_ context.Context, t core.Target, id int64) error {
	if f.busyDel[id] {
		return gh.ErrRunnerBusy
	}
	f.deleted = append(f.deleted, id)
	rs := f.runners[t.String()]
	for i, r := range rs {
		if r.ID == id {
			f.runners[t.String()] = append(rs[:i:i], rs[i+1:]...)
			break
		}
	}
	return nil
}
func (f *fakeGH) CreateRegistrationToken(context.Context, core.Target) (string, error) {
	f.tokens++
	return "REGTOKEN", nil
}
