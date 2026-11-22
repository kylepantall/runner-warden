package scheduler

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/core"
)

const tickInterval = 10 * time.Second

type phase int

const (
	pHeld phase = iota
	pStarting
	pBound
)

type jobRec struct {
	id        string
	job       core.Job
	lane      LaneKey
	phase     phase
	cost      int64
	cpu       int64 // shared millicpus
	pinned    int
	devices   []string
	image     string
	local     bool   // must run on this host (rule, devices or pinned cores)
	machine   string // must run on this machine (rule), "" for any
	age       time.Time
	ageKey    string
	seq       int
	rr        *runnerRec
	inQueue   bool
	unboundAt time.Time // non-zero: GitHub un-assigned it (reassign churn)
	notBefore time.Time // earliest next start attempt
	startedAt time.Time
	phantom   bool // placeholder for a job GitHub assigned that we never heard of
	rerun     bool // runner lost mid-job; rerun once after completion
	synthetic bool // id is ours (job id unknown): match events by runner name
}

type runnerRec struct {
	tok       string // ledger key
	seq       int
	name      string // "" until StartRunner returns
	lane      LaneKey
	full      bool // reservation covers the job (else overhead only)
	parked    bool // job bound/starting but hook must wait for full
	job       *jobRec
	createdAt time.Time
	hookOK    bool
	jobDone   bool // job-completed hook seen: a later exit is normal
	released  bool
	vmKey     string // ledger key of the pending VM reservation, "" once converted
	host      string // remote Linux host
	remote    bool   // on another machine (Linux or Windows host): reserves nothing here
}

type laneRec struct {
	key    LaneKey
	queue  []*jobRec // unstarted jobs, oldest first; only the head may get a runner
	capSet bool
	cap    int
	stats  LaneStats

	hasStats     bool
	statsAt      time.Time
	statsInitial bool      // first stats of this session: trusted past StatsMaxAge
	changedAt    time.Time // last event that makes older stats misleading
	deficitSince time.Time // when GitHub first showed more unstarted jobs than we know
}

type ageEntry struct {
	t    time.Time
	seen time.Time
}

type hookWaiter struct {
	req      HookRequest
	since    time.Time
	deadline time.Time
	ch       chan HookResponse
	done     bool
}

type rerunReq struct {
	owner, repo string
	runID       int64
	job         string
	due         time.Time
	tries       int
}

// Scheduler is the lane state machine. All methods are safe for concurrent
// use; state changes happen under one mutex and side effects (runner calls)
// run afterwards in goroutines tracked by Wait.
type Scheduler struct {
	o   Options
	log logger

	mu      sync.Mutex
	base    context.Context
	led     *admission.Ledger
	lanes   map[LaneKey]*laneRec
	jobs    map[string]*jobRec
	byAge   map[string]*jobRec
	ages    map[string]ageEntry
	runners map[string]*runnerRec // by token
	byName  map[string]*runnerRec
	waiters []*hookWaiter
	reaps   map[string]time.Time
	stopped map[string]time.Time // runners already sent to StopRunner: stop once
	done    map[string]time.Time // recently finished job ids: late events are ignored
	reruns  []rerunReq
	reran   map[string]bool
	stats   map[LaneKey]LaneStats
	seq     int
	drain   bool
	fx      []func(context.Context)
	wg      sync.WaitGroup
}

// New builds a Scheduler.
func New(o Options) *Scheduler {
	o.defaults()
	s := &Scheduler{
		o:       o,
		log:     logger{o.Log},
		base:    context.Background(),
		lanes:   map[LaneKey]*laneRec{},
		jobs:    map[string]*jobRec{},
		byAge:   map[string]*jobRec{},
		ages:    map[string]ageEntry{},
		runners: map[string]*runnerRec{},
		byName:  map[string]*runnerRec{},
		reaps:   map[string]time.Time{},
		stopped: map[string]time.Time{},
		done:    map[string]time.Time{},
		reran:   map[string]bool{},
		stats:   map[LaneKey]LaneStats{},
		led: admission.New(admission.Config{
			Budget: o.Budget, External: o.ExternalReserved, Cores: o.Cores, ReservedCPUs: o.ReservedCPUs,
			CPUBudget: o.CPUBudget,
		}),
	}
	s.locked(func() {
		for _, lk := range o.Lanes {
			s.lane(lk)
		}
	})
	return s
}

// Run drives the periodic tick until ctx ends, then waits for in-flight
// runner calls.
func (s *Scheduler) Run(ctx context.Context) error {
	s.mu.Lock()
	s.base = ctx
	s.mu.Unlock()
	t := s.o.Clock.NewTicker(tickInterval)
	defer t.Stop()
	s.Tick()
	for {
		select {
		case <-ctx.Done():
			s.Wait()
			return ctx.Err()
		case <-t.C():
			s.Tick()
		}
	}
}

// Wait blocks until every in-flight runner call has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

// locked runs f under the lock, then settles (grants, promotions, hook
// verdicts, capacity) and launches the collected side effects.
func (s *Scheduler) locked(f func()) {
	s.mu.Lock()
	f()
	s.settle()
	fx, ctx := s.fx, s.base
	s.fx = nil
	s.wg.Add(len(fx))
	s.mu.Unlock()
	for _, e := range fx {
		go func() {
			defer s.wg.Done()
			e(ctx)
		}()
	}
}

func (s *Scheduler) effect(f func(context.Context)) { s.fx = append(s.fx, f) }

func (s *Scheduler) now() time.Time { return s.o.Clock.Now() }

func (s *Scheduler) lane(lk LaneKey) *laneRec {
	l := s.lanes[lk]
	if l == nil {
		l = &laneRec{key: lk}
		s.lanes[lk] = l
	}
	return l
}

func (s *Scheduler) overhead() admission.Request { return admission.Request{Mem: s.o.RunnerOverhead} }

// charge is what rr holds in this host's ledger: nothing when it runs on
// another machine (a Linux host or a Windows host).
func charge(rr *runnerRec, req admission.Request) admission.Request {
	if rr.remote {
		return admission.Request{}
	}
	return req
}

// vmManaged reports lanes whose jobs run in a VM the scheduler budgets for.
func (s *Scheduler) vmManaged(lk LaneKey) bool {
	return s.o.VMCost != nil && lk.OS == core.Windows
}

// fullReq is what a runner holds for the job. On VM-managed lanes the VM's
// memory (reserved separately) is the job's memory, so only overhead remains.
func (s *Scheduler) fullReq(r *jobRec) admission.Request {
	if s.vmManaged(r.lane) {
		return admission.Request{Mem: s.o.RunnerOverhead, PinnedCores: r.pinned, CPU: r.cpu}
	}
	return admission.Request{Mem: r.cost + s.o.RunnerOverhead, PinnedCores: r.pinned, CPU: r.cpu}
}

// remoteVM reports that a new grant on lk goes to a separate Windows machine.
// It is decided once, at the grant: later the lane's next job may go
// elsewhere, which says nothing about the runners already started.
func (s *Scheduler) remoteVM(lk LaneKey) bool {
	return s.vmManaged(lk) && s.o.RemoteVM != nil && s.o.RemoteVM(lk)
}

// vmNeed is the VM memory a new grant on lk must reserve: zero unless the lane
// is VM-managed and no suitable VM is up.
func (s *Scheduler) vmNeed(lk LaneKey) int64 {
	if !s.vmManaged(lk) {
		return 0
	}
	if mem, up := s.o.VMCost(lk); !up {
		return mem
	}
	return 0
}

