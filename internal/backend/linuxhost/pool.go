package linuxhost

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/core"
)

// Local is this machine's Linux backend (the Docker backend).
type Local interface {
	core.Backend
	EnsureImage(ctx context.Context, ref string) error
	Adopt(ctx context.Context, h core.Handle) error
	Persistent(ctx context.Context) ([]core.PersistentContainer, error)
}

// listGrace is how long a host's last runner list stands in for one it
// cannot give: a short outage must not make its runners look gone.
const listGrace = 15 * time.Minute

// ProbeEvery is how often Loop re-probes the hosts.
const ProbeEvery = time.Minute

type listed struct {
	at time.Time
	hs []core.Handle
}

// Pool is the Linux backend: this machine plus the remote hosts.
type Pool struct {
	local Local
	hosts map[string]*Host
	order []string
	log   *slog.Logger

	mu    sync.Mutex
	where map[string]string // runner -> host, for runners on a host
	last  map[string]listed
}

var _ core.Backend = (*Pool)(nil)

// NewPool combines local with hosts (probed by Prepare and Loop).
func NewPool(local Local, hosts []*Host, log *slog.Logger) *Pool {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	p := &Pool{local: local, hosts: map[string]*Host{}, log: log, where: map[string]string{}, last: map[string]listed{}}
	for _, h := range hosts {
		p.hosts[h.Name()] = h
		p.order = append(p.order, h.Name())
	}
	return p
}

// IsUp reports whether a host is reachable and prepared; it never blocks.
func (p *Pool) IsUp(name string) bool {
	h := p.hosts[name]
	return h != nil && h.Up()
}

// HostsUp maps every host to its state.
func (p *Pool) HostsUp() map[string]bool {
	out := map[string]bool{}
	for n, h := range p.hosts {
		out[n] = h.Up()
	}
	return out
}

// Hosts lists the host names in config order.
func (p *Pool) Hosts() []string { return append([]string(nil), p.order...) }

// Caps implements core.Backend.
func (p *Pool) Caps() core.Caps { return p.local.Caps() }

// PrepareTimeout bounds the hosts' first probe at daemon start.
const PrepareTimeout = 3 * time.Minute

// Prepare prepares this machine and probes the hosts, in parallel and for at
// most PrepareTimeout: runners on a host are adopted only if it answers, and
// an unreachable host must not hold up the daemon for long.
func (p *Pool) Prepare(ctx context.Context) error {
	if err := p.local.Prepare(ctx); err != nil {
		return err
	}
	p.probeAll(ctx, PrepareTimeout)
	return nil
}

func (p *Pool) probeAll(ctx context.Context, limit time.Duration) {
	var wg sync.WaitGroup
	for _, h := range p.hosts {
		wg.Go(func() {
			pctx, cancel := context.WithTimeout(ctx, limit)
			defer cancel()
			_ = h.Probe(pctx)
		})
	}
	wg.Wait()
}

// Loop re-probes every host until ctx ends.
func (p *Pool) Loop(ctx context.Context) {
	t := time.NewTicker(ProbeEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		p.probeAll(ctx, 10*time.Minute)
	}
}

func (p *Pool) host(name string) (*Host, error) {
	if h := p.hosts[name]; h != nil {
		return h, nil
	}
	return nil, fmt.Errorf("unknown linux host %q", name)
}

func (p *Pool) note(runner, host string) {
	p.mu.Lock()
	p.where[runner] = host
	p.mu.Unlock()
}

// Start implements core.Backend: on the host named by HostEnv, else here.
func (p *Pool) Start(ctx context.Context, spec core.RunnerSpec) (core.Handle, error) {
	name := spec.Env[HostEnv]
	if name == "" {
		return p.local.Start(ctx, spec)
	}
	h, err := p.host(name)
	if err != nil {
		return core.Handle{}, err
	}
	env := maps.Clone(spec.Env)
	delete(env, HostEnv)
	spec.Env = env
	p.note(spec.Name, name)
	hd, err := h.Start(ctx, spec)
	if err != nil {
		return core.Handle{}, err
	}
	hd.BackendID = onHost(name, hd.BackendID)
	return hd, nil
}

// route finds the backend of a handle and the handle as that backend knows it.
func (p *Pool) route(hd core.Handle) (core.Backend, *Host, core.Handle, error) {
	name, id := HostOf(hd.BackendID)
	if name == "" {
		return p.local, nil, hd, nil
	}
	h, err := p.host(name)
	hd.BackendID = id
	return h, h, hd, err
}

// Stats implements core.Backend.
func (p *Pool) Stats(ctx context.Context, hd core.Handle) (core.Usage, error) {
	b, _, hd, err := p.route(hd)
	if err != nil {
		return core.Usage{}, err
	}
	return b.Stats(ctx, hd)
}

// Stop implements core.Backend.
func (p *Pool) Stop(ctx context.Context, hd core.Handle, grace time.Duration) error {
	b, _, hd, err := p.route(hd)
	if err != nil {
		return err
	}
	return b.Stop(ctx, hd, grace)
}

// Cleanup implements core.Backend. A runner of unknown placement is cleaned
// here and on every reachable host (cleanup is idempotent).
func (p *Pool) Cleanup(ctx context.Context, runner string) error {
	p.mu.Lock()
	name, known := p.where[runner]
	delete(p.where, runner)
	p.mu.Unlock()
	if known {
		h, err := p.host(name)
		if err != nil {
			return err
		}
		return h.Cleanup(ctx, runner)
	}
	err := p.local.Cleanup(ctx, runner)
	for _, h := range p.hosts {
		if h.Up() {
			if herr := h.Cleanup(ctx, runner); herr != nil {
				p.log.Debug("host cleanup failed", "host", h.Name(), "runner", runner, "error", herr.Error())
			}
		}
	}
	return err
}

// List implements core.Backend. A host that cannot answer contributes its
// last list for listGrace, then nothing.
func (p *Pool) List(ctx context.Context) ([]core.Handle, error) {
	out, err := p.local.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range p.order {
		h := p.hosts[name]
		// Asked even when down: a host that failed its probe may still
		// answer for the runners it has.
		hs, lerr := h.List(ctx)
		p.mu.Lock()
		if lerr == nil {
			for r, at := range p.where {
				if at == name {
					delete(p.where, r)
				}
			}
			for i := range hs {
				hs[i].BackendID = onHost(name, hs[i].BackendID)
				p.where[hs[i].RunnerName] = name
			}
			p.last[name] = listed{at: time.Now(), hs: hs}
		} else if l, ok := p.last[name]; ok && time.Since(l.at) < listGrace {
			hs = l.hs
		} else {
			hs = nil
		}
		p.mu.Unlock()
		out = append(out, hs...)
	}
	return out, nil
}

// EnsureImage pulls a per-job image here; a host pulls its own at start.
func (p *Pool) EnsureImage(ctx context.Context, ref string) error {
	return p.local.EnsureImage(ctx, ref)
}

// Adopt re-checks an adopted runner's Docker proxy where it runs.
func (p *Pool) Adopt(ctx context.Context, hd core.Handle) error {
	_, h, rhd, err := p.route(hd)
	if err != nil {
		return err
	}
	if h != nil {
		p.note(hd.RunnerName, h.Name())
		return h.Adopt(ctx, rhd)
	}
	return p.local.Adopt(ctx, hd)
}

// Persistent reports this machine's persistent containers (they count
// against this machine's budget only).
func (p *Pool) Persistent(ctx context.Context) ([]core.PersistentContainer, error) {
	return p.local.Persistent(ctx)
}
