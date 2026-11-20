package imagegc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/system"
	"github.com/docker/docker/client"
)

var now = time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

type fakeDocker struct {
	mu         sync.Mutex
	conts      []container.Summary
	imgs       []image.Summary
	tagTime    map[string]time.Time
	removed    []string
	failRemove map[string]bool
	cachePrune int
	onRemove   func(ref string)
}

func (f *fakeDocker) Info(context.Context) (system.Info, error) {
	return system.Info{DockerRootDir: "/var/lib/docker"}, nil
}
func (f *fakeDocker) ContainerList(context.Context, container.ListOptions) ([]container.Summary, error) {
	return f.conts, nil
}
func (f *fakeDocker) ImageList(context.Context, image.ListOptions) ([]image.Summary, error) {
	return f.imgs, nil
}
func (f *fakeDocker) ImageInspect(_ context.Context, id string, _ ...client.ImageInspectOption) (image.InspectResponse, error) {
	var r image.InspectResponse
	r.Metadata.LastTagTime = f.tagTime[id]
	return r, nil
}
func (f *fakeDocker) ImageRemove(_ context.Context, ref string, o image.RemoveOptions) ([]image.DeleteResponse, error) {
	if o.Force {
		return nil, errors.New("force must never be set")
	}
	if f.onRemove != nil {
		f.onRemove(ref)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRemove[ref] {
		return nil, errors.New("in use")
	}
	f.removed = append(f.removed, ref)
	return nil, nil
}
func (f *fakeDocker) BuildCachePrune(_ context.Context, o build.CachePruneOptions) (*build.CachePruneReport, error) {
	f.cachePrune++
	if !o.Filters.Contains("until") {
		return nil, errors.New("until filter required")
	}
	return &build.CachePruneReport{CachesDeleted: []string{"a"}, SpaceReclaimed: 10}, nil
}

type fakeGate struct {
	mu       sync.Mutex
	paused   bool
	inflight int
	events   []string
	onPoll   func(*fakeGate)
}

func (g *fakeGate) Pause() {
	g.mu.Lock()
	g.paused = true
	g.events = append(g.events, "pause")
	g.mu.Unlock()
}
func (g *fakeGate) Resume() {
	g.mu.Lock()
	g.paused = false
	g.events = append(g.events, "resume")
	g.mu.Unlock()
}
func (g *fakeGate) InFlight() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.onPoll != nil {
		g.onPoll(g)
	}
	return g.inflight
}

