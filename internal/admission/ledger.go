// Package admission holds the host-wide resource ledger (memory plus whole
// physical-core pinning), cost-rule matching and the anti-starvation and
// fairness helpers the scheduler uses. Everything here is pure and
// deterministic; the Ledger is not safe for concurrent use (the scheduler
// serialises access).
package admission

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Errors returned by Reserve and Resize.
var (
	ErrNoMemory = errors.New("admission: not enough free memory")
	ErrNoCores  = errors.New("admission: not enough free physical cores")
	ErrExists   = errors.New("admission: key already reserved")
	ErrUnknown  = errors.New("admission: unknown reservation")
)

// Core is one physical core and its hardware threads (logical CPU ids).
type Core struct {
	ID      int
	Threads []int
}

// Request is what a reservation asks for.
type Request struct {
	Mem         int64 // bytes
	PinnedCores int   // whole physical cores, 0 = shared
}

// Reservation is a granted request.
type Reservation struct {
	Key   string
	Mem   int64
	Cores []int // physical core ids pinned to the holder (ascending)
	CPUs  []int // logical cpu ids of those cores (ascending)
}

// CPUSet formats the pinned logical CPUs as a cpuset string ("2,3,10,11"), ""
// when nothing is pinned.
func (r Reservation) CPUSet() string {
	parts := make([]string, len(r.CPUs))
	for i, c := range r.CPUs {
		parts[i] = strconv.Itoa(c)
	}
	return strings.Join(parts, ",")
}

// Config configures a Ledger.
type Config struct {
	Budget       int64 // total memory budget (slice MemoryMax)
	External     int64 // memory reserved for things outside the ledger
	Cores        []Core
	ReservedCPUs []int // logical cpus never pinned (config reserved_cores)
}

// Usage is a point-in-time summary.
type Usage struct {
	Budget      int64
	External    int64
	Reserved    int64
	Free        int64
	TotalCores  int // pinnable physical cores
	PinnedCores int
}

// Ledger tracks reservations against one budget.
type Ledger struct {
	budget   int64
	external int64
	reserved int64
	res      map[string]*Reservation
	cores    []Core         // pinnable cores, ascending id
	owner    map[int]string // core id -> key
}

// New builds a Ledger. Cores that contain any reserved CPU are not pinnable.
func New(c Config) *Ledger {
	reserved := map[int]bool{}
	for _, cpu := range c.ReservedCPUs {
		reserved[cpu] = true
	}
	var cores []Core
	for _, core := range c.Cores {
		skip := false
		for _, t := range core.Threads {
			if reserved[t] {
				skip = true
			}
		}
		if !skip {
			t := append([]int(nil), core.Threads...)
			sort.Ints(t)
			cores = append(cores, Core{ID: core.ID, Threads: t})
		}
	}
	sort.Slice(cores, func(i, j int) bool { return cores[i].ID < cores[j].ID })
	return &Ledger{
		budget:   c.Budget,
		external: c.External,
		res:      map[string]*Reservation{},
		cores:    cores,
		owner:    map[int]string{},
	}
}

// SetBudget changes the total budget.
func (l *Ledger) SetBudget(n int64) { l.budget = n }

// SetExternal changes the memory reserved outside the ledger.
func (l *Ledger) SetExternal(n int64) { l.external = n }

// Budget returns the total budget.
func (l *Ledger) Budget() int64 { return l.budget }

// Reserved is the memory held by reservations.
func (l *Ledger) Reserved() int64 { return l.reserved }

// Free is budget - external - reserved; it may be negative after Force.
func (l *Ledger) Free() int64 { return l.budget - l.external - l.reserved }

// FreeCores counts pinnable cores not held by any reservation.
func (l *Ledger) FreeCores() int {
	n := 0
	for _, c := range l.cores {
		if _, held := l.owner[c.ID]; !held {
			n++
		}
	}
	return n
}

// CanFit reports whether req would be granted now.
func (l *Ledger) CanFit(req Request) bool {
	return req.Mem <= l.Free() && req.PinnedCores <= l.FreeCores()
}

// FitsEver reports whether req could be granted on an otherwise empty ledger.
func (l *Ledger) FitsEver(req Request) bool {
	return req.Mem <= l.budget-l.external && req.PinnedCores <= len(l.cores)
}

// Get returns a reservation by key.
func (l *Ledger) Get(key string) (Reservation, bool) {
	r, ok := l.res[key]
	if !ok {
		return Reservation{}, false
	}
	return clone(r), true
}

