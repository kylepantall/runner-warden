package gh

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/go-github/v93/github"

	"github.com/kylepantall/runner-warden/internal/core"
)

type srv struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies map[string]string
}

func newSrv(t *testing.T, routes map[string]http.HandlerFunc) (*REST, *srv) {
	t.Helper()
	s := &srv{bodies: map[string]string{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.calls = append(s.calls, key)
		s.bodies[key] = string(b)
		s.mu.Unlock()
		h, ok := routes[key]
		if !ok {
			http.Error(w, `{"message":"no route"}`, http.StatusNotFound)
			return
		}
		h(w, r)
	}))
	t.Cleanup(s.Close)
	u := s.URL + "/"
	api, err := NewREST("tok", github.WithURLs(&u, &u))
	if err != nil {
		t.Fatal(err)
	}
	return api, s
}

func js(v any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
}

func status204(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }
func code(c int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(c)
		_, _ = w.Write([]byte(`{"message":"x"}`))
	}
}

func (s *srv) called(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.calls {
		if c == key {
			return true
		}
	}
	return false
}

var ctx = context.Background()

func TestListRunnersOrgPaginated(t *testing.T) {
	var s *srv
	var api *REST
	api, s = newSrv(t, map[string]http.HandlerFunc{
		"GET /orgs/acme/actions/runners": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("page") == "2" {
				js(map[string]any{"total_count": 2, "runners": []map[string]any{{"id": 2, "name": "b", "status": "offline"}}})(w, r)
				return
			}
			w.Header().Set("Link", `<`+s.URL+`/orgs/acme/actions/runners?page=2>; rel="next"`)
			js(map[string]any{"total_count": 2, "runners": []map[string]any{{"id": 1, "name": "a", "status": "online", "busy": true,
				"labels": []map[string]any{{"name": "self-hosted"}, {"name": "x"}}}}})(w, r)
		},
	})
	rs, err := api.ListRunners(ctx, core.Target{Owner: "acme"})
	if err != nil || len(rs) != 2 {
		t.Fatalf("%v %v", rs, err)
	}
	if !rs[0].Busy || rs[0].Status != "online" || len(rs[0].Labels) != 2 || rs[1].ID != 2 {
		t.Fatalf("%+v", rs)
	}
}

func TestDeleteRunnerBusyAndMissing(t *testing.T) {
	api, _ := newSrv(t, map[string]http.HandlerFunc{
		"DELETE /repos/o/r/actions/runners/1": code(http.StatusUnprocessableEntity),
		"DELETE /repos/o/r/actions/runners/2": code(http.StatusNotFound),
		"DELETE /orgs/o/actions/runners/3":    status204,
	})
	if err := api.DeleteRunner(ctx, core.Target{Owner: "o", Repo: "r"}, 1); !errors.Is(err, ErrRunnerBusy) {
		t.Fatalf("want busy, got %v", err)
	}
	if err := api.DeleteRunner(ctx, core.Target{Owner: "o", Repo: "r"}, 2); err != nil {
		t.Fatalf("missing should be nil: %v", err)
	}
	if err := api.DeleteRunner(ctx, core.Target{Owner: "o"}, 3); err != nil {
		t.Fatal(err)
	}
}

func TestTokenScopes(t *testing.T) {
	api, _ := newSrv(t, map[string]http.HandlerFunc{
		"GET /user": func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-OAuth-Scopes", "repo, read:org, workflow")
			js(map[string]any{})(w, r)
		},
	})
	got, err := api.TokenScopes(ctx)
	if err != nil || len(got) != 3 || got[1] != "read:org" {
		t.Fatalf("%v %v", got, err)
	}
	api2, _ := newSrv(t, map[string]http.HandlerFunc{"GET /user": js(map[string]any{})})
	got, err = api2.TokenScopes(ctx)
	if err != nil || len(got) != 1 || got[0] != ScopesUnknown {
		t.Fatalf("%v %v", got, err)
	}
}

func TestRerunJobLooksUpByName(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/actions/runs/9/jobs": js(map[string]any{"total_count": 2, "jobs": []map[string]any{
			{"id": 100, "name": "build"}, {"id": 101, "name": "test (1)"}}}),
		"POST /repos/o/r/actions/jobs/101/rerun": js(map[string]any{}),
	})
	if err := api.RerunJob(ctx, "o", "r", 9, "test (1)"); err != nil {
		t.Fatal(err)
	}
	if !s.called("POST /repos/o/r/actions/jobs/101/rerun") {
		t.Fatal("rerun not called for matching job id")
	}
	if err := api.RerunJob(ctx, "o", "r", 9, "nope"); !errors.Is(err, ErrJobNotFound) {
		t.Fatalf("want ErrJobNotFound, got %v", err)
	}
}

