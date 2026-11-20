package docker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// runnerStats keeps cumulative counters monotonic: when a job container is
// removed its cgroup disappears, so its last sample is folded into retired.
type runnerStats struct {
	perCont map[string]cgroupSample
	retired cgroupSample
	peakMem int64
}

func (b *Backend) forgetCgroup(id string) {
	b.mu.Lock()
	delete(b.cgPaths, id)
	b.mu.Unlock()
}

func (b *Backend) cgroupFor(id string) (string, bool) {
	b.mu.Lock()
	p, ok := b.cgPaths[id]
	b.mu.Unlock()
	if ok {
		return p, true
	}
	p, ok = findCgroup(b.cgroupRoot, b.cfg.Slice, id)
	if ok {
		b.mu.Lock()
		b.cgPaths[id] = p
		b.mu.Unlock()
	}
	return p, ok
}

// Stats sums the runner container and every job container labelled with the
// runner. CPU, I/O and OOM kills only grow; memory is the live sum.
func (b *Backend) Stats(ctx context.Context, h core.Handle) (core.Usage, error) {
	u := core.Usage{SampledAt: time.Now()}
	if h.BackendID != "" {
		insp, err := b.api.ContainerInspect(ctx, h.BackendID)
		switch {
		case err == nil:
			u.Alive = insp.State != nil && insp.State.Running
		case !client.IsErrNotFound(err):
			return u, err
		}
	}

	ids := []string{}
	if h.BackendID != "" {
		ids = append(ids, h.BackendID)
	}
	f := b.labelFilter(h.RunnerName)
	f.Add("label", naming.MetaRole+"=job")
	jobs, err := b.api.ContainerList(ctx, container.ListOptions{Filters: f})
	if err != nil {
		return u, err
	}
	for _, c := range jobs {
		ids = append(ids, c.ID)
	}

	cur := map[string]cgroupSample{}
	for _, id := range ids {
		if s, ok := b.sampleContainer(ctx, id); ok {
			cur[id] = s
		}
	}
	b.fold(h.RunnerName, cur, &u)
	u.LogBytes = dirSize(filepath.Join(b.runnerDir(h.RunnerName), "_diag"))
	return u, nil
}

func (b *Backend) fold(runner string, cur map[string]cgroupSample, u *core.Usage) {
	b.mu.Lock()
	defer b.mu.Unlock()
	rs := b.stats[runner]
	if rs == nil {
		rs = &runnerStats{perCont: map[string]cgroupSample{}}
		b.stats[runner] = rs
	}
	for id, last := range rs.perCont {
		if _, ok := cur[id]; !ok {
			rs.retired.cpuSeconds += last.cpuSeconds
			rs.retired.ioBytes += last.ioBytes
			rs.retired.oomKills += last.oomKills
			delete(rs.perCont, id)
		}
	}
	var sum cgroupSample
	for id, s := range cur {
		rs.perCont[id] = s
		sum.cpuSeconds += s.cpuSeconds
		sum.memCurrent += s.memCurrent
		sum.memPeak += s.memPeak
		sum.anon += s.anon
		sum.oomKills += s.oomKills
		sum.ioBytes += s.ioBytes
	}
	// Peak is monotonic: the larger of the cgroups' own peaks, the highest
	// live anonymous memory seen, and the highest live total seen.
	rs.peakMem = max(rs.peakMem, sum.memPeak, sum.anon, sum.memCurrent)
	u.CPUSeconds = rs.retired.cpuSeconds + sum.cpuSeconds
	u.MemCurrent = sum.memCurrent
	u.MemPeak = rs.peakMem
	u.IOBytes = rs.retired.ioBytes + sum.ioBytes
	u.OOMKills = rs.retired.oomKills + sum.oomKills
}

// sampleContainer reads cgroup v2 files, falling back to the Docker stats API.
func (b *Backend) sampleContainer(ctx context.Context, id string) (cgroupSample, bool) {
	if dir, ok := b.cgroupFor(id); ok {
		if s, ok := readCgroup(dir); ok {
			return s, true
		}
		b.forgetCgroup(id)
	}
	return b.apiSample(ctx, id)
}

func (b *Backend) apiSample(ctx context.Context, id string) (cgroupSample, bool) {
	r, err := b.api.ContainerStatsOneShot(ctx, id)
	if err != nil {
		return cgroupSample{}, false
	}
	defer r.Body.Close()
	var st container.StatsResponse
	if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
		return cgroupSample{}, false
	}
	s := cgroupSample{
		cpuSeconds: float64(st.CPUStats.CPUUsage.TotalUsage) / 1e9,
		memCurrent: int64(st.MemoryStats.Usage),
		memPeak:    int64(st.MemoryStats.MaxUsage),
		anon:       int64(st.MemoryStats.Stats["anon"]),
	}
	for _, e := range st.BlkioStats.IoServiceBytesRecursive {
		if e.Op == "read" || e.Op == "write" || e.Op == "Read" || e.Op == "Write" {
			s.ioBytes += int64(e.Value)
		}
	}
	return s, true
}