// heldCores is how many pinned cores the runner was started with.
func (s *Scheduler) heldCores(rr *runnerRec) int {
	r, _ := s.led.Get(rr.tok)
	return len(r.Cores)
}

// reqFor is fullReq for a job on an existing runner. A container's cpuset is
// fixed when it is created, so a runner never gains or changes pinned cores.
func (s *Scheduler) reqFor(rr *runnerRec, rec *jobRec) admission.Request {
	req := s.fullReq(rec)
	held := s.heldCores(rr)
	if rec.pinned > held {
		s.log.warn("pinned job on a runner started without those cores", "job", rec.id, "runner", rr.name, "want", rec.pinned, "have", held)
	}
	req.PinnedCores = held
	return req
}

func ageKeyOf(id string, j core.Job) string {
	if j.RunID == 0 {
		return "id|" + id
	}
	return fmt.Sprintf("%d|%s", j.RunID, j.Name)
}

func isCanceled(res string) bool {
	r := strings.ToLower(res)
	return r == "canceled" || r == "cancelled"
}

func laneLess(a, b LaneKey) int {
	return cmp.Or(
		cmp.Compare(a.Target.Owner, b.Target.Owner),
		cmp.Compare(a.Target.Repo, b.Target.Repo),
		cmp.Compare(a.Target.RunnerGroup, b.Target.RunnerGroup),
		cmp.Compare(a.OS, b.OS),
		cmp.Compare(a.Lane, b.Lane),
	)
}

func (s *Scheduler) sortedLanes() []*laneRec {
	out := make([]*laneRec, 0, len(s.lanes))
	for _, l := range s.lanes {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b *laneRec) int { return laneLess(a.key, b.key) })
	return out
}

func (s *Scheduler) sortedRunners() []*runnerRec {
	out := make([]*runnerRec, 0, len(s.runners))
	for _, r := range s.runners {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *runnerRec) int { return cmp.Compare(a.seq, b.seq) })
	return out
}

// ---- queue helpers -------------------------------------------------------

func (s *Scheduler) enqueue(r *jobRec) {
	l := s.lane(r.lane)
	if r.inQueue {
		return
	}
	r.inQueue = true
	i := slices.IndexFunc(l.queue, func(o *jobRec) bool {
		return r.age.Before(o.age) || (r.age.Equal(o.age) && r.seq < o.seq)
	})
	if i < 0 {
		l.queue = append(l.queue, r)
	} else {
		l.queue = slices.Insert(l.queue, i, r)
	}
}

func (s *Scheduler) dequeue(r *jobRec) {
	if !r.inQueue {
		return
	}
	r.inQueue = false
	l := s.lane(r.lane)
	l.queue = slices.DeleteFunc(l.queue, func(o *jobRec) bool { return o == r })
}

// ---- job lookup ----------------------------------------------------------

func (s *Scheduler) findJob(j core.Job) *jobRec {
	if j.JobID != "" {
		if r := s.jobs[j.JobID]; r != nil {
			return r
		}
	}
	// An empty or unknown id matches the job of the runner named in the event;
	// unknown ids only for jobs whose own id we never knew.
	if j.RunnerName != "" {
		if rr := s.byName[j.RunnerName]; rr != nil && rr.job != nil && (j.JobID == "" || rr.job.synthetic) {
			s.adoptID(rr.job, j.JobID)
			return rr.job
		}
	}
	if j.RunID != 0 && j.JobID != "" {
		if r := s.byAge[ageKeyOf(j.JobID, j)]; r != nil {
			s.adoptID(r, j.JobID) // GitHub re-issued the job under a new id
			return r
		}
	}
	return nil
}

// adoptID re-keys a job under the id GitHub uses for it.
func (s *Scheduler) adoptID(r *jobRec, id string) {
	if id == "" || r.id == id || s.jobs[id] != nil {
		return
	}
	delete(s.jobs, r.id)
	r.id, r.synthetic = id, false
	r.job.JobID = id
	s.jobs[id] = r
}

// setJob refreshes a job's identity from an event; an event without an id
// only contributes the runner name.
func setJob(r *jobRec, j core.Job) {
	if j.JobID == "" {
		if j.RunnerName != "" {
			r.job.RunnerName = j.RunnerName
		}
		return
	}
	r.job = j
}

// applyRule sets the job's cost, CPU, pinned cores, devices and image from
// the first matching [[cost]] rule, or the defaults. A job with pinned cores
// runs on those and takes no shared CPU.
func (s *Scheduler) applyRule(r *jobRec) {
	j := r.job
	r.cost, r.cpu, r.pinned, r.devices, r.image, r.local, r.machine = s.o.DefaultJobMem, s.o.DefaultJobCPU, 0, nil, "", false, ""
	if rule, ok := admission.MatchRule(s.o.Costs, j.Owner, j.Repo, j.WorkflowRef, j.Name); ok {
		if rule.Mem > 0 {
			r.cost = rule.Mem
		}
		if rule.CPU > 0 {
			r.cpu = rule.CPU
		}
		r.pinned = rule.PinnedCores
		r.devices, r.image, r.local, r.machine = rule.Devices, rule.Image, rule.Local, rule.Machine
		if r.machine == "local" {
			r.local, r.machine = true, ""
		}
	}
	if r.pinned > 0 {
		r.cpu = 0
	}
	// Host devices and pinned cores are this machine's.
	r.local = r.local || r.pinned > 0 || len(r.devices) > 0
}

// updateJob takes newer job details and re-applies the cost rule when they
// change which rule matches: the first event for a job (a placeholder, or a
// Started after a restart) may lack its workflow or name. A runner already
// holding the job is resized to the new cost.
func (s *Scheduler) updateJob(r *jobRec, j core.Job) {
	key := func() string { return admission.JobKey(r.job.Owner, r.job.Repo, r.job.WorkflowRef, r.job.Name) }
	before := key()
	setJob(r, j)
	if key() == before {
		return
	}
	old, oldCPU := r.cost, r.cpu
	s.applyRule(r)
	if r.cost == old && r.cpu == oldCPU {
		return
	}
	s.log.info("job cost updated", "job", r.id, "from", old, "to", r.cost, "cpu_from", oldCPU, "cpu_to", r.cpu)
	if rr := r.rr; rr != nil && rr.host != "" && r.local {
		s.log.warn("local-only job is on a remote host", "job", r.id, "runner", rr.name, "host", rr.host)
	}
	if rr := r.rr; rr != nil && rr.full && rr.job == r {
		_, _ = s.led.ForceResize(rr.tok, charge(rr, s.reqFor(rr, r)))
	}
}

func (s *Scheduler) newJob(lk LaneKey, j core.Job) *jobRec {
	now := s.now()
	s.seq++
	id := j.JobID
	if id == "" { // never key on an empty id: restored jobs may not have one
		id = fmt.Sprintf("anon-%d", s.seq)
		if j.RunnerName != "" {
			id = "restored-" + j.RunnerName
			if s.jobs[id] != nil {
				id = fmt.Sprintf("%s-%d", id, s.seq)
			}
		}
	}
	r := &jobRec{id: id, job: j, lane: lk, ageKey: ageKeyOf(id, j), age: now, synthetic: j.JobID == ""}
	r.seq = s.seq
	s.applyRule(r)
	if a, ok := s.ages[r.ageKey]; ok {
		r.age = a.t
	}
	s.ages[r.ageKey] = ageEntry{t: r.age, seen: now}
	s.jobs[r.id] = r
	s.byAge[r.ageKey] = r
	return r
}

