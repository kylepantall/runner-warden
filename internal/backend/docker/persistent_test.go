package docker

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"

	"github.com/kylepantall/runner-warden/internal/naming"
)

// labelAPI applies ContainerList label filters like the daemon does.
type labelAPI struct{ *fakeAPI }

func (l labelAPI) ContainerList(ctx context.Context, o container.ListOptions) ([]container.Summary, error) {
	all, err := l.fakeAPI.ContainerList(ctx, o)
	if err != nil {
		return nil, err
	}
	var out []container.Summary
	for _, c := range all {
		ok := true
		for _, want := range o.Filters.Get("label") {
			k, v, _ := strings.Cut(want, "=")
			if c.Labels[k] != v {
				ok = false
			}
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

func mixedContainers() []container.Summary {
	base := func(runner, role string) map[string]string {
		l := map[string]string{naming.MetaManaged: "1", naming.MetaInstance: "main", naming.MetaHostID: "h1", naming.MetaRole: role}
		if runner != "" {
			l[naming.MetaRunner] = runner
		}
		return l
	}
	return []container.Summary{
		{ID: "job", Labels: base(testRunner, "job")},
		{ID: "runner", Labels: base(testRunner, "runner")},
		{ID: "pers", Names: []string{"/dev-1"}, Labels: base("", "persistent")},
		{ID: "pers-stale-runner", Labels: base(testRunner, "persistent")},
	}
}

func TestCleanupKeepsPersistentContainers(t *testing.T) {
	api := &fakeAPI{conts: mixedContainers()}
	b := testBackend(t, labelAPI{api})
	if err := b.Cleanup(context.Background(), testRunner); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, id := range api.removed {
		got[id] = true
	}
	if got["pers"] || got["pers-stale-runner"] {
		t.Fatal("persistent container removed by runner cleanup")
	}
	if !got["job"] || !got["runner"] {
		t.Fatalf("runner containers not removed: %v", api.removed)
	}
}

func TestListAndStopIgnorePersistent(t *testing.T) {
	api := &fakeAPI{conts: mixedContainers()}
	b := testBackend(t, labelAPI{api})
	hs, err := b.List(context.Background())
	if err != nil || len(hs) != 1 || hs[0].BackendID != "runner" {
		t.Fatalf("%v %v", hs, err)
	}
	if err := b.Stop(context.Background(), hs[0], 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range api.stopped {
		if id == "pers" {
			t.Fatal("persistent container stopped with the runner")
		}
	}
}

func TestPersistentReportsMemory(t *testing.T) {
	api := &fakeAPI{conts: mixedContainers()}
	b := testBackend(t, labelAPI{api})
	slice := filepath.Join(b.cgroupRoot, expandSlice(b.cfg.Slice))
	writeCg(t, filepath.Join(slice, "docker-pers.scope"), 1_000_000, 700, 800, 600, 0, "")
	writeCg(t, filepath.Join(slice, "docker-pers-stale-runner.scope"), 1_000_000, 50, 50, 50, 0, "")
	cs, err := b.Persistent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	names := map[string]bool{}
	for _, c := range cs {
		total += c.Mem
		names[c.Name] = true
	}
	if !names["dev-1"] || total != 750 {
		t.Fatalf("%+v", cs)
	}
}
