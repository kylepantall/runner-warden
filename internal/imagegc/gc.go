// Package imagegc is the only path that prunes Docker images and build cache.
// It never forces a removal, never touches an image any container (ours or
// foreign) uses, and holds image pulls while it runs so a pull never loses
// layers to a concurrent removal.
package imagegc

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

// PullGate is implemented by every proxy (and by an aggregate of them).
type PullGate interface {
	Pause()
	Resume()
	InFlight() int
}

// Docker is the subset of the Docker client the collector needs.
type Docker interface {
	Info(ctx context.Context) (system.Info, error)
	ContainerList(ctx context.Context, opts container.ListOptions) ([]container.Summary, error)
	ImageList(ctx context.Context, opts image.ListOptions) ([]image.Summary, error)
	ImageInspect(ctx context.Context, id string, opts ...client.ImageInspectOption) (image.InspectResponse, error)
	ImageRemove(ctx context.Context, id string, opts image.RemoveOptions) ([]image.DeleteResponse, error)
	BuildCachePrune(ctx context.Context, opts build.CachePruneOptions) (*build.CachePruneReport, error)
}

// Config configures a Collector.
type Config struct {
	Docker        Docker
	Gates         []PullGate
	HighWatermark int           // percent of the data-root filesystem; 0 disables the disk trigger
	Interval      time.Duration // also run at least this often; 0 disables the timer trigger
	UnusedFor     time.Duration // minimum age since an image was last tagged/pulled
	CheckEvery    time.Duration // Loop polling period; 0 = 1m
	DrainTimeout  time.Duration // max wait for in-flight pulls; 0 = 2m
	Protect       []string      // image refs/IDs never removed (e.g. the runner image)
	Log           *slog.Logger  // nil = discard

	// Overridable for tests.
	DiskUsedPercent func(path string) (int, error) // nil = statfs
	Now             func() time.Time               // nil = time.Now
	PollEvery       time.Duration                  // drain poll period; 0 = 250ms
}

// Result summarises one pass.
type Result struct {
	Removed        []string // image refs/IDs removed
	ImageBytes     int64    // sum of removed image sizes
	CacheDeleted   int
	CacheBytes     uint64
	AbortedPulls   bool // a pull was in flight; pass stopped early
	DrainTimedOut  bool
	DiskPercentWas int
}

// Collector runs the GC.
type Collector struct {
	cfg Config
	log *slog.Logger
}