func (s *Scheduler) forget(r *jobRec) {
	s.dequeue(r)
	delete(s.jobs, r.id)
	if s.byAge[r.ageKey] == r {
		delete(s.byAge, r.ageKey)
	}
}

// ---- events --------------------------------------------------------------

// Assigned handles a JobAssigned message for a lane.
func (s *Scheduler) Assigned(key core.ScaleSetKey, lane int, j core.Job) {
	s.locked(func() { s.onAssigned(LaneKey{key, lane}, j) })
}

// Reassigned handles a job the service assigned again (reassign churn); it is
// idempotent with Assigned.
func (s *Scheduler) Reassigned(key core.ScaleSetKey, lane int, j core.Job) {
	s.locked(func() { s.onAssigned(LaneKey{key, lane}, j) })
}

func (s *Scheduler) onAssigned(lk LaneKey, j core.Job) {
	if s.isDone(j) {
		return
	}
	r := s.findJob(j)
	if r != nil {
		if r.phase == pBound {
			return // stale: already started
		}
		s.updateJob(r, j)
		if a, ok := s.ages[r.ageKey]; ok {
			s.ages[r.ageKey] = ageEntry{t: a.t, seen: s.now()}
		}
		if !r.unboundAt.IsZero() {
			r.unboundAt = time.Time{}
			s.log.info("job reassigned", "job", r.id, "lane", lk.Lane)
		}
		if lk != r.lane { // churn moved it to another lane
			s.dequeue(r)
			if rr := r.rr; rr != nil { // its runner would wait in the old lane: spare there
				r.rr, rr.job = nil, nil
				s.shrink(rr)
				r.phase = pHeld
				s.log.info("job moved lane, runner detached", "job", r.id, "from", r.lane.Lane, "to", lk.Lane)
			}
			r.lane = lk
		}
		s.enqueue(r) // no-op when still queued
		return
	}
	r = s.newJob(lk, j)
	s.mergePhantom(r)
	s.enqueue(r)
	s.log.info("job assigned", "job", r.id, "run", j.RunID, "cost", r.cost, "cpu", r.cpu, "pinned", r.pinned, "lane", lk.Lane)
}

// Started handles a JobStarted message: the job is bound to j.RunnerName.
func (s *Scheduler) Started(key core.ScaleSetKey, lane int, j core.Job) {
	s.locked(func() { s.onStarted(LaneKey{key, lane}, j) })
}

func (s *Scheduler) onStarted(lk LaneKey, j core.Job) {
	if s.isDone(j) {
		s.log.info("late started event ignored", "job", j.JobID, "runner", j.RunnerName)
		return
	}
	rec := s.findJob(j)
	if rec == nil {
		rec = s.newJob(lk, j)
	}
	s.lane(lk).changedAt = s.now()
	s.updateJob(rec, j)
	rec.lane = lk
	name := j.RunnerName
	var rr *runnerRec
	if name != "" {
		rr = s.byName[name]
	}
	if rr == nil && rec.rr != nil && rec.rr.name == "" { // Started beat StartRunner's return
		rr = rec.rr
		rr.name = name
		s.byName[name] = rr
	}
	if rr != nil && rec.phase == pBound && rec.rr == rr {
		return // duplicate
	}
	if rr == nil { // a runner we did not start (restart, adoption)
		s.seq++
		rr = &runnerRec{tok: fmt.Sprintf("r%d", s.seq), seq: s.seq, name: name, lane: lk, createdAt: s.now(), full: true}
		s.led.Force(rr.tok, s.fullReq(rec)) // cpuset unknown: only the count is tracked
		s.runners[rr.tok] = rr
		if name != "" {
			s.byName[name] = rr
		}
	}
	if old := rr.job; old != nil && old != rec { // the runner took another job than we planned
		old.rr = nil
		if old.phantom || (old.synthetic && old.phase == pBound) {
			s.forget(old) // the real job took the placeholder's runner
		} else if old.phase == pStarting {
			old.phase = pHeld
		}
	}
	if o := rec.rr; o != nil && o != rr { // our planned runner for this job is now spare
		o.job = nil
		s.shrink(o)
	}
	s.dequeue(rec)
	rec.rr, rr.job = rr, rec
	rec.phase = pBound
	rec.unboundAt = time.Time{}
	rec.startedAt = s.now()
	if rr.full {
		_, _ = s.led.ForceResize(rr.tok, charge(rr, s.reqFor(rr, rec)))
	} else {
		rr.parked = true // waits in the hook until the ledger grants the rest
	}
	s.log.info("job started", "job", rec.id, "runner", name, "parked", rr.parked)
}

// shrink drops a runner's reservation to idle overhead.
func (s *Scheduler) shrink(rr *runnerRec) {
	req := s.overhead()
	req.PinnedCores = s.heldCores(rr) // the cpuset stays with the container
	_, _ = s.led.ForceResize(rr.tok, charge(rr, req))
	rr.full, rr.parked, rr.hookOK = false, false, false
}

// Completed handles a JobCompleted message.
func (s *Scheduler) Completed(key core.ScaleSetKey, lane int, j core.Job) {
	s.locked(func() { s.onCompleted(LaneKey{key, lane}, j) })
}

func (s *Scheduler) onCompleted(lk LaneKey, j core.Job) {
	rec := s.findJob(j)
	if j.RunnerName == "" && isCanceled(j.Result) && (rec == nil || rec.phase != pBound) {
		if rec == nil {
			return
		}
		// Reassign churn (or a user cancel): keep age and any runner we already
		// started; if no Assigned follows, Tick drops it.
		if rec.unboundAt.IsZero() {
			rec.unboundAt = s.now()
		}
		s.lane(lk).changedAt = s.now()
		if rec.phase == pHeld {
			s.dequeue(rec)
		}
		s.log.info("job unbound", "job", rec.id, "lane", lk.Lane)
		return
	}
	if rec == nil {
		if rr := s.byName[j.RunnerName]; rr != nil && j.RunnerName != "" {
			s.releaseRunner(rr)
		}
		return
	}
	s.finish(rec, j)
}

func (s *Scheduler) finish(rec *jobRec, j core.Job) {
	now := s.now()
	rr := rec.rr
	if rr == nil && j.RunnerName != "" {
		rr = s.byName[j.RunnerName]
		if rr != nil && rr.job != nil && rr.job != rec {
			rr = nil
		}
	}
	s.forget(rec)
	s.lane(rec.lane).changedAt = now
	delete(s.ages, rec.ageKey)
	for _, id := range []string{rec.job.JobID, j.JobID} {
		if id != "" {
			s.done[id] = now
		}
	}
	runner := j.RunnerName
	if rr != nil {
		if runner == "" {
			runner = rr.name
		}
		s.releaseRunner(rr)
	}
	if rec.rerun && j.Result != "succeeded" {
		k := fmt.Sprintf("%d|%s", rec.job.RunID, rec.job.Name)
		if !s.reran[k] {
			s.reran[k] = true
			s.reruns = append(s.reruns, rerunReq{owner: rec.job.Owner, repo: rec.job.Repo, runID: rec.job.RunID, job: rec.job.Name, due: now.Add(s.o.RerunDelay)})
		}
	}
	if o := s.o.Observer; o != nil {
		if rec.job.JobID == "" {
			rec.job.JobID = j.JobID
		}
		res := JobResult{Job: rec.job, Lane: rec.lane, Runner: runner, Cost: rec.cost, Result: j.Result, Started: rec.startedAt, Finished: now}
		res.Job.Result = j.Result
		s.effect(func(context.Context) { o.JobFinished(res) })
	}
	s.log.info("job finished", "job", rec.id, "runner", runner)
}

