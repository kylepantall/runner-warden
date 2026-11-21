package daemon

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/repoconf"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

// imageEnsurer is implemented by backends that can run a per-job image: it
// makes the image available locally (pulling when needed).
type imageEnsurer interface {
	EnsureImage(ctx context.Context, ref string) error
}

// fileGetter reads a repo file at a ref (implemented by *gh.REST).
type fileGetter interface {
	GetFile(ctx context.Context, owner, repo, p, ref string) ([]byte, string, error)
}

const (
	repoConfTTL     = time.Minute
	repoConfTimeout = 10 * time.Second
)

type repoConfEntry struct {
	f  *repoconf.File // nil: no file, or an invalid one
	at time.Time
}

// repoConfs caches .github/runner-warden.yml per repo and ref.
type repoConfs struct {
	mu sync.Mutex
	m  map[string]repoConfEntry
}

// runnerImage picks the runner image for a grant: a [[cost]] rule image
// first, then the repo's .github/runner-warden.yml when the host allows the
// image. "" means the backend default.
func (d *Daemon) runnerImage(ctx context.Context, g scheduler.Grant) string {
	if g.Image != "" {
		return g.Image
	}
	j := g.Job
	if len(d.cfg.Linux.AllowedImages) == 0 || j.Owner == "" || j.Repo == "" {
		return ""
	}
	img := d.repoConf(ctx, j).ImageFor(admission.WorkflowFile(j.WorkflowRef), j.Name)
	if img == "" {
		return ""
	}
	if !repoconf.Allowed(img, d.cfg.Linux.AllowedImages) {
		d.log.Warn("repo image not in linux.allowed_images, using default", "repo", j.Owner+"/"+j.Repo, "image", img)
		return ""
	}
	return img
}

// workflowRef returns the git ref the job's workflow came from when the
// workflow lives in the job's own repo, else "" (the default branch).
func workflowRef(j core.Job) string {
	file, ref, ok := strings.Cut(j.WorkflowRef, "@")
	if !ok || !strings.HasPrefix(strings.ToLower(file), strings.ToLower(j.Owner+"/"+j.Repo+"/")) {
		return ""
	}
	return ref
}

func (d *Daemon) repoConf(ctx context.Context, j core.Job) *repoconf.File {
	fg, ok := d.rest.(fileGetter)
	if !ok {
		return nil
	}
	ref := workflowRef(j)
	key := strings.ToLower(j.Owner+"/"+j.Repo) + "@" + ref
	d.confs.mu.Lock()
	e, hit := d.confs.m[key]
	d.confs.mu.Unlock()
	if hit && time.Since(e.at) < repoConfTTL {
		return e.f
	}
	ctx, cancel := context.WithTimeout(ctx, repoConfTimeout)
	defer cancel()
	b, _, err := fg.GetFile(ctx, j.Owner, j.Repo, repoconf.Path, ref)
	var f *repoconf.File
	switch {
	case errors.Is(err, gh.ErrNotFound):
	case err != nil:
		// Not cached: the next job retries. Running on the default image
		// beats holding the job.
		d.log.Warn("read repo config failed, using default image", "repo", j.Owner+"/"+j.Repo, "error", err.Error())
		return nil
	default:
		if f, err = repoconf.Parse(b); err != nil {
			d.log.Warn("invalid repo config, using default image", "repo", j.Owner+"/"+j.Repo, "error", err.Error())
		}
	}
	d.confs.mu.Lock()
	if d.confs.m == nil {
		d.confs.m = map[string]repoConfEntry{}
	}
	d.confs.m[key] = repoConfEntry{f: f, at: time.Now()}
	d.confs.mu.Unlock()
	return f
}