// New validates and builds a Collector.
func New(cfg Config) (*Collector, error) {
	if cfg.Docker == nil {
		return nil, errors.New("imagegc: Docker is required")
	}
	if cfg.CheckEvery <= 0 {
		cfg.CheckEvery = time.Minute
	}
	if cfg.DrainTimeout <= 0 {
		cfg.DrainTimeout = 2 * time.Minute
	}
	if cfg.PollEvery <= 0 {
		cfg.PollEvery = 250 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.DiskUsedPercent == nil {
		cfg.DiskUsedPercent = diskUsedPercent
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Collector{cfg: cfg, log: log}, nil
}

// Loop checks every CheckEvery and runs a pass when disk usage is at or above
// the watermark, or Interval has elapsed since the last pass. It returns when
// ctx is done.
func (c *Collector) Loop(ctx context.Context) {
	last := c.cfg.Now()
	t := time.NewTicker(c.cfg.CheckEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		reason := c.due(ctx, last)
		if reason == "" {
			continue
		}
		res, err := c.Run(ctx)
		last = c.cfg.Now()
		if err != nil {
			c.log.Warn("image gc failed", "reason", reason, "error", err.Error())
			continue
		}
		c.log.Info("image gc finished", "reason", reason, "removed", len(res.Removed),
			"image_bytes", res.ImageBytes, "cache_bytes", res.CacheBytes, "aborted", res.AbortedPulls)
	}
}

func (c *Collector) due(ctx context.Context, last time.Time) string {
	if c.cfg.HighWatermark > 0 {
		if pct, err := c.diskPercent(ctx); err == nil && pct >= c.cfg.HighWatermark {
			return "watermark"
		}
	}
	if c.cfg.Interval > 0 && c.cfg.Now().Sub(last) >= c.cfg.Interval {
		return "interval"
	}
	return ""
}

func (c *Collector) diskPercent(ctx context.Context) (int, error) {
	info, err := c.cfg.Docker.Info(ctx)
	if err != nil {
		return 0, err
	}
	return c.cfg.DiskUsedPercent(info.DockerRootDir)
}

// Run executes one GC pass now.
func (c *Collector) Run(ctx context.Context) (Result, error) {
	var res Result
	if pct, err := c.diskPercent(ctx); err == nil {
		res.DiskPercentWas = pct
	}

	for _, g := range c.cfg.Gates {
		g.Pause()
	}
	defer func() {
		for _, g := range c.cfg.Gates {
			g.Resume()
		}
	}()
	// New pulls are held; wait for those already running to finish.
	if !c.drain(ctx) {
		res.DrainTimedOut = true
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		c.log.Warn("image gc skipped: pulls still in flight")
		return res, nil
	}

	cands, err := c.candidates(ctx)
	if err != nil {
		return res, err
	}
	for _, cand := range cands {
		if c.inFlight() > 0 {
			res.AbortedPulls = true
			break
		}
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		removed := false
		for _, ref := range cand.refs {
			_, err := c.cfg.Docker.ImageRemove(ctx, ref, image.RemoveOptions{Force: false, PruneChildren: true})
			if err != nil {
				// In use, or a race with a new container: leave it.
				c.log.Info("image gc kept image", "image", ref, "error", err.Error())
				continue
			}
			removed = true
			res.Removed = append(res.Removed, ref)
		}
		if removed {
			res.ImageBytes += cand.size
		}
	}

	if !res.AbortedPulls && c.cfg.UnusedFor > 0 && c.inFlight() == 0 {
		f := filters.NewArgs(filters.Arg("until", c.cfg.UnusedFor.String()))
		rep, err := c.cfg.Docker.BuildCachePrune(ctx, build.CachePruneOptions{Filters: f})
		if err != nil {
			c.log.Warn("image gc build cache prune failed", "error", err.Error())
		} else if rep != nil {
			res.CacheDeleted, res.CacheBytes = len(rep.CachesDeleted), rep.SpaceReclaimed
		}
	}
	return res, nil
}

func (c *Collector) inFlight() int {
	n := 0
	for _, g := range c.cfg.Gates {
		n += g.InFlight()
	}
	return n
}

func (c *Collector) drain(ctx context.Context) bool {
	deadline := time.NewTimer(c.cfg.DrainTimeout)
	defer deadline.Stop()
	tick := time.NewTicker(c.cfg.PollEvery)
	defer tick.Stop()
	for c.inFlight() > 0 {
		select {
		case <-ctx.Done():
			return false
		case <-deadline.C:
			return false
		case <-tick.C:
		}
	}
	return true
}

type candidate struct {
	refs []string
	size int64
	used time.Time
}

// candidates lists images unused by any container and untouched for UnusedFor,
// oldest first. Each image is removed by tag so multi-tag images need no force.
func (c *Collector) candidates(ctx context.Context) ([]candidate, error) {
	conts, err := c.cfg.Docker.ContainerList(ctx, container.ListOptions{All: true})
	if err != nil {
		return nil, err
	}
	inUse := map[string]bool{}
	for _, ct := range conts {
		inUse[ct.ImageID] = true
	}
	protect := map[string]bool{}
	for _, p := range c.cfg.Protect {
		protect[p] = true
	}
	imgs, err := c.cfg.Docker.ImageList(ctx, image.ListOptions{All: false})
	if err != nil {
		return nil, err
	}
	cutoff := c.cfg.Now().Add(-c.cfg.UnusedFor)
	var out []candidate
	for _, im := range imgs {
		if inUse[im.ID] || im.Containers > 0 || protect[im.ID] || anyIn(protect, im.RepoTags) {
			continue
		}
		used := time.Unix(im.Created, 0)
		if insp, err := c.cfg.Docker.ImageInspect(ctx, im.ID); err == nil && !insp.Metadata.LastTagTime.IsZero() {
			used = insp.Metadata.LastTagTime
		}
		if used.After(cutoff) {
			continue
		}
		refs := make([]string, 0, len(im.RepoTags))
		for _, t := range im.RepoTags {
			if t != "<none>:<none>" {
				refs = append(refs, t)
			}
		}
		if len(refs) == 0 {
			refs = []string{im.ID}
		}
		out = append(out, candidate{refs: refs, size: im.Size, used: used})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].used.Before(out[j].used) })
	return out, nil
}

func anyIn(set map[string]bool, xs []string) bool {
	for _, x := range xs {
		if set[x] {
			return true
		}
	}
	return false
}