// isDone reports a job that already finished (late or redelivered events).
func (s *Scheduler) isDone(j core.Job) bool {
	_, ok := s.done[j.JobID]
	return ok && j.JobID != ""
}

// releaseRunner frees the reservation now and schedules the reap.
func (s *Scheduler) releaseRunner(rr *runnerRec) {
	name := rr.name
	s.dropRunner(rr)
	if name != "" {
		s.reaps[name] = s.now().Add(s.o.ReapGrace)
	}
}

func (s *Scheduler) dropRunner(rr *runnerRec) {
	s.led.Release(rr.tok)
	delete(s.runners, rr.tok)
	if rr.name != "" && s.byName[rr.name] == rr {
		delete(s.byName, rr.name)
	}
	rr.released = true
	if rr.vmKey != "" { // the VM never came up (or is gone): its reservation goes too
		s.led.Release(rr.vmKey)
		rr.vmKey = ""
	}
	if rr.job != nil && rr.job.rr == rr {
		rr.job.rr = nil
	}
	rr.job = nil
}

// Stats records the service's view of a lane; a surplus of GitHub-assigned
// unstarted jobs over the ones we know creates placeholder jobs (phantoms).
func (s *Scheduler) Stats(key core.ScaleSetKey, lane int, st LaneStats) {
	s.locked(func() {
		l := s.lane(LaneKey{key, lane})
		l.statsInitial = !l.hasStats
		l.hasStats, l.statsAt, l.stats = true, s.now(), st
	})
}

// RunnerDied reports a runner that is no longer alive. oom says the kernel
// killed it for memory (no rerun then). The runner is always reaped (stopped:
// container, dirs, registration, VM): at once when it had no job or its job is
// done, after ReapGrace when it died mid-job. The run is not cancelled: GitHub
// fails the lost job by itself, and only that job is rerun afterwards.
func (s *Scheduler) RunnerDied(name string, oom bool) {
	s.locked(func() {
		rr := s.byName[name]
		if rr == nil { // already released (job completed) or never known
			s.reapNow(name)
			return
		}
		rec := rr.job
		s.dropRunner(rr)
		switch {
		case rec == nil:
			s.reapNow(name)
		case rec.phase != pBound: // died before taking the job: plan a new runner
			if rec.phase == pStarting {
				rec.phase = pHeld
				rec.notBefore = s.now().Add(s.o.StartRetry)
			}
			s.reapNow(name)
		case rr.jobDone:
			s.reapNow(name) // normal exit after the job finished
		default:
			if !oom && s.o.RerunOnLoss {
				rec.rerun = true // rerun just this job once its Completed arrives
			}
			s.log.warn("runner died mid-job", "runner", name, "job", rec.id, "oom", oom)
			s.reaps[name] = s.now().Add(s.o.ReapGrace)
		}
	})
}

// reapNow stops the runner at once, replacing any pending delayed reap.
func (s *Scheduler) reapNow(name string) {
	delete(s.reaps, name)
	s.stop(name)
}

// DropRun forgets every not-yet-started job of a workflow run that has
// finished: a job cancelled while queued (or missed across a restart) sends
// no JobCompleted, and its record would keep asking for runners. A runner
// already started for one becomes spare. Returns how many were dropped.
func (s *Scheduler) DropRun(owner, repo string, runID int64) int {
	n := 0
	s.locked(func() {
		for _, rec := range s.sortedJobs() {
			j := rec.job
			if j.RunID != runID || rec.phase == pBound || !strings.EqualFold(j.Owner, owner) || !strings.EqualFold(j.Repo, repo) {
				continue
			}
			s.forget(rec)
			if rr := rec.rr; rr != nil {
				rr.job, rec.rr = nil, nil
				s.shrink(rr)
			}
			if j.JobID != "" {
				s.done[j.JobID] = s.now() // a late Assigned must not revive it
			}
			s.log.info("job of a finished run dropped", "job", rec.id, "run", runID)
			n++
		}
	})
	return n
}

// Drain stops new work: capacity becomes started (no spare slot) and no new
// runners are started; held jobs stay queued.
func (s *Scheduler) Drain(on bool) {
	s.locked(func() { s.drain = on })
}

// Draining reports the drain flag.
func (s *Scheduler) Draining() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drain
}

// SetExternalReserved changes memory reserved outside the ledger.
func (s *Scheduler) SetExternalReserved(n int64) {
	s.locked(func() { s.led.SetExternal(n) })
}

// Restore re-registers runners found at startup so their memory counts.
func (s *Scheduler) Restore(rs []RestoredRunner) {
	s.locked(func() {
		for _, r := range rs {
			if s.byName[r.Name] != nil {
				continue
			}
			s.lane(r.Lane)
			s.seq++
			rr := &runnerRec{tok: fmt.Sprintf("r%d", s.seq), seq: s.seq, name: r.Name, lane: r.Lane, createdAt: r.Created, host: r.Host, remote: r.Host != "" || r.Remote}
			if rr.createdAt.IsZero() {
				rr.createdAt = s.now()
			}
			s.runners[rr.tok] = rr
			s.byName[rr.name] = rr
			if r.Job == nil && !r.Started {
				s.led.Force(rr.tok, charge(rr, s.overhead()))
				continue
			}
			var jb core.Job
			if r.Job != nil {
				jb = *r.Job
			} else {
				// GitHub reports it busy but we never saw its job (it started
				// while we were down): bind an anonymous job so NeverJob does not
				// recycle it; Completed matches it by runner name.
				jb = core.Job{Owner: r.Lane.Target.Owner, Repo: r.Lane.Target.Repo, Name: "(unknown)", ScaleSet: r.Lane.ScaleSetKey}
			}
			if jb.RunnerName == "" {
				jb.RunnerName = r.Name
			}
			var rec *jobRec
			if jb.JobID != "" { // the store may not have kept the id
				rec = s.findJob(jb)
			}
			if rec == nil {
				rec = s.newJob(r.Lane, jb)
			}
			rec.rr, rr.job = rr, rec
			rr.full = true
			s.led.Force(rr.tok, charge(rr, s.fullReq(rec)))
			if r.Started {
				rec.phase = pBound
				rec.startedAt = rr.createdAt
				rr.hookOK = true
			} else {
				rec.phase = pStarting
				s.enqueue(rec)
			}
		}
	})
}

// HookCompleted records the runner's job-completed hook: a later exit of that
// runner is normal, not a loss. It reports whether the runner is known.
func (s *Scheduler) HookCompleted(req HookRequest) bool {
	known := false
	s.locked(func() {
		if rr := s.lookup(req); rr != nil {
			if rr.job == nil { // its job started while we were down
				rec := s.bindFromHook(rr, req)
				_, _ = s.led.ForceResize(rr.tok, charge(rr, s.reqFor(rr, rec)))
				rr.full, rr.hookOK = true, true
			}
			rr.jobDone = true
			known = true
		}
	})
	return known
}

// ---- hook ----------------------------------------------------------------

func (s *Scheduler) lookup(req HookRequest) *runnerRec {
	if req.Runner != "" {
		return s.byName[req.Runner]
	}
	for _, rr := range s.sortedRunners() {
		if rr.job != nil && rr.job.job.RunID == req.RunID && req.RunID != 0 &&
			strings.EqualFold(rr.job.job.Owner+"/"+rr.job.job.Repo, req.Repository) {
			return rr
		}
	}
	return nil
}

