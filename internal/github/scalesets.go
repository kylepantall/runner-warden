package gh

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"

	"github.com/kylepantall/runner-warden/internal/core"
)

// DefaultRunnerGroupID is the id of the default runner group. Repo-level scale
// sets always use it.
const DefaultRunnerGroupID = 1

// ErrUnknownScaleSet is returned when a key has no known scale-set id yet
// (Ensure has not run).
var ErrUnknownScaleSet = errors.New("scale set id unknown; call Ensure first")

// scaleSetAPI is the subset of *scaleset.Client used here (fakeable in tests).
type scaleSetAPI interface {
	GetRunnerScaleSet(ctx context.Context, runnerGroupID int, name string) (*scaleset.RunnerScaleSet, error)
	CreateRunnerScaleSet(ctx context.Context, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	UpdateRunnerScaleSet(ctx context.Context, id int, rs *scaleset.RunnerScaleSet) (*scaleset.RunnerScaleSet, error)
	DeleteRunnerScaleSet(ctx context.Context, id int) error
	GetRunnerGroupByName(ctx context.Context, group string) (*scaleset.RunnerGroup, error)
	GenerateJitRunnerConfig(ctx context.Context, s *scaleset.RunnerScaleSetJitRunnerSetting, scaleSetID int) (*scaleset.RunnerScaleSetJitRunnerConfig, error)
	RemoveRunner(ctx context.Context, runnerID int64) error
	MessageSessionClient(ctx context.Context, scaleSetID int, owner string, opts ...scaleset.HTTPOption) (*scaleset.MessageSessionClient, error)
}

// Session is a scale-set message session.
type Session interface {
	listener.Client
	Close(ctx context.Context) error
}

// ScaleSets manages the scale sets an instance owns. It keeps one scaleset
// client per target and remembers the id of every scale set it ensured.
type ScaleSets struct {
	token   string
	sysInfo scaleset.SystemInfo
	// newClient builds the per-target client; replaced in tests.
	newClient func(targetURL string) (scaleSetAPI, error)

	mu      sync.Mutex
	clients map[string]scaleSetAPI
	ids     map[core.ScaleSetKey]int
}

// NewScaleSets returns a ScaleSets authenticating with a token (PAT or gh
// OAuth token).
func NewScaleSets(token, version string) *ScaleSets {
	s := &ScaleSets{
		token:   token,
		sysInfo: scaleset.SystemInfo{System: "runner-warden", Version: version},
		clients: map[string]scaleSetAPI{},
		ids:     map[core.ScaleSetKey]int{},
	}
	s.newClient = func(u string) (scaleSetAPI, error) {
		return scaleset.NewClientWithPersonalAccessToken(scaleset.NewClientWithPersonalAccessTokenConfig{
			GitHubConfigURL:     u,
			PersonalAccessToken: s.token,
			SystemInfo:          s.sysInfo,
		})
	}
	return s
}

func (s *ScaleSets) client(t core.Target) (scaleSetAPI, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u := t.URL()
	if c, ok := s.clients[u]; ok {
		return c, nil
	}
	c, err := s.newClient(u)
	if err != nil {
		return nil, fmt.Errorf("scale set client for %s: %w", t, err)
	}
	s.clients[u] = c
	return c, nil
}

// ID returns the scale-set id remembered for key.
func (s *ScaleSets) ID(key core.ScaleSetKey) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.ids[key]
	return id, ok
}

func (s *ScaleSets) remember(key core.ScaleSetKey, id int) {
	s.mu.Lock()
	s.ids[key] = id
	s.mu.Unlock()
}

func (s *ScaleSets) mustID(key core.ScaleSetKey) (int, error) {
	id, ok := s.ID(key)
	if !ok {
		return 0, fmt.Errorf("%w: %s/%s", ErrUnknownScaleSet, key.Target, key.OS)
	}
	return id, nil
}

// groupID resolves the runner group: repo targets and the default/empty group
// use the default id; other org groups are looked up by name.
func groupID(ctx context.Context, c scaleSetAPI, t core.Target, group string) (int, error) {
	if !t.IsOrg() || group == "" || group == scaleset.DefaultRunnerGroup {
		return DefaultRunnerGroupID, nil
	}
	g, err := c.GetRunnerGroupByName(ctx, group)
	if err != nil {
		return 0, fmt.Errorf("resolve runner group %q: %w", group, err)
	}
	return g.ID, nil
}

// labelSet returns name plus extra labels, deduplicated, name first.
func labelSet(name string, labels []string) []string {
	out := []string{name}
	for _, l := range labels {
		if l != "" && !slices.Contains(out, l) {
			out = append(out, l)
		}
	}
	return out
}