func TestRunCancelAndPR(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/actions/runs/9": js(map[string]any{"id": 9, "event": "pull_request", "head_branch": "f", "head_sha": "abc",
			"status": "queued", "pull_requests": []map[string]any{{"number": 12}}}),
		"POST /repos/o/r/actions/runs/9/cancel":       func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) },
		"POST /repos/o/r/actions/runs/9/force-cancel": func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) },
		"GET /repos/o/r/pulls/12":                     js(map[string]any{"state": "closed", "merged": true}),
		"GET /repos/o/r/branches/gone":                code(http.StatusNotFound),
		"GET /repos/o/r/branches/here":                js(map[string]any{"name": "here"}),
	})
	run, err := api.GetRun(ctx, "o", "r", 9)
	if err != nil || run.Event != "pull_request" || len(run.PRs) != 1 || run.PRs[0] != 12 || run.HeadBranch != "f" {
		t.Fatalf("%+v %v", run, err)
	}
	if err := api.CancelRun(ctx, "o", "r", 9); err != nil {
		t.Fatal(err)
	}
	if err := api.ForceCancelRun(ctx, "o", "r", 9); err != nil || !s.called("POST /repos/o/r/actions/runs/9/force-cancel") {
		t.Fatal(err)
	}
	if p, err := api.GetPRState(ctx, "o", "r", 12); err != nil || !p.Merged || !p.Closed {
		t.Fatalf("%+v %v", p, err)
	}
	if ok, err := api.BranchExists(ctx, "o", "r", "gone"); err != nil || ok {
		t.Fatal(ok, err)
	}
	if ok, err := api.BranchExists(ctx, "o", "r", "here"); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestCommitFilesSingleCommit(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/git/ref/heads/b":    js(map[string]any{"object": map[string]any{"sha": "tip"}}),
		"GET /repos/o/r/git/commits/tip":    js(map[string]any{"sha": "tip", "tree": map[string]any{"sha": "tree0"}}),
		"GET /repos/o/r/contents/a.yml":     js(map[string]any{"type": "file", "path": "a.yml", "sha": "s1", "encoding": "base64", "content": "b2xk"}),
		"GET /repos/o/r/contents/new.yml":   code(http.StatusNotFound),
		"POST /repos/o/r/git/trees":         js(map[string]any{"sha": "tree1"}),
		"POST /repos/o/r/git/commits":       js(map[string]any{"sha": "c1"}),
		"PATCH /repos/o/r/git/refs/heads/b": js(map[string]any{"object": map[string]any{"sha": "c1"}}),
	})
	sha, changed, err := api.CommitFiles(ctx, "o", "r", "b", "msg", []FileChange{
		{Path: "a.yml", Content: []byte("old")}, // identical to tip: skipped
		{Path: "new.yml", Content: []byte("x")},
	})
	if err != nil || !changed || sha != "c1" {
		t.Fatal(sha, changed, err)
	}
	var tree struct {
		BaseTree string                  `json:"base_tree"`
		Tree     []struct{ Path string } `json:"tree"`
	}
	if err := json.Unmarshal([]byte(s.bodies["POST /repos/o/r/git/trees"]), &tree); err != nil {
		t.Fatal(err)
	}
	if tree.BaseTree != "tree0" || len(tree.Tree) != 1 || tree.Tree[0].Path != "new.yml" {
		t.Fatalf("%+v", tree)
	}
	// No-op when everything matches.
	sha, changed, err = api.CommitFiles(ctx, "o", "r", "b", "msg", []FileChange{{Path: "a.yml", Content: []byte("old")}})
	if err != nil || changed || sha != "tip" {
		t.Fatal(sha, changed, err)
	}
}

func TestCreateBranchAndOpenPRIdempotent(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/git/ref/heads/exists": js(map[string]any{"object": map[string]any{"sha": "e1"}}),
		"GET /repos/o/r/git/ref/heads/main":   js(map[string]any{"object": map[string]any{"sha": "m1"}}),
		"POST /repos/o/r/git/refs":            js(map[string]any{"ref": "refs/heads/new"}),
		"GET /repos/o/r/pulls":                js([]map[string]any{{"number": 3, "html_url": "u3"}}),
		"GET /repos/o/p/pulls":                js([]map[string]any{}),
		"POST /repos/o/p/pulls":               js(map[string]any{"number": 4, "html_url": "u4"}),
	})
	if sha, created, err := api.CreateBranch(ctx, "o", "r", "exists", "main"); err != nil || created || sha != "e1" {
		t.Fatal(sha, created, err)
	}
	if sha, created, err := api.CreateBranch(ctx, "o", "r", "new", "main"); err != nil || !created || sha != "m1" || !s.called("POST /repos/o/r/git/refs") {
		t.Fatal(sha, created, err)
	}
	if pr, err := api.OpenPR(ctx, "o", "r", "h", "main", "t", "b"); err != nil || pr.Created || pr.Number != 3 {
		t.Fatalf("%+v %v", pr, err)
	}
	if pr, err := api.OpenPR(ctx, "o", "p", "h", "main", "t", "b"); err != nil || !pr.Created || pr.Number != 4 {
		t.Fatalf("%+v %v", pr, err)
	}
}