// Keys returns every reservation key, sorted.
func (l *Ledger) Keys() []string {
	out := make([]string, 0, len(l.res))
	for k := range l.res {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Usage summarises the ledger.
func (l *Ledger) Usage() Usage {
	return Usage{
		Budget: l.budget, External: l.external, Reserved: l.reserved, Free: l.Free(),
		TotalCores: len(l.cores), PinnedCores: len(l.owner),
	}
}

// Reserve grants req under key, or fails without side effects.
func (l *Ledger) Reserve(key string, req Request) (Reservation, error) {
	if _, ok := l.res[key]; ok {
		return Reservation{}, fmt.Errorf("%w: %s", ErrExists, key)
	}
	if req.Mem > l.Free() {
		return Reservation{}, ErrNoMemory
	}
	if req.PinnedCores > l.FreeCores() {
		return Reservation{}, ErrNoCores
	}
	r := &Reservation{Key: key, Mem: req.Mem}
	l.pin(r, req.PinnedCores)
	l.res[key] = r
	l.reserved += req.Mem
	return clone(r), nil
}

// Force records a reservation even when it overcommits memory (a job already
// running, a VM already up). Cores are pinned as far as free cores allow.
func (l *Ledger) Force(key string, req Request) Reservation {
	if old, ok := l.res[key]; ok {
		l.reserved -= old.Mem
		l.unpin(old)
		delete(l.res, key)
	}
	r := &Reservation{Key: key, Mem: req.Mem}
	n := req.PinnedCores
	if f := l.FreeCores(); n > f {
		n = f
	}
	l.pin(r, n)
	l.res[key] = r
	l.reserved += req.Mem
	return clone(r)
}

// Resize changes an existing reservation to req, failing without side effects
// when the growth does not fit. Existing pinned cores are kept when the new
// request still wants at least as many.
func (l *Ledger) Resize(key string, req Request) (Reservation, error) {
	r, ok := l.res[key]
	if !ok {
		return Reservation{}, ErrUnknown
	}
	if req.Mem-r.Mem > l.Free() {
		return Reservation{}, ErrNoMemory
	}
	if req.PinnedCores-len(r.Cores) > l.FreeCores() {
		return Reservation{}, ErrNoCores
	}
	l.applyResize(r, req)
	return clone(r), nil
}

// ForceResize is Resize that overcommits memory when needed.
func (l *Ledger) ForceResize(key string, req Request) (Reservation, error) {
	r, ok := l.res[key]
	if !ok {
		return Reservation{}, ErrUnknown
	}
	if extra := req.PinnedCores - len(r.Cores); extra > l.FreeCores() {
		req.PinnedCores = len(r.Cores) + l.FreeCores()
	}
	l.applyResize(r, req)
	return clone(r), nil
}

func (l *Ledger) applyResize(r *Reservation, req Request) {
	l.reserved += req.Mem - r.Mem
	r.Mem = req.Mem
	switch {
	case req.PinnedCores < len(r.Cores):
		l.unpin(r)
		l.pin(r, req.PinnedCores)
	case req.PinnedCores > len(r.Cores):
		l.pin(r, req.PinnedCores-len(r.Cores))
	}
}

// Release frees a reservation; it reports whether the key existed.
func (l *Ledger) Release(key string) bool {
	r, ok := l.res[key]
	if !ok {
		return false
	}
	l.reserved -= r.Mem
	l.unpin(r)
	delete(l.res, key)
	return true
}

// pin adds n more free cores (lowest id first) to r.
func (l *Ledger) pin(r *Reservation, n int) {
	for _, c := range l.cores {
		if n == 0 {
			break
		}
		if _, held := l.owner[c.ID]; held {
			continue
		}
		l.owner[c.ID] = r.Key
		r.Cores = append(r.Cores, c.ID)
		r.CPUs = append(r.CPUs, c.Threads...)
		n--
	}
	sort.Ints(r.Cores)
	sort.Ints(r.CPUs)
}

func (l *Ledger) unpin(r *Reservation) {
	for _, id := range r.Cores {
		delete(l.owner, id)
	}
	r.Cores, r.CPUs = nil, nil
}

func clone(r *Reservation) Reservation {
	c := *r
	c.Cores = append([]int(nil), r.Cores...)
	c.CPUs = append([]int(nil), r.CPUs...)
	return c
}
