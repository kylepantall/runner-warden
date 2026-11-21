// Package scheduler implements the lane state machine: each (target, OS) has
// several lane scale sets, every lane exposes at most one unstarted job
// (capacity = started + 1), and a host-wide admission ledger decides when a
// runner may be started for that job.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/kylepantall/runner-warden/internal/admission"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// LaneKey identifies one lane scale set (<prefix>-<os>-l<Lane>).
type LaneKey struct {
	core.ScaleSetKey
	Lane int
}

// LaneStats is the scale-set service's view of one lane; the daemon adapts
// gh.Stats into it.
type LaneStats struct {
	Assigned, Running, Registered, Busy, Idle int
}

// Grant is the reservation a runner is started with.
type Grant struct {
	Key      string // ledger key; opaque
	MemBytes int64  // reserved for this runner (overhead only when Parked)
	CPUSet   string // pinned logical cpus, "" for shared
	Parked   bool   // overhead-only reservation: the job's hook waits for the rest
	Job      core.Job
}

// Runners starts and stops runners; implemented by the daemon over backends
// and the GitHub client. Calls are made outside the scheduler lock and may
// block.
type Runners interface {
	StartRunner(ctx context.Context, lane LaneKey, reservation Grant) (runnerName string, err error)
	StopRunner(ctx context.Context, runnerName string) error
	ForceCancelRun(ctx context.Context, owner, repo string, runID int64) error
	RerunJob(ctx context.Context, owner, repo string, runID int64, jobName string) error
}

// Capacity publishes the max-runners value a lane's listener sends on its next
// poll. It is called under the scheduler lock and must not block or call back.
type Capacity interface {
	SetCapacity(lane LaneKey, n int)
}

// VMProbe reports VMs currently up and the memory each reserves
// (name -> bytes); the scheduler mirrors them as ledger keys "vm:<name>".
type VMProbe interface {
	VMsUp() map[string]int64
}

// Observer receives job history (optional).
type Observer interface {
	JobFinished(JobResult)
}

// JobResult is one finished job, for the store's history table.
type JobResult struct {
	Job      core.Job
	Lane     LaneKey
	Runner   string
	Cost     int64
	Result   string
	Started  time.Time // zero if the job never started
	Finished time.Time
}

// TargetOpts are per-target fairness settings.
type TargetOpts struct {
	Weight int // default 1
	Max    int // max live runners for the target, 0 = unlimited
}

// Options configures a Scheduler. Zero durations take the documented default.
type Options struct {
	Clock    Clock // default RealClock
	Runners  Runners
	Capacity Capacity
	Log      *slog.Logger
	VMs      VMProbe
	Observer Observer

	Budget           int64
	ExternalReserved int64
	Cores            []admission.Core
	ReservedCPUs     []int
	Costs            []admission.Rule

	DefaultJobMem  int64 // default 2GiB
	LightThreshold int64
	RunnerOverhead int64

	ReserveAfter       time.Duration // 5m
	HolGrace           time.Duration // 2m
	HoldMax            time.Duration // 30m
	AdmissionTimeout   time.Duration // 40m
	NeverJob           time.Duration // 10m
	ReapGrace          time.Duration // 3m
	UnboundDrop        time.Duration // 90s
	UnknownRunnerGrace time.Duration // 30s: hook for a runner we do not know yet
	StartRetry         time.Duration // 10s after a failed StartRunner
	RerunDelay         time.Duration // 30s after the lost job completes
	AgeTTL             time.Duration // 2h: remembered job ages
	PhantomGrace       time.Duration // 45s: deficit of known vs GitHub-assigned jobs before a placeholder
	StatsMaxAge        time.Duration // 2m: lane stats older than this are ignored (initial ones excepted)

	MaxParked   int // 4
	MaxRunners  int // 0 = unlimited
	RerunOnLoss bool

	Targets map[core.Target]TargetOpts
	Lanes   []LaneKey // lanes to publish capacity for from the start

	// JobTimeout optionally returns a job's timeout-minutes (0 = unknown); it
	// only biases which held job gets promoted to a parked runner.
	JobTimeout func(core.Job) time.Duration

	// VMCost serves Windows lanes, whose jobs run inside a VM. It returns the
	// memory a VM for this lane reserves and whether a suitable VM is already
	// up (its memory is then mirrored in the ledger through VMs). It runs under
	// the scheduler lock and must not block or call back. Without a suitable VM
	// a grant also reserves vmMem as "vm:pending:<job>" before StartRunner; the
	// job's own cost is then overhead only, because the VM's memory is what the
	// job uses. Nil keeps the plain per-job cost on every lane.
	VMCost func(LaneKey) (vmMem int64, alreadyUp bool)
}

