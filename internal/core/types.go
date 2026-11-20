// Package core holds the types and interfaces shared by every runner-warden
// package. It has no dependencies on other internal packages.
package core

import (
	"context"
	"time"
)

// OS is a runner operating system; each OS has its own label and scale set.
type OS string

const (
	Linux   OS = "linux"
	Windows OS = "windows"
	MacOS   OS = "macos"
)

// Target is a GitHub owner (org) or a single repo that runners serve.
type Target struct {
	Owner       string // org or user login
	Repo        string // empty for an org target
	RunnerGroup string // org targets only; "" means the default group
}

// IsOrg reports whether runners register at org level.
func (t Target) IsOrg() bool { return t.Repo == "" }

// URL is the registration URL used by the scale-set API.
func (t Target) URL() string {
	if t.IsOrg() {
		return "https://github.com/" + t.Owner
	}
	return "https://github.com/" + t.Owner + "/" + t.Repo
}

// String is owner or owner/repo.
func (t Target) String() string {
	if t.IsOrg() {
		return t.Owner
	}
	return t.Owner + "/" + t.Repo
}

// ScaleSetKey identifies one scale set this instance owns.
type ScaleSetKey struct {
	Target Target
	OS     OS
}

// Job is what the scale-set service tells us about a job.
type Job struct {
	JobID       string // service GUID
	RunID       int64  // workflow run id (REST)
	Owner       string
	Repo        string
	WorkflowRef string // owner/repo/.github/workflows/x.yml@ref
	Name        string // job display name
	Event       string // push, pull_request, workflow_dispatch, ...
	Labels      []string
	QueueTime   time.Time
	AssignedAt  time.Time
	RunnerName  string // set once started
	RunnerID    int
	Result      string // set once completed
	ScaleSet    ScaleSetKey
}

// Mount is a host directory exposed to a runner and its job containers.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// RunnerSpec is everything a backend needs to start one ephemeral runner.
type RunnerSpec struct {
	Name      string
	JITConfig string
	ScaleSet  ScaleSetKey
	MemBytes  int64  // reservation; backends may use it as a hard limit
	CPUSet    string // pinned cpus, "" for shared
	Env       map[string]string
	Mounts    []Mount
	Image     string // backend-specific; "" for default
	Devices   []string
}

// Handle identifies a running runner inside a backend.
type Handle struct {
	RunnerName string
	BackendID  string // container id, VM id, pid ...
	StartedAt  time.Time
}

// Usage is a point-in-time resource sample for a runner and its children.
type Usage struct {
	CPUSeconds float64
	MemCurrent int64
	MemPeak    int64 // peak anonymous memory where available
	IOBytes    int64
	LogBytes   int64 // runner diagnostic log size, for hang detection
	OOMKills   int
	Alive      bool
	SampledAt  time.Time
}

// Caps describes what a backend can do.
type Caps struct {
	OS            OS
	MemLimits     bool  // enforces MemBytes
	FixedMemBytes int64 // VM backends: memory reserved per instance regardless of job
	MaxConcurrent int   // 0 = unlimited
	DockerInJob   bool
}

// Backend runs ephemeral runners on one platform.
type Backend interface {
	Caps() Caps
	// Prepare is called once at daemon start (pull images, boot nothing).
	Prepare(ctx context.Context) error
	Start(ctx context.Context, spec RunnerSpec) (Handle, error)
	Stats(ctx context.Context, h Handle) (Usage, error)
	// Stop terminates the runner (grace, then kill).
	Stop(ctx context.Context, h Handle, grace time.Duration) error
	// Cleanup removes everything the runner and its jobs created. It must be
	// safe to call repeatedly and on runners that never started.
	Cleanup(ctx context.Context, runnerName string) error
	// List returns runners this instance owns (by metadata, never by name).
	List(ctx context.Context) ([]Handle, error)
}
