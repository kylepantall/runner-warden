package gh

import (
	"context"
	"errors"
	"testing"

	"github.com/actions/scaleset"

	"github.com/kylepantall/runner-warden/internal/core"
)

type fakeAPI struct {
	groups    map[string]int
	existing  *scaleset.RunnerScaleSet
	gotGroup  int
	created   *scaleset.RunnerScaleSet
	updated   *scaleset.RunnerScaleSet
	deleted   []int
	removed   []int64
	jitName   string
	removeErr error
}

func (f *fakeAPI) GetRunnerScaleSet(_ context.Context, g int, _ string) (*scaleset.RunnerScaleSet, error) {
	f.gotGroup = g
	return f.existing, nil
}
func (f *fakeAPI) CreateRunnerScaleSet(_ context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.created = rs
	c := *rs
	c.ID = 42
	return &c, nil
}
func (f *fakeAPI) UpdateRunnerScaleSet(_ context.Context, _ int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error) {
	f.updated = rs
	return rs, nil
}
func (f *fakeAPI) DeleteRunnerScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return nil
}
func (f *fakeAPI) GetRunnerGroupByName(_ context.Context, n string) (*scaleset.RunnerGroup, error) {
	id, ok := f.groups[n]
	if !ok {
		return nil, errors.New("no group")
	}
	return &scaleset.RunnerGroup{ID: id, Name: n}, nil
}
func (f *fakeAPI) GenerateJitRunnerConfig(_ context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, id int) (*scaleset.RunnerScaleSetJitRunnerConfig, error) {
	f.jitName = s.Name
	return &scaleset.RunnerScaleSetJitRunnerConfig{EncodedJITConfig: "enc", Runner: &scaleset.RunnerReference{ID: id + 1}}, nil
}
func (f *fakeAPI) RemoveRunner(_ context.Context, id int64) error {
	f.removed = append(f.removed, id)
	return f.removeErr
}
func (f *fakeAPI) MessageSessionClient(context.Context, int, string, ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error) {
	return nil, errors.New("not in tests")
}

func newFake(f *fakeAPI) *ScaleSets {
	s := NewScaleSets("t", "test")
	s.newClient = func(string) (scaleSetAPI, error) { return f, nil }
	return s
}

var orgKey = core.ScaleSetKey{Target: core.Target{Owner: "acme"}, OS: core.Linux}

func TestEnsureCreates(t *testing.T) {
	f := &fakeAPI{groups: map[string]int{"ci": 7}}
	s := newFake(f)
	id, err := s.Ensure(context.Background(), orgKey, "x-linux", []string{"x", "x-linux"}, "ci")
	if err != nil || id != 42 {
		t.Fatalf("id=%d err=%v", id, err)
	}
	if f.gotGroup != 7 || f.created.RunnerGroupID != 7 || !f.created.RunnerSetting.DisableUpdate {
		t.Fatalf("bad create: group=%d %+v", f.gotGroup, f.created)
	}
	if !sameLabels(f.created.Labels, []string{"x-linux", "x"}) {
		t.Fatalf("labels %+v", f.created.Labels)
	}
	if got, ok := s.ID(orgKey); !ok || got != 42 {
		t.Fatal("id not remembered")
	}
}

func TestEnsureRepoUsesDefaultGroup(t *testing.T) {
	f := &fakeAPI{}
	s := newFake(f)
	key := core.ScaleSetKey{Target: core.Target{Owner: "me", Repo: "r", RunnerGroup: "ignored"}, OS: core.Linux}
	if _, err := s.Ensure(context.Background(), key, "x-linux", nil, "ignored"); err != nil {
		t.Fatal(err)
	}
	if f.gotGroup != DefaultRunnerGroupID {
		t.Fatalf("group %d", f.gotGroup)
	}
}

func TestEnsureUpdatesLabelsOnlyWhenDifferent(t *testing.T) {
	f := &fakeAPI{existing: &scaleset.RunnerScaleSet{ID: 5, Name: "x-linux", Labels: []scaleset.Label{{Name: "x-linux"}}}}
	s := newFake(f)
	id, err := s.Ensure(context.Background(), orgKey, "x-linux", []string{"x"}, "")
	if err != nil || id != 5 {
		t.Fatal(id, err)
	}
	if f.updated == nil || !sameLabels(f.updated.Labels, []string{"x-linux", "x"}) || f.created != nil {
		t.Fatalf("expected update, got %+v", f.updated)
	}
	f.updated = nil
	f.existing.Labels = toLabels([]string{"x", "x-linux"})
	if _, err = s.Ensure(context.Background(), orgKey, "x-linux", []string{"x"}, ""); err != nil || f.updated != nil {
		t.Fatalf("unexpected update %v %v", f.updated, err)
	}
}

func TestJITDeleteRemove(t *testing.T) {
	f := &fakeAPI{existing: &scaleset.RunnerScaleSet{ID: 9, Name: "x-linux", Labels: toLabels([]string{"x-linux"})}}
	s := newFake(f)
	if _, _, err := s.GenerateJIT(context.Background(), orgKey, "r1"); !errors.Is(err, ErrUnknownScaleSet) {
		t.Fatalf("want ErrUnknownScaleSet, got %v", err)
	}
	if _, err := s.Ensure(context.Background(), orgKey, "x-linux", nil, ""); err != nil {
		t.Fatal(err)
	}
	enc, rid, err := s.GenerateJIT(context.Background(), orgKey, "r1")
	if err != nil || enc != "enc" || rid != 10 || f.jitName != "r1" {
		t.Fatal(enc, rid, err)
	}
	f.removeErr = scaleset.RunnerNotFoundError
	if err := s.RemoveRunner(context.Background(), orgKey, rid); err != nil {
		t.Fatalf("missing runner should be ok: %v", err)
	}
	f.removeErr = errors.New("boom")
	if err := s.RemoveRunner(context.Background(), orgKey, rid); err == nil {
		t.Fatal("want error")
	}
	if err := s.Delete(context.Background(), orgKey); err != nil || len(f.deleted) != 1 || f.deleted[0] != 9 {
		t.Fatal(err, f.deleted)
	}
	if _, ok := s.ID(orgKey); ok {
		t.Fatal("id should be forgotten")
	}
}