func sameLabels(have []scaleset.Label, want []string) bool {
	if len(have) != len(want) {
		return false
	}
	for _, h := range have {
		if !slices.Contains(want, h.Name) {
			return false
		}
	}
	return true
}

// Ensure gets or creates the scale set `name` for key, aligns its labels with
// labels (name is always included) and returns its id. Created scale sets set
// DisableUpdate so GitHub never self-updates our runners.
func (s *ScaleSets) Ensure(ctx context.Context, key core.ScaleSetKey, name string, labels []string, runnerGroup string) (int, error) {
	c, err := s.client(key.Target)
	if err != nil {
		return 0, err
	}
	gid, err := groupID(ctx, c, key.Target, runnerGroup)
	if err != nil {
		return 0, err
	}
	want := labelSet(name, labels)
	ss, err := c.GetRunnerScaleSet(ctx, gid, name)
	if err != nil {
		return 0, fmt.Errorf("get scale set: %w", err)
	}
	if ss == nil {
		ss, err = c.CreateRunnerScaleSet(ctx, &scaleset.RunnerScaleSet{
			Name:          name,
			RunnerGroupID: gid,
			Labels:        toLabels(want),
			RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
		})
		if err != nil {
			return 0, fmt.Errorf("create scale set: %w", err)
		}
	} else if !sameLabels(ss.Labels, want) {
		upd := *ss
		upd.Labels = toLabels(want)
		upd.RunnerSetting.DisableUpdate = true
		if _, err = c.UpdateRunnerScaleSet(ctx, ss.ID, &upd); err != nil {
			return 0, fmt.Errorf("update scale set labels: %w", err)
		}
	}
	s.remember(key, ss.ID)
	return ss.ID, nil
}

func toLabels(names []string) []scaleset.Label {
	out := make([]scaleset.Label, len(names))
	for i, n := range names {
		out[i] = scaleset.Label{Name: n}
	}
	return out
}

// Delete removes the scale set Ensure returned for key.
func (s *ScaleSets) Delete(ctx context.Context, key core.ScaleSetKey) error {
	id, err := s.mustID(key)
	if err != nil {
		return err
	}
	if err := s.DeleteByID(ctx, key.Target, id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.ids, key)
	s.mu.Unlock()
	return nil
}

// DeleteByID removes a scale set by id (reconciler use; id from a state DB).
func (s *ScaleSets) DeleteByID(ctx context.Context, t core.Target, id int) error {
	c, err := s.client(t)
	if err != nil {
		return err
	}
	if err := c.DeleteRunnerScaleSet(ctx, id); err != nil {
		return fmt.Errorf("delete scale set %d: %w", id, err)
	}
	return nil
}

// GenerateJIT registers an ephemeral runner named runnerName and returns the
// encoded JIT config and the runner id. workFolder is optional (Windows VMs
// pass an absolute per-job folder); empty keeps the runner default `_work`.
func (s *ScaleSets) GenerateJIT(ctx context.Context, key core.ScaleSetKey, runnerName string, workFolder ...string) (string, int, error) {
	id, err := s.mustID(key)
	if err != nil {
		return "", 0, err
	}
	c, err := s.client(key.Target)
	if err != nil {
		return "", 0, err
	}
	setting := &scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName}
	if len(workFolder) > 0 {
		setting.WorkFolder = workFolder[0]
	}
	cfg, err := c.GenerateJitRunnerConfig(ctx, setting, id)
	if err != nil {
		return "", 0, fmt.Errorf("generate jit config: %w", err)
	}
	if cfg == nil || cfg.Runner == nil {
		return "", 0, errors.New("generate jit config: empty response")
	}
	return cfg.EncodedJITConfig, cfg.Runner.ID, nil
}

// RemoveRunner deregisters a runner by id. A missing runner is not an error.
func (s *ScaleSets) RemoveRunner(ctx context.Context, key core.ScaleSetKey, runnerID int) error {
	c, err := s.client(key.Target)
	if err != nil {
		return err
	}
	if err := c.RemoveRunner(ctx, int64(runnerID)); err != nil {
		if errors.Is(err, scaleset.RunnerNotFoundError) || errors.Is(err, scaleset.NotFoundError) {
			return nil
		}
		return fmt.Errorf("remove runner %d: %w", runnerID, err)
	}
	return nil
}

// OpenSession opens a message session for key's scale set.
func (s *ScaleSets) OpenSession(ctx context.Context, key core.ScaleSetKey, owner string) (Session, error) {
	id, err := s.mustID(key)
	if err != nil {
		return nil, err
	}
	c, err := s.client(key.Target)
	if err != nil {
		return nil, err
	}
	sess, err := c.MessageSessionClient(ctx, id, owner)
	if err != nil {
		return nil, err
	}
	return sess, nil
}