// OptionsFromConfig fills the config-driven fields. lanes lists every lane the
// daemon runs scale sets for. The caller sets Runners, Capacity, Clock, Log,
// VMs, Observer and the host topology.
func OptionsFromConfig(c *config.Config, lanes []LaneKey) Options {
	o := Options{
		Budget:           int64(c.Limits.MemoryBudget),
		ExternalReserved: int64(c.Limits.ExternalReserved),
		Costs:            admission.RulesFromConfig(c.Costs),
		DefaultJobMem:    int64(c.Limits.DefaultJobMem),
		LightThreshold:   int64(c.Limits.LightThreshold),
		RunnerOverhead:   int64(c.Limits.RunnerOverhead),
		ReserveAfter:     c.Limits.ReserveAfter.D(),
		HolGrace:         c.Limits.HolGrace.D(),
		HoldMax:          c.Limits.HoldMax.D(),
		AdmissionTimeout: c.Limits.AdmissionTimeout.D(),
		NeverJob:         c.Hang.NeverJob.D(),
		ReapGrace:        c.Hang.Grace.D(),
		MaxParked:        c.Limits.MaxParked,
		MaxRunners:       c.Limits.MaxRunners,
		RerunOnLoss:      c.Hang.RerunOnLoss,
		Targets:          map[core.Target]TargetOpts{},
		Lanes:            lanes,
	}
	if cpus, err := admission.ParseCPUList(c.Limits.ReservedCores); err == nil {
		o.ReservedCPUs = cpus
	}
	for _, t := range c.Targets {
		o.Targets[t.Core()] = TargetOpts{Weight: t.Weight, Max: t.Max}
	}
	return o
}

func (o *Options) defaults() {
	if o.Clock == nil {
		o.Clock = RealClock{}
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	dur := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	dur(&o.ReserveAfter, 5*time.Minute)
	dur(&o.HolGrace, 2*time.Minute)
	dur(&o.HoldMax, 30*time.Minute)
	dur(&o.AdmissionTimeout, 40*time.Minute)
	dur(&o.NeverJob, 10*time.Minute)
	dur(&o.ReapGrace, 3*time.Minute)
	dur(&o.UnboundDrop, 90*time.Second)
	dur(&o.UnknownRunnerGrace, 30*time.Second)
	dur(&o.StartRetry, 10*time.Second)
	dur(&o.RerunDelay, 30*time.Second)
	dur(&o.AgeTTL, 2*time.Hour)
	dur(&o.PhantomGrace, 45*time.Second)
	dur(&o.StatsMaxAge, 2*time.Minute)
	if o.DefaultJobMem <= 0 {
		o.DefaultJobMem = 2 << 30
	}
	if o.MaxParked <= 0 {
		o.MaxParked = 4
	}
}

// Hook verdict reasons.
const (
	ReasonGranted          = "granted"
	ReasonAdmissionTimeout = "admission_timeout"
	ReasonUnknownRunner    = "unknown_runner"
)

// HookRequest is what the runner's job-started hook sends. Runner is the
// primary key; RunID and Repository are the fallback when it is empty.
type HookRequest struct {
	Runner     string `json:"runner"`
	RunID      int64  `json:"run_id"`
	JobKey     string `json:"job_key"`  // GITHUB_JOB (job id, not display name)
	Workflow   string `json:"workflow"` // GITHUB_WORKFLOW
	Repository string `json:"repository"`
}

// HookResponse is the verdict.
type HookResponse struct {
	Allow  bool          `json:"allow"`
	Reason string        `json:"reason"`
	Waited time.Duration `json:"waited"`
}

// JobState is a job's position in the lane state machine.
type JobState string

// Job lifecycle states.
const (
	StateHeld     JobState = "held"     // assigned, waiting for a grant
	StateStarting JobState = "starting" // runner requested, no Started yet
	StateStarted  JobState = "started"  // bound to a runner, hook not yet cleared
	StateParked   JobState = "parked"   // runner up, hook waiting for memory
	StateRunning  JobState = "running"
	StateUnbound  JobState = "unbound" // reassign churn: assigned job vanished
)

// JobStatus is one job in Status.
type JobStatus struct {
	JobID    string        `json:"job_id"`
	Owner    string        `json:"owner"`
	Repo     string        `json:"repo"`
	Workflow string        `json:"workflow"`
	Name     string        `json:"name"`
	RunID    int64         `json:"run_id"`
	State    JobState      `json:"state"`
	Runner   string        `json:"runner,omitempty"`
	Cost     int64         `json:"cost"`
	Pinned   int           `json:"pinned_cores,omitempty"`
	Age      time.Duration `json:"age"`
	Phantom  bool          `json:"phantom,omitempty"`
}

// LaneStatus is one lane in Status.
type LaneStatus struct {
	Lane     LaneKey     `json:"lane"`
	Name     string      `json:"name"` // owner[/repo]/os/lN
	Capacity int         `json:"capacity"`
	Started  int         `json:"started"`
	Stats    LaneStats   `json:"stats"`
	Jobs     []JobStatus `json:"jobs"`
}

// Status is the snapshot behind `runner-warden status`.
type Status struct {
	Draining bool            `json:"draining"`
	Ledger   admission.Usage `json:"ledger"`
	Runners  int             `json:"runners"`
	Parked   int             `json:"parked"`
	Held     int             `json:"held"`
	Lanes    []LaneStatus    `json:"lanes"`
}

// RestoredRunner is a runner found in the store/backends at startup.
type RestoredRunner struct {
	Name    string
	Lane    LaneKey
	Created time.Time
	Job     *core.Job // job bound to the runner, if known
	Started bool      // the job has started (otherwise it is still unstarted)
}
