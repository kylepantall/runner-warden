package imagegc

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/docker/docker/api/types/image"

	"github.com/kylepantall/runner-warden/internal/pulllock"
)

func lockedDocker() *fakeDocker {
	return &fakeDocker{
		imgs:    []image.Summary{{ID: "a", RepoTags: []string{"a:1"}}},
		tagTime: map[string]time.Time{"a": now.Add(-48 * time.Hour)},
	}
}

// A pull holding the shared file lock (as another process's proxy would)
// keeps GC from pruning; once it ends GC proceeds.
func TestRunBlocksWhileAPullHoldsTheFileLock(t *testing.T) {
	l := pulllock.New(filepath.Join(t.TempDir(), ".pull.lock"))
	pull, err := l.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := lockedDocker()
	c := newGC(t, d, []PullGate{pulllock.NewGate(l)}, nil)
	res, err := c.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !res.DrainTimedOut || len(d.removed) != 0 {
		t.Fatalf("GC must skip while a pull runs: %+v removed=%v", res, d.removed)
	}
	pull.Close()
	res, err = c.Run(context.Background())
	if err != nil || res.DrainTimedOut || len(d.removed) != 1 {
		t.Fatalf("GC after the pull ended: %+v %v removed=%v", res, err, d.removed)
	}
}

// While GC prunes, a pull from another process waits for the pass to end.
func TestPullsWaitWhileGCRuns(t *testing.T) {
	l := pulllock.New(filepath.Join(t.TempDir(), ".pull.lock"))
	d := lockedDocker()
	blocked := make(chan bool, 1)
	d.onRemove = func(string) {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		f, err := l.RLock(ctx)
		if err == nil {
			f.Close()
		}
		blocked <- err != nil
	}
	c := newGC(t, d, []PullGate{pulllock.NewGate(l)}, nil)
	if _, err := c.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !<-blocked {
		t.Fatal("a pull got through during the prune pass")
	}
	f, err := l.RLock(context.Background())
	if err != nil {
		t.Fatalf("pulls must proceed after the pass: %v", err)
	}
	f.Close()
}