func newGC(t *testing.T, d Docker, gates []PullGate, mod func(*Config)) *Collector {
	t.Helper()
	cfg := Config{
		Docker: d, Gates: gates, UnusedFor: 24 * time.Hour, HighWatermark: 85, Interval: 20 * time.Hour,
		Now: func() time.Time { return now }, PollEvery: time.Millisecond, DrainTimeout: 200 * time.Millisecond,
		DiskUsedPercent: func(string) (int, error) { return 90, nil },
	}
	if mod != nil {
		mod(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRunRemovesOnlyUnusedOldImages(t *testing.T) {
	old := now.Add(-48 * time.Hour)
	fresh := now.Add(-time.Hour)
	d := &fakeDocker{
		conts: []container.Summary{{ImageID: "sha-used"}},
		imgs: []image.Summary{
			{ID: "sha-used", RepoTags: []string{"used:1"}},
			{ID: "sha-old", RepoTags: []string{"old:1", "old:2"}, Size: 100},
			{ID: "sha-fresh", RepoTags: []string{"fresh:1"}},
			{ID: "sha-runner", RepoTags: []string{"runner:latest"}},
			{ID: "sha-dangling"},
		},
		tagTime: map[string]time.Time{"sha-used": old, "sha-old": old, "sha-fresh": fresh, "sha-runner": old, "sha-dangling": old},
	}
	g := &fakeGate{}
	c := newGC(t, d, []PullGate{g}, func(c *Config) { c.Protect = []string{"runner:latest"} })
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range d.removed {
		got[r] = true
	}
	for _, want := range []string{"old:1", "old:2", "sha-dangling"} {
		if !got[want] {
			t.Errorf("expected %s removed", want)
		}
	}
	if len(got) != 3 {
		t.Errorf("removed %v", d.removed)
	}
	if res.ImageBytes != 100 || d.cachePrune != 1 || res.CacheDeleted != 1 {
		t.Errorf("result %+v", res)
	}
	if len(g.events) != 2 || g.events[0] != "pause" || g.events[1] != "resume" {
		t.Errorf("gate events %v", g.events)
	}
}

func TestRunWaitsForPullsToDrain(t *testing.T) {
	d := &fakeDocker{
		imgs:    []image.Summary{{ID: "a", RepoTags: []string{"a:1"}}},
		tagTime: map[string]time.Time{"a": now.Add(-48 * time.Hour)},
	}
	g := &fakeGate{inflight: 1}
	polls := 0
	g.onPoll = func(g *fakeGate) {
		polls++
		if polls > 3 {
			g.inflight = 0
		}
	}
	c := newGC(t, d, []PullGate{g}, nil)
	res, err := c.Run(context.Background())
	if err != nil || res.DrainTimedOut || len(d.removed) != 1 {
		t.Fatalf("res %+v err %v removed %v", res, err, d.removed)
	}
	if g.events[0] != "pause" {
		t.Error("must pause before waiting")
	}
}

func TestRunSkipsWhenPullNeverDrains(t *testing.T) {
	d := &fakeDocker{imgs: []image.Summary{{ID: "a", RepoTags: []string{"a:1"}}}, tagTime: map[string]time.Time{"a": now.Add(-48 * time.Hour)}}
	g := &fakeGate{inflight: 1}
	c := newGC(t, d, []PullGate{g}, nil)
	res, err := c.Run(context.Background())
	if err != nil || !res.DrainTimedOut || len(d.removed) != 0 || d.cachePrune != 0 {
		t.Fatalf("res %+v err %v", res, err)
	}
	if g.paused {
		t.Error("gate left paused")
	}
}

func TestRecheckBeforeEachRemove(t *testing.T) {
	old := now.Add(-48 * time.Hour)
	d := &fakeDocker{
		imgs:    []image.Summary{{ID: "a", RepoTags: []string{"a:1"}}, {ID: "b", RepoTags: []string{"b:1"}}},
		tagTime: map[string]time.Time{"a": old, "b": old.Add(time.Hour)},
	}
	g := &fakeGate{}
	d.onRemove = func(string) { g.mu.Lock(); g.inflight = 1; g.mu.Unlock() } // a pull appears after the first rmi
	c := newGC(t, d, []PullGate{g}, nil)
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(d.removed) != 1 || !res.AbortedPulls || d.cachePrune != 0 {
		t.Fatalf("removed %v res %+v", d.removed, res)
	}
}

func TestInUseRemovalErrorIsTolerated(t *testing.T) {
	old := now.Add(-48 * time.Hour)
	d := &fakeDocker{
		imgs:       []image.Summary{{ID: "a", RepoTags: []string{"a:1"}}, {ID: "b", RepoTags: []string{"b:1"}}},
		tagTime:    map[string]time.Time{"a": old, "b": old},
		failRemove: map[string]bool{"a:1": true},
	}
	c := newGC(t, d, nil, nil)
	res, err := c.Run(context.Background())
	if err != nil || len(d.removed) != 1 || d.removed[0] != "b:1" || len(res.Removed) != 1 {
		t.Fatalf("res %+v err %v removed %v", res, err, d.removed)
	}
}

func TestDueTriggers(t *testing.T) {
	d := &fakeDocker{}
	pct := 50
	c := newGC(t, d, nil, func(c *Config) { c.DiskUsedPercent = func(string) (int, error) { return pct, nil } })
	ctx := context.Background()
	if r := c.due(ctx, now); r != "" {
		t.Errorf("unexpected trigger %q", r)
	}
	if r := c.due(ctx, now.Add(-21*time.Hour)); r == "" {
		t.Error("interval should trigger")
	}
	pct = 85
	if r := c.due(ctx, now); r == "" {
		t.Error("watermark should trigger")
	}
}