// bindFromHook gives a runner that has no job (its job started while the
// daemon was down) one built from the hook request, costed from the rules as
// far as the request identifies it, and marks it started. GITHUB_WORKFLOW is
// a name rather than a file, so a rule only matches it by pattern.
func (s *Scheduler) bindFromHook(rr *runnerRec, req HookRequest) *jobRec {
	j := core.Job{
		RunID: req.RunID, Owner: rr.lane.Target.Owner, Repo: rr.lane.Target.Repo,
		Name: req.JobKey, WorkflowRef: req.Workflow, RunnerName: rr.name, ScaleSet: rr.lane.ScaleSetKey,
	}
	if o, r, ok := strings.Cut(req.Repository, "/"); ok && o != "" && r != "" {
		j.Owner, j.Repo = o, r
	}
	rec := s.newJob(rr.lane, j)
	rec.rr, rr.job = rr, rec
	rec.phase = pBound
	rec.startedAt = s.now()
	rr.full, rr.parked = false, false
	s.lane(rr.lane).changedAt = s.now()
	s.log.info("job bound from hook", "job", rec.id, "runner", rr.name, "run", req.RunID, "cost", rec.cost)
	return rec
}

// Hook is the job-started hook: it returns once the runner's job holds its
// full reservation, or with a deny verdict after AdmissionTimeout.
func (s *Scheduler) Hook(ctx context.Context, req HookRequest) (HookResponse, error) {
	var w *hookWaiter
	s.locked(func() {
		now := s.now()
		if rr := s.lookup(req); rr != nil && rr.job == nil {
			s.bindFromHook(rr, req).rr.parked = true // waits for its memory like any started job
		}
		w = &hookWaiter{req: req, since: now, deadline: now.Add(s.o.AdmissionTimeout), ch: make(chan HookResponse, 1)}
		s.waiters = append(s.waiters, w)
	})
	select {
	case r := <-w.ch:
		return r, nil
	case <-ctx.Done():
		s.locked(func() {
			w.done = true
			s.waiters = slices.DeleteFunc(s.waiters, func(o *hookWaiter) bool { return o == w })
		})
		return HookResponse{}, ctx.Err()
	}
}

func (s *Scheduler) pumpWaiters() {
	now := s.now()
	keep := s.waiters[:0]
	for _, w := range s.waiters {
		if w.done {
			continue
		}
		resp, ok := HookResponse{Waited: now.Sub(w.since)}, true
		rr := s.lookup(w.req)
		switch {
		case rr != nil && rr.full:
			resp.Allow, resp.Reason = true, ReasonGranted
			rr.hookOK = true
		case !now.Before(w.deadline):
			resp.Reason = ReasonAdmissionTimeout
			s.log.warn("admission timeout", "runner", w.req.Runner, "run", w.req.RunID)
		case rr == nil && now.Sub(w.since) >= s.o.UnknownRunnerGrace:
			resp.Allow, resp.Reason = true, ReasonUnknownRunner
			s.log.warn("hook from unknown runner", "runner", w.req.Runner, "run", w.req.RunID)
		default:
			ok = false
		}
		if !ok {
			keep = append(keep, w)
			continue
		}
		w.done = true
		w.ch <- resp
	}
	clear(s.waiters[len(keep):])
	s.waiters = keep
}

// ---- tick ----------------------------------------------------------------

// Tick runs the time-driven work: VM sync, never-job recycle, unbound drop,
// reaps, reruns, hook deadlines, age pruning, and a scheduling pass.
func (s *Scheduler) Tick() {
	var vms map[string]int64
	if s.o.VMs != nil {
		vms = s.o.VMs.VMsUp()
	}
	s.locked(func() {
		now := s.now()
		if s.o.VMs != nil {
			s.syncVMs(vms)
		}
		for _, rr := range s.sortedRunners() {
			if rr.job != nil && rr.job.phase == pBound {
				continue
			}
			if now.Sub(rr.createdAt) < s.o.NeverJob {
				continue
			}
			s.log.warn("runner never got a job", "runner", rr.name)
			rec := rr.job
			name := rr.name
			s.dropRunner(rr)
			if name != "" {
				s.stop(name)
			}
			if rec != nil && rec.phase == pStarting {
				rec.phase = pHeld
				rec.notBefore = now.Add(s.o.StartRetry)
			}
		}
		for _, rec := range s.sortedJobs() {
			if !rec.unboundAt.IsZero() && now.Sub(rec.unboundAt) >= s.o.UnboundDrop {
				s.forget(rec)
				if rr := rec.rr; rr != nil {
					rr.job, rec.rr = nil, nil
					s.shrink(rr)
				}
				s.log.info("unbound job dropped", "job", rec.id)
			}
		}
		for id, at := range s.done {
			if now.Sub(at) > s.o.AgeTTL {
				delete(s.done, id)
			}
		}
		for name, at := range s.stopped {
			if now.Sub(at) > s.o.AgeTTL {
				delete(s.stopped, name)
			}
		}
		for name, due := range s.reaps {
			if !now.Before(due) {
				delete(s.reaps, name)
				s.stop(name)
			}
		}
		due := s.reruns[:0]
		var rest []rerunReq
		for _, r := range s.reruns {
			if now.Before(r.due) {
				rest = append(rest, r)
			} else {
				due = append(due, r)
			}
		}
		due = slices.Clone(due)
		s.reruns = rest
		for _, r := range due {
			s.effect(func(ctx context.Context) {
				if err := s.o.Runners.RerunJob(ctx, r.owner, r.repo, r.runID, r.job); err != nil {
					s.log.warn("rerun failed", "run", r.runID, "err", err.Error())
					if r.tries < 2 {
						s.locked(func() {
							r.tries++
							r.due = s.now().Add(s.o.RerunDelay)
							s.reruns = append(s.reruns, r)
						})
					}
				}
			})
		}
		for k, a := range s.ages {
			if s.byAge[k] == nil && now.Sub(a.seen) > s.o.AgeTTL {
				delete(s.ages, k)
			}
		}
	})
}

func (s *Scheduler) sortedJobs() []*jobRec {
	out := make([]*jobRec, 0, len(s.jobs))
	for _, r := range s.jobs {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *jobRec) int { return cmp.Compare(a.seq, b.seq) })
	return out
}

// stop sends StopRunner once per runner, however many paths ask for it.
func (s *Scheduler) stop(name string) {
	if _, ok := s.stopped[name]; ok {
		return
	}
	s.stopped[name] = s.now()
	s.effect(func(ctx context.Context) {
		if err := s.o.Runners.StopRunner(ctx, name); err != nil {
			s.log.warn("stop runner failed", "runner", name, "err", err.Error())
		}
	})
}

func (s *Scheduler) syncVMs(up map[string]int64) {
	for name, mem := range up {
		key := "vm:" + name
		r, ok := s.led.Get(key)
		if !ok {
			s.convertPendingVM(mem) // a grant already reserved this VM's memory
		}
		if !ok || r.Mem != mem {
			s.led.Force(key, admission.Request{Mem: mem})
		}
	}
	for _, key := range s.led.Keys() {
		if name, ok := strings.CutPrefix(key, "vm:"); ok && !strings.HasPrefix(name, pendingVM) {
			if _, still := up[name]; !still {
				s.led.Release(key)
			}
		}
	}
}