func TestVariables(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/actions/variables/CI":   code(http.StatusNotFound),
		"GET /orgs/o/actions/variables/CI":      js(map[string]any{"name": "CI", "value": `["self-hosted"]`}),
		"GET /repos/o/p/actions/variables/CI":   js(map[string]any{"name": "CI", "value": "v"}),
		"PATCH /orgs/o/actions/variables/CI":    status204,
		"PATCH /repos/o/p/actions/variables/CI": status204,
	})
	v, ok, err := api.FindVariable(ctx, "o", "r", "CI")
	if err != nil || !ok || !v.Scope.IsOrg() {
		t.Fatalf("%+v %v %v", v, ok, err)
	}
	v2, ok, err := api.FindVariable(ctx, "o", "p", "CI")
	if err != nil || !ok || v2.Scope.IsOrg() || v2.Scope.Repo != "p" {
		t.Fatalf("%+v %v %v", v2, ok, err)
	}
	if _, ok, _ := api.FindVariable(ctx, "o", "r", "MISSING"); ok {
		t.Fatal("missing var found")
	}
	if err := api.UpdateVariable(ctx, v.Scope, "CI", "new"); err != nil || !s.called("PATCH /orgs/o/actions/variables/CI") {
		t.Fatal(err)
	}
	if err := api.UpdateVariable(ctx, v2.Scope, "CI", "new"); err != nil || !s.called("PATCH /repos/o/p/actions/variables/CI") {
		t.Fatal(err)
	}
}

func TestListWorkflowFilesAndGetFile(t *testing.T) {
	api, _ := newSrv(t, map[string]http.HandlerFunc{
		"GET /repos/o/r/contents/.github/workflows": js([]map[string]any{
			{"type": "file", "name": "ci.yml", "path": ".github/workflows/ci.yml", "sha": "1"},
			{"type": "file", "name": "README.md", "path": ".github/workflows/README.md", "sha": "2"},
			{"type": "file", "name": "b.yaml", "path": ".github/workflows/b.yaml", "sha": "3"},
		}),
		"GET /repos/o/p/contents/.github/workflows": code(http.StatusNotFound),
		"GET /repos/o/r/contents/f.txt":             js(map[string]any{"type": "file", "path": "f.txt", "sha": "9", "encoding": "base64", "content": "aGk="}),
	})
	fs, err := api.ListWorkflowFiles(ctx, "o", "r", "main")
	if err != nil || len(fs) != 2 || fs[0].SHA != "1" || fs[1].Path != ".github/workflows/b.yaml" {
		t.Fatalf("%+v %v", fs, err)
	}
	if fs, err = api.ListWorkflowFiles(ctx, "o", "p", "main"); err != nil || len(fs) != 0 {
		t.Fatalf("%+v %v", fs, err)
	}
	b, sha, err := api.GetFile(ctx, "o", "r", "f.txt", "main")
	if err != nil || string(b) != "hi" || sha != "9" {
		t.Fatal(string(b), sha, err)
	}
}

func TestOrgListsAndDispatch(t *testing.T) {
	api, s := newSrv(t, map[string]http.HandlerFunc{
		"GET /orgs/o/repos": js([]map[string]any{{"name": "a", "default_branch": "main"}, {"name": "old", "archived": true, "default_branch": "master"}}),
		"GET /orgs/o/actions/runner-groups": js(map[string]any{"total_count": 2, "runner_groups": []map[string]any{
			{"id": 1, "name": "Default", "default": true}, {"id": 5, "name": "ci"}}}),
		"GET /repos/o/a": js(map[string]any{"default_branch": "trunk"}),
		"POST /repos/o/a/actions/workflows/smoke.yml/dispatches": status204,
		"POST /orgs/o/actions/runners/registration-token":        js(map[string]any{"token": "rt"}),
	})
	repos, err := api.ListOrgRepos(ctx, "o")
	if err != nil || len(repos) != 2 || !repos[1].Archived || repos[0].DefaultBranch != "main" {
		t.Fatalf("%+v %v", repos, err)
	}
	gs, err := api.ListOrgRunnerGroups(ctx, "o")
	if err != nil || len(gs) != 2 || !gs[0].Default || gs[1].ID != 5 {
		t.Fatalf("%+v %v", gs, err)
	}
	if b, err := api.DefaultBranch(ctx, "o", "a"); err != nil || b != "trunk" {
		t.Fatal(b, err)
	}
	if err := api.DispatchWorkflow(ctx, "o", "a", "smoke.yml", "main", nil); err != nil || !s.called("POST /repos/o/a/actions/workflows/smoke.yml/dispatches") {
		t.Fatal(err)
	}
	if tok, err := api.CreateRegistrationToken(ctx, core.Target{Owner: "o"}); err != nil || tok != "rt" {
		t.Fatal(tok, err)
	}
}
