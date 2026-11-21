package migrate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
)

// ScanInput configures Scan.
type ScanInput struct {
	Targets   []core.Target
	Rules     Rules // Prefix required
	HostRoots []string
	Root      string // filesystem root for host scanning; default "/"
	// TrustedUID owns the unit files and runner dirs we trust (default 0 = root).
	TrustedUID int
	GH         GitHub
	Log        *slog.Logger
	// Concurrency bounds parallel repo scans (default 4).
	Concurrency int
	Now         func() time.Time
}

// Scan reads everything the migration needs without changing anything:
// registered runners, legacy local runners, and every runs-on of every
// workflow on each repo's default branch.
func Scan(ctx context.Context, in ScanInput) (*Plan, error) {
	if in.Rules.Prefix == "" {
		return nil, fmt.Errorf("scan: prefix is required")
	}
	log := in.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	now := time.Now
	if in.Now != nil {
		now = in.Now
	}
	p := &Plan{Prefix: in.Rules.Prefix, Rules: in.Rules, CreatedAt: now().UTC()}

	repos := map[string]gh.Repo{}
	for _, t := range in.Targets {
		ts := TargetScan{Target: t}
		rs, err := in.GH.ListRunners(ctx, t)
		if err != nil {
			ts.Error = err.Error()
			log.Warn("list runners failed", "target", t.String(), "error", err.Error())
		}
		ts.Runners = rs
		p.Targets = append(p.Targets, ts)

		if t.IsOrg() {
			list, err := in.GH.ListOrgRepos(ctx, t.Owner)
			if err != nil {
				return nil, fmt.Errorf("list repos of %s: %w", t.Owner, err)
			}
			for _, r := range list {
				if !r.Archived {
					repos[r.Owner+"/"+r.Name] = r
				}
			}
			continue
		}
		db, err := in.GH.DefaultBranch(ctx, t.Owner, t.Repo)
		if err != nil {
			return nil, fmt.Errorf("default branch of %s: %w", t, err)
		}
		repos[t.String()] = gh.Repo{Owner: t.Owner, Name: t.Repo, DefaultBranch: db}
	}

	local, err := ScanLocal(in.Root, in.HostRoots, in.TrustedUID)
	if err != nil {
		return nil, err
	}
	p.Local = enrichLocal(local, p.Targets)

	keys := make([]string, 0, len(repos))
	for k := range repos {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	p.Repos = make([]RepoScan, len(keys))
	conc := in.Concurrency
	if conc <= 0 {
		conc = 4
	}
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, k := range keys {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			p.Repos[i] = scanRepo(ctx, in, repos[k])
		}()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.Vars = collectVars(ctx, in, p.Repos)
	return p, nil
}

func scanRepo(ctx context.Context, in ScanInput, r gh.Repo) RepoScan {
	rs := RepoScan{Owner: r.Owner, Repo: r.Name, DefaultBranch: r.DefaultBranch}
	files, err := in.GH.ListWorkflowFiles(ctx, r.Owner, r.Name, r.DefaultBranch)
	if err != nil {
		rs.Error = err.Error()
		return rs
	}
	for _, f := range files {
		src, sha, err := in.GH.GetFile(ctx, r.Owner, r.Name, f.Path, r.DefaultBranch)
		if err != nil {
			rs.Error = err.Error()
			continue
		}
		res := Analyze(src, in.Rules)
		rs.Files = append(rs.Files, FileScan{Path: f.Path, SHA: sha, Changes: res.Changes, Skipped: res.Skipped, Untouched: res.Untouched})
	}
	return rs
}

// enrichLocal fills labels from the registered runner of the same name when
// the host has no label file.
func enrichLocal(local []LocalRunner, targets []TargetScan) []LocalRunner {
	byName := map[string]gh.Runner{}
	for _, t := range targets {
		for _, r := range t.Runners {
			byName[r.Name] = r
		}
	}
	for i := range local {
		if len(local[i].Labels) == 0 {
			if r, ok := byName[local[i].Name]; ok {
				local[i].Labels = r.Labels
			}
		}
	}
	return local
}

// collectVars resolves every vars.* reference reported by the scan and
// proposes a new value when it holds legacy labels.
func collectVars(ctx context.Context, in ScanInput, repos []RepoScan) []VarUpdate {
	type key struct {
		scope core.Target
		name  string
	}
	idx := map[key]int{}
	var out []VarUpdate
	for _, r := range repos {
		for _, f := range r.Files {
			for _, s := range f.Skipped {
				if s.Reason != ReasonVariable {
					continue
				}
				v, found, err := in.GH.FindVariable(ctx, r.Owner, r.Repo, s.Var)
				scope := core.Target{Owner: r.Owner, Repo: r.Repo}
				if found {
					scope = v.Scope
				}
				k := key{scope, s.Var}
				i, ok := idx[k]
				if !ok {
					u := VarUpdate{Name: s.Var, Scope: scope}
					switch {
					case err != nil || !found:
						u.Reason = ReasonVarNotFound
					default:
						u.Old = v.Value
						if nv, ok := RewriteVarValue(v.Value, in.Rules); ok {
							u.New = nv
						} else {
							u.Reason = ReasonVarNotLabels
						}
					}
					out = append(out, u)
					i = len(out) - 1
					idx[k] = i
				}
				out[i].UsedBy = append(out[i].UsedBy, VarUse{Owner: r.Owner, Repo: r.Repo, File: f.Path, Line: s.Line})
			}
		}
	}
	return out
}