const pendingVM = "pending:"

// convertPendingVM drops the pending reservation a newly seen VM replaces:
// the oldest one of the same size, else the oldest.
func (s *Scheduler) convertPendingVM(mem int64) {
	var pick *runnerRec
	for _, rr := range s.sortedRunners() {
		if rr.vmKey == "" {
			continue
		}
		if r, ok := s.led.Get(rr.vmKey); ok && r.Mem == mem {
			pick = rr
			break
		}
		if pick == nil {
			pick = rr
		}
	}
	if pick != nil {
		s.led.Release(pick.vmKey)
		pick.vmKey = ""
	}
}

// ---- admission -----------------------------------------------------------

type cand struct {
	rec    *jobRec
	w      admission.Waiter
	parked bool
}

func (s *Scheduler) waiter(r *jobRec) admission.Waiter {
	return admission.Waiter{ID: r.id, Group: r.lane.Target.String(), Cost: r.cost, Since: r.age, Seq: r.seq}
}

func (s *Scheduler) weight(group string) int {
	for t, o := range s.o.Targets {
		if t.String() == group && o.Weight > 0 {
			return o.Weight
		}
	}
	return 1
}

// heads returns each lane's head job that is held and may be granted.
func (s *Scheduler) heads() []*jobRec {
	var out []*jobRec
	for _, l := range s.sortedLanes() {
		if len(l.queue) > 0 && l.queue[0].phase == pHeld && l.queue[0].unboundAt.IsZero() {
			out = append(out, l.queue[0])
		}
	}
	return out
}

func (s *Scheduler) candidates() []cand {
	now := s.now()
	var out []cand
	for _, r := range s.heads() {
		if !now.Before(r.notBefore) {
			out = append(out, cand{rec: r, w: s.waiter(r)})
		}
	}
	for _, rr := range s.sortedRunners() {
		if rr.parked && !rr.full && rr.job != nil {
			out = append(out, cand{rec: rr.job, w: s.waiter(rr.job), parked: true})
		}
	}
	return out
}

// oldestWaiter is the oldest job (held or parked) that could ever fit.
func (s *Scheduler) oldestWaiter() *admission.Waiter {
	var ws []admission.Waiter
	for _, r := range s.heads() {
		// A job only hosts can run never waits for this host's memory.
		if s.fitsEver(r) && !s.o.HostsOnly[r.lane.OS] && !s.pinnedRemote(r) {
			ws = append(ws, s.waiter(r))
		}
	}
	for _, rr := range s.sortedRunners() {
		if rr.parked && !rr.full && rr.job != nil && s.fitsEver(rr.job) {
			ws = append(ws, s.waiter(rr.job))
		}
	}
	if i := admission.Oldest(ws); i >= 0 {
		return &ws[i]
	}
	return nil
}

// fitsEver is FitsEver for the job including the VM it may need.
func (s *Scheduler) fitsEver(r *jobRec) bool {
	req := s.fullReq(r)
	if s.vmManaged(r.lane) {
		vm, _ := s.o.VMCost(r.lane)
		req.Mem += vm
	}
	return s.led.FitsEver(req)
}

func (s *Scheduler) granted() map[string]int64 {
	g := map[string]int64{}
	for _, rr := range s.runners {
		if r, ok := s.led.Get(rr.tok); ok {
			g[rr.lane.Target.String()] += r.Mem
		}
	}
	return g
}

func (s *Scheduler) policy() admission.Policy {
	return admission.Policy{ReserveAfter: s.o.ReserveAfter, LightThreshold: s.o.LightThreshold}
}

// settle runs after every state change.
func (s *Scheduler) settle() {
	s.reconcilePhantoms()
	s.grantLoop()
	s.promote()
	s.pumpWaiters()
	s.pushCapacity()
}

func (s *Scheduler) grantLoop() {
	if s.drain {
		return
	}
	for {
		cs := s.candidates()
		if len(cs) == 0 {
			return
		}
		ws := make([]admission.Waiter, len(cs))
		byID := make(map[string]cand, len(cs))
		for i, c := range cs {
			ws[i] = c.w
			byID[c.w.ID] = c
		}
		admission.SortFair(ws, s.granted(), s.weight)
		oldest := s.oldestWaiter()
		progressed := false
		for _, w := range ws {
			if s.tryGrant(byID[w.ID], oldest) {
				progressed = true
				break
			}
		}
		if !progressed {
			return
		}
	}
}

// liveCounts counts runners: all on this host (max_runners is a host limit),
// and the target's on every machine.
func (s *Scheduler) liveCounts(t core.Target) (all, target int) {
	for _, rr := range s.runners {
		if rr.host == "" {
			all++
		}
		if rr.lane.Target == t {
			target++
		}
	}
	return
}

func (s *Scheduler) tryGrant(c cand, oldest *admission.Waiter) bool {
	rec := c.rec
	// The oldest waiter blocks only what competes with it for this host.
	blocked := oldest != nil && s.policy().Blocks(*oldest, c.w, s.now())
	if c.parked && blocked {
		return false
	}
	if c.parked {
		if _, err := s.led.Resize(rec.rr.tok, s.reqFor(rec.rr, rec)); err != nil {
			return false
		}
		rec.rr.full, rec.rr.parked = true, false
		s.log.info("parked job granted", "job", rec.id, "runner", rec.rr.name)
		return true
	}
	local, target := s.canStartRunner(rec.lane.Target)
	if !target {
		return false
	}
	s.seq++
	tok := fmt.Sprintf("r%d", s.seq)
	if rec.machine != "" {
		return s.grantPinned(rec, tok, blocked)
	}
	if s.remoteVM(rec.lane) { // takes nothing here, like a Linux host
		s.led.Force(tok, admission.Request{})
		s.startRunner(rec, tok, s.seq, admission.Reservation{}, false, "", "")
		s.runners[tok].remote = true
		return true
	}
	if local && !blocked && !s.o.HostsOnly[rec.lane.OS] {
		if res, vmKey, ok := s.reserveRunner(rec, tok, s.fullReq(rec)); ok {
			s.startRunner(rec, tok, s.seq, res, false, vmKey, "")
			return true
		}
	}
	host := ""
	if !rec.local {
		host = s.freeHost(rec.lane)
	}
	if host == "" {
		return false
	}
	s.led.Force(tok, admission.Request{})
	s.startRunner(rec, tok, s.seq, admission.Reservation{}, false, "", host)
	return true
}

// grantPinned starts a runner for a job pinned to one machine, or leaves it
// waiting until that machine can take it.
// A blocked job may still take a remote machine: it competes with nothing
// here.
func (s *Scheduler) grantPinned(rec *jobRec, tok string, blocked bool) bool {
	if s.vmManaged(rec.lane) {
		if s.o.VMNamed == nil {
			return false
		}
		mem, up, remote, idle := s.o.VMNamed(rec.machine)
		if !idle {
			return false
		}
		if remote {
			s.led.Force(tok, admission.Request{})
			s.startRunner(rec, tok, s.seq, admission.Reservation{}, false, "", "")
			s.runners[tok].remote = true
			return true
		}
		if blocked {
			return false
		}
		vm := int64(0)
		if !up {
			vm = mem
		}
		res, vmKey, ok := s.reserveWithVM(rec, tok, s.fullReq(rec), vm)
		if !ok {
			return false
		}
		s.startRunner(rec, tok, s.seq, res, false, vmKey, "")
		return true
	}
	if !s.hostOK(rec.machine, rec.lane) {
		return false
	}
	s.led.Force(tok, admission.Request{})
	s.startRunner(rec, tok, s.seq, admission.Reservation{}, false, "", rec.machine)
	return true
}

