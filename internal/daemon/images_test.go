package daemon

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

type fileAPI struct {
	*fakeAPI
	body  string
	err   error
	calls int
	refs  []string
}

func (f *fileAPI) GetFile(_ context.Context, _, _, _, ref string) ([]byte, string, error) {
	f.calls++
	f.refs = append(f.refs, ref)
	return []byte(f.body), "", f.err
}

func imageDaemon(api *fileAPI, allowed ...string) *Daemon {
	cfg := &config.Config{}
	cfg.Linux.AllowedImages = allowed
	return &Daemon{cfg: cfg, log: slog.New(slog.DiscardHandler), rest: api}
}

func grantFor(job string) scheduler.Grant {
	return scheduler.Grant{Job: core.Job{Owner: "Acme", Repo: "app", Name: job,
		WorkflowRef: "acme/app/.github/workflows/ci.yml@refs/pull/7/merge"}}
}

const repoFile = "image: ghcr.io/acme/ci:1\njobs:\n  - match: ci.yml/android*\n    image: ghcr.io/acme/android:1\n  - match: ci.yml/evil\n    image: docker.io/evil/x:1\n"

func TestRunnerImageFromRepo(t *testing.T) {
	api := &fileAPI{fakeAPI: &fakeAPI{}, body: repoFile}
	d := imageDaemon(api, "ghcr.io/acme/*")
	ctx := context.Background()
	if got := d.runnerImage(ctx, grantFor("android (1/2)")); got != "ghcr.io/acme/android:1" {
		t.Fatalf("android: %q", got)
	}
	if got := d.runnerImage(ctx, grantFor("test")); got != "ghcr.io/acme/ci:1" {
		t.Fatalf("default: %q", got)
	}
	if got := d.runnerImage(ctx, grantFor("evil")); got != "" {
		t.Fatalf("image outside allowlist used: %q", got)
	}
	if api.calls != 1 || api.refs[0] != "refs/pull/7/merge" {
		t.Fatalf("want one cached read at the workflow ref, got %d %v", api.calls, api.refs)
	}
}

func TestRunnerImageRuleWinsAndAllowlistGates(t *testing.T) {
	api := &fileAPI{fakeAPI: &fakeAPI{}, body: repoFile}
	g := grantFor("test")
	g.Image = "host/rule:1"
	if got := imageDaemon(api).runnerImage(context.Background(), g); got != "host/rule:1" {
		t.Fatalf("rule: %q", got)
	}
	if got := imageDaemon(api).runnerImage(context.Background(), grantFor("test")); got != "" || api.calls != 0 {
		t.Fatalf("empty allowlist must not read the repo: %q calls=%d", got, api.calls)
	}
}

func TestRunnerImageMissingOrBrokenFile(t *testing.T) {
	ctx := context.Background()
	missing := &fileAPI{fakeAPI: &fakeAPI{}, err: gh.ErrNotFound}
	d := imageDaemon(missing, "*")
	if d.runnerImage(ctx, grantFor("a")) != "" || d.runnerImage(ctx, grantFor("b")) != "" || missing.calls != 1 {
		t.Fatalf("404 should be cached, calls=%d", missing.calls)
	}
	flaky := &fileAPI{fakeAPI: &fakeAPI{}, err: errors.New("boom")}
	d = imageDaemon(flaky, "*")
	_, _ = d.runnerImage(ctx, grantFor("a")), d.runnerImage(ctx, grantFor("a"))
	if flaky.calls != 2 {
		t.Fatalf("transient errors must not be cached, calls=%d", flaky.calls)
	}
	bad := &fileAPI{fakeAPI: &fakeAPI{}, body: "imgae: x\n"}
	if imageDaemon(bad, "*").runnerImage(ctx, grantFor("a")) != "" {
		t.Fatal("invalid file must fall back to default")
	}
}

func TestWorkflowRefOnlyForOwnRepo(t *testing.T) {
	j := core.Job{Owner: "acme", Repo: "app", WorkflowRef: "acme/shared/.github/workflows/x.yml@refs/heads/main"}
	if workflowRef(j) != "" {
		t.Fatal("reusable workflow from another repo must read the default branch")
	}
}