// pinnedRemote reports a job pinned to another machine.
func (s *Scheduler) pinnedRemote(r *jobRec) bool {
	if r.machine == "" {
		return false
	}
	if !s.vmManaged(r.lane) {
		return true
	}
	if s.o.VMNamed == nil {
		return false
	}
	_, _, remote, _ := s.o.VMNamed(r.machine)
	return remote
}

// hostOK reports whether remote host name serves lk and has a free slot now.
func (s *Scheduler) hostOK(name string, lk LaneKey) bool {
	for _, h := range s.o.LinuxHosts {
		if h.Name == name {
			return s.hostFree(h, lk)
		}
	}
	return false
}

// freeHost is the first reachable remote host with a free slot serving the
// lane's target, or "".
func (s *Scheduler) freeHost(lk LaneKey) string {
	for _, h := range s.o.LinuxHosts {
		if s.hostFree(h, lk) {
			return h.Name
		}
	}
	return ""
}

// hostFree reports whether h runs lk's OS and target, is reachable and has a
// free slot.
func (s *Scheduler) hostFree(h RemoteHost, lk LaneKey) bool {
	os := h.OS
	if os == "" {
		os = core.Linux
	}
	if os != lk.OS || (h.Serves != nil && !h.Serves(lk.Target)) {
		return false
	}
	used := 0
	for _, rr := range s.runners {
		if rr.host == h.Name {
			used++
		}
	}
	if used >= h.Slots {
		return false
	}
	return s.o.HostUp == nil || s.o.HostUp(h.Name)
}

// reserveRunner reserves tok and, on a VM-managed lane with no suitable VM up,
// the memory of the VM that StartRunner is about to boot, or nothing.
func (s *Scheduler) reserveRunner(rec *jobRec, tok string, req admission.Request) (admission.Reservation, string, bool) {
	return s.reserveWithVM(rec, tok, req, s.vmNeed(rec.lane))
}

// reserveWithVM reserves tok and, when vm > 0, the memory of the VM that
// StartRunner is about to boot.
func (s *Scheduler) reserveWithVM(rec *jobRec, tok string, req admission.Request, vm int64) (admission.Reservation, string, bool) {
	withVM := req
	withVM.Mem += vm
	if !s.led.CanFit(withVM) {
		return admission.Reservation{}, "", false
	}
	res, err := s.led.Reserve(tok, req)
	if err != nil {
		return admission.Reservation{}, "", false
	}
	if vm == 0 {
		return res, "", true
	}
	key := "vm:" + pendingVM + rec.id
	if _, err := s.led.Reserve(key, admission.Request{Mem: vm}); err != nil {
		s.led.Release(tok)
		return admission.Reservation{}, "", false
	}
	return res, key, true
}

// canStartRunner reports room for a runner on this host (max_runners) and
// for the target on any machine (its max).
func (s *Scheduler) canStartRunner(t core.Target) (local, target bool) {
	all, n := s.liveCounts(t)
	if m := s.o.Targets[t].Max; m > 0 && n >= m {
		return false, false
	}
	return s.o.MaxRunners <= 0 || all < s.o.MaxRunners, true
}

func (s *Scheduler) startRunner(rec *jobRec, tok string, seq int, res admission.Reservation, parked bool, vmKey, host string) {
	rr := &runnerRec{tok: tok, seq: seq, lane: rec.lane, full: !parked, parked: parked, job: rec, createdAt: s.now(), vmKey: vmKey, host: host, remote: host != ""}
	s.runners[tok] = rr
	rec.rr, rec.phase = rr, pStarting
	g := Grant{Key: tok, MemBytes: res.Mem, CPUSet: res.CPUSet(), Parked: parked, Job: rec.job, Devices: rec.devices, Image: rec.image, Host: host}
	if s.vmManaged(rec.lane) {
		g.Machine = rec.machine
	}
	lk := rec.lane
	s.log.info("starting runner", "job", rec.id, "lane", lk.Lane, "mem", res.Mem, "parked", parked, "host", host)
	s.effect(func(ctx context.Context) {
		name, err := s.o.Runners.StartRunner(ctx, lk, g)
		s.locked(func() {
			if rr.released {
				if err == nil {
					s.stop(name)
				}
				return
			}
			if err != nil {
				s.log.warn("start runner failed", "job", rec.id, "err", err.Error())
				s.dropRunner(rr)
				if rec.rr == nil && rec.phase == pStarting {
					rec.phase = pHeld
					rec.notBefore = s.now().Add(s.o.StartRetry)
				}
				return
			}
			rr.name = name
			s.byName[name] = rr
		})
	})
}

// promote starts one parked runner when a lane group is head-of-line blocked:
// every lane of a scale set has a head held past HolGrace, or any head is held
// past HoldMax.
func (s *Scheduler) promote() {
	if s.drain {
		return
	}
	parked := 0
	for _, rr := range s.runners {
		if rr.parked && !rr.full {
			parked++
		}
	}
	if parked >= s.o.MaxParked {
		return
	}
	now := s.now()
	groups := map[core.ScaleSetKey][]*laneRec{}
	var order []core.ScaleSetKey
	for _, l := range s.sortedLanes() {
		if _, ok := groups[l.key.ScaleSetKey]; !ok {
			order = append(order, l.key.ScaleSetKey)
		}
		groups[l.key.ScaleSetKey] = append(groups[l.key.ScaleSetKey], l)
	}
	for _, k := range order {
		var cs []*jobRec
		all := true
		for _, l := range groups[k] {
			h := (*jobRec)(nil)
			// A head backing off after a failed start (notBefore) counts as not
			// waiting: promoting it again would retry the start in a hot loop.
			if len(l.queue) > 0 && l.queue[0].phase == pHeld && l.queue[0].unboundAt.IsZero() && !now.Before(l.queue[0].notBefore) {
				h = l.queue[0]
			}
			switch {
			case h == nil:
				all = false
			case now.Sub(h.age) <= s.o.HolGrace:
				all = false
			default:
				cs = append(cs, h)
			}
		}
		if !all {
			cs = cs[:0]
			for _, l := range groups[k] {
				if len(l.queue) > 0 {
					if h := l.queue[0]; h.phase == pHeld && h.unboundAt.IsZero() && !now.Before(h.notBefore) && now.Sub(h.age) > s.o.HoldMax {
						cs = append(cs, h)
					}
				}
			}
		}
		if len(cs) == 0 {
			continue
		}
		slices.SortFunc(cs, func(a, b *jobRec) int {
			return cmp.Or(
				-cmp.Compare(s.prefers(a), s.prefers(b)),
				cmp.Compare(a.cost, b.cost),
				a.age.Compare(b.age),
				cmp.Compare(a.seq, b.seq),
			)
		})
		for _, rec := range cs {
			if local, _ := s.canStartRunner(rec.lane.Target); !local || s.o.HostsOnly[rec.lane.OS] || rec.machine != "" {
				continue
			}
			s.seq++
			tok := fmt.Sprintf("r%d", s.seq)
			// Pinned cores are fixed when the runner starts, so a parked runner
			// takes the job's cores now or is not promoted.
			req := s.overhead()
			req.PinnedCores = rec.pinned
			res, vmKey, ok := s.reserveRunner(rec, tok, req)
			if !ok {
				continue
			}
			s.log.info("promoting held job to parked runner", "job", rec.id, "lane", rec.lane.Lane)
			s.startRunner(rec, tok, s.seq, res, true, vmKey, "")
			return
		}
	}
}

// prefers is 1 when the job's expected wait (time already waited) is at most
// 25% of its timeout, which makes it cheap to park.
func (s *Scheduler) prefers(r *jobRec) int {
	if s.o.JobTimeout == nil {
		return 0
	}
	if to := s.o.JobTimeout(r.job); to > 0 && s.now().Sub(r.age)*4 <= to {
		return 1
	}
	return 0
}

func (s *Scheduler) pushCapacity() {
	if s.o.Capacity == nil {
		return
	}
	started := map[LaneKey]int{}
	for _, r := range s.jobs {
		if r.phase == pBound {
			started[r.lane]++
		}
	}
	for _, l := range s.sortedLanes() {
		n := started[l.key]
		if !s.drain {
			n++
		}
		if !l.capSet || l.cap != n {
			l.capSet, l.cap = true, n
			s.o.Capacity.SetCapacity(l.key, n)
		}
	}
}

// ---- status --------------------------------------------------------------

func (s *Scheduler) jobState(r *jobRec) JobState {
	switch {
	case !r.unboundAt.IsZero():
		return StateUnbound
	case r.phase == pHeld:
		return StateHeld
	case r.phase == pStarting:
		return StateStarting
	case r.rr != nil && r.rr.parked && !r.rr.full:
		return StateParked
	case r.rr != nil && r.rr.hookOK:
		return StateRunning
	}
	return StateStarted
}

// Snapshot returns the current state for the status command.
func (s *Scheduler) Snapshot() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	st := Status{Draining: s.drain, Ledger: s.led.Usage(), Runners: len(s.runners)}
	for _, rr := range s.runners {
		if rr.parked && !rr.full {
			st.Parked++
		}
	}
	byLane := map[LaneKey][]*jobRec{}
	for _, r := range s.sortedJobs() {
		byLane[r.lane] = append(byLane[r.lane], r)
		if r.phase == pHeld && r.unboundAt.IsZero() {
			st.Held++
		}
	}
	for _, l := range s.sortedLanes() {
		ls := LaneStatus{Lane: l.key, Name: laneName(l.key), Capacity: l.cap, Stats: l.stats}
		for _, r := range byLane[l.key] {
			if r.phase == pBound {
				ls.Started++
			}
			js := JobStatus{
				JobID: r.id, Owner: r.job.Owner, Repo: r.job.Repo, Workflow: admission.WorkflowFile(r.job.WorkflowRef),
				Name: r.job.Name, RunID: r.job.RunID, State: s.jobState(r), Cost: r.cost, CPU: r.cpu, Pinned: r.pinned,
				Age: now.Sub(r.age), Phantom: r.phantom,
			}
			if r.rr != nil {
				js.Runner = r.rr.name
			}
			ls.Jobs = append(ls.Jobs, js)
		}
		st.Lanes = append(st.Lanes, ls)
	}
	return st
}

func laneName(l LaneKey) string {
	return fmt.Sprintf("%s/%s/l%d", l.Target, l.OS, l.Lane)
}

// ---- logging -------------------------------------------------------------

type logger struct {
	l interface {
		Info(string, ...any)
		Warn(string, ...any)
	}
}

func (l logger) info(msg string, kv ...any) { l.l.Info(msg, kv...) }
func (l logger) warn(msg string, kv ...any) { l.l.Warn(msg, kv...) }

// ---- phantom jobs --------------------------------------------------------

// reconcilePhantoms compares GitHub's count of assigned-unstarted jobs per lane
// (Assigned - Running) with the ones we know. A deficit that lasts PhantomGrace
// (a lost JobAssigned, e.g. after a restart) gets placeholder jobs that go
// through normal admission, so a runner starts and takes the real job.
func (s *Scheduler) reconcilePhantoms() {
	now := s.now()
	for _, l := range s.sortedLanes() {
		if !l.hasStats {
			continue
		}
		// Stats older than the last state change, or past StatsMaxAge (except
		// the session's initial stats), say nothing reliable.
		if l.statsAt.Add(2*time.Second).Before(l.changedAt) || (!l.statsInitial && now.Sub(l.statsAt) > s.o.StatsMaxAge) {
			continue
		}
		known := 0
		var ph []*jobRec
		for _, r := range s.sortedJobs() {
			switch {
			case r.lane != l.key:
			case r.phantom:
				ph = append(ph, r)
			case (r.phase == pHeld || r.phase == pStarting) && r.unboundAt.IsZero():
				known++
			}
		}
		for _, rr := range s.runners {
			if rr.lane == l.key && rr.job == nil {
				known++ // spare runner: it will take the oldest assigned job
			}
		}
		need := max(0, l.stats.Assigned-l.stats.Running) - known
		if need <= 0 {
			l.deficitSince = time.Time{}
			for _, p := range ph {
				s.dropPhantom(p)
			}
			continue
		}
		if l.deficitSince.IsZero() {
			l.deficitSince = now
		}
		for len(ph) > need { // drop placeholders without a runner first
			i := slices.IndexFunc(ph, func(p *jobRec) bool { return p.rr == nil })
			if i < 0 {
				i = len(ph) - 1
			}
			s.dropPhantom(ph[i])
			ph = slices.Delete(ph, i, i+1)
		}
		if s.drain || now.Sub(l.deficitSince) < s.o.PhantomGrace {
			continue
		}
		for len(ph) < need {
			s.seq++
			id := fmt.Sprintf("phantom-%d", s.seq)
			r := &jobRec{
				id: id, lane: l.key, seq: s.seq, phantom: true, cost: s.o.DefaultJobMem, cpu: s.o.DefaultJobCPU,
				age: l.deficitSince, ageKey: "phantom|" + id,
				job: core.Job{JobID: id, Owner: l.key.Target.Owner, Repo: l.key.Target.Repo, Name: "(unknown)", ScaleSet: l.key.ScaleSetKey},
			}
			s.jobs[id] = r
			s.enqueue(r)
			ph = append(ph, r)
			s.log.warn("assigned job unknown; placeholder created", "lane", l.key.Lane, "assigned", l.stats.Assigned, "running", l.stats.Running)
		}
	}
}

// dropPhantom removes a placeholder; a runner already started for it stays as
// a spare that will take the next assigned job.
func (s *Scheduler) dropPhantom(p *jobRec) {
	s.forget(p)
	if rr := p.rr; rr != nil {
		rr.job, p.rr = nil, nil
		s.shrink(rr)
	}
}

// mergePhantom lets a late real JobAssigned replace a placeholder in its lane,
// taking over the placeholder's runner and age so nothing starts twice.
func (s *Scheduler) mergePhantom(incoming *jobRec) {
	var ph *jobRec
	for _, r := range s.sortedJobs() {
		if r.phantom && r.lane == incoming.lane && r.phase != pBound && (ph == nil || (ph.rr == nil && r.rr != nil)) {
			ph = r
		}
	}
	if ph == nil {
		return
	}
	if ph.age.Before(incoming.age) {
		incoming.age = ph.age
	}
	if rr := ph.rr; rr != nil {
		ph.rr = nil
		rr.job, incoming.rr = incoming, rr
		incoming.phase = pStarting
		if rr.full {
			_, _ = s.led.ForceResize(rr.tok, charge(rr, s.reqFor(rr, incoming)))
		}
	}
	s.forget(ph)
}
