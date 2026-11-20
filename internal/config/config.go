// Package config loads and validates an instance's config.toml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/docker/go-units"

	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// SchemaVersion is bumped on incompatible config changes.
const SchemaVersion = 1

// Size is a byte count written as "22GiB", "512MiB", ...
type Size int64

func (s *Size) UnmarshalText(b []byte) error {
	v, err := units.RAMInBytes(string(b))
	if err != nil {
		return fmt.Errorf("invalid size %q: %w", b, err)
	}
	*s = Size(v)
	return nil
}

func (s Size) MarshalText() ([]byte, error) { return []byte(units.BytesSize(float64(s))), nil }

// Duration is a time.Duration written as "40m", "6h", ...
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", b, err)
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// D returns the value as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

type Config struct {
	SchemaVersion int    `toml:"schema_version"`
	Prefix        string `toml:"prefix"`
	Instance      string `toml:"instance"`
	StateDir      string `toml:"state_dir"`
	CacheDir      string `toml:"cache_dir"`
	LogLevel      string `toml:"log_level"`

	GitHub  GitHub   `toml:"github"`
	Limits  Limits   `toml:"limits"`
	Linux   Linux    `toml:"linux"`
	Windows Windows  `toml:"windows"`
	Hang    Hang     `toml:"hang"`
	Stale   Stale    `toml:"stale"`
	ImageGC ImageGC  `toml:"image_gc"`
	Caches  []Cache  `toml:"cache"`
	Costs   []Cost   `toml:"cost"`
	Targets []Target `toml:"target"`
}

type GitHub struct {
	TokenFile string `toml:"token_file"`
}

type Limits struct {
	MemoryBudget     Size     `toml:"memory_budget"`     // hard ceiling for all jobs (slice MemoryMax)
	SwapMax          Size     `toml:"swap_max"`          // slice MemorySwapMax
	ReservedCores    string   `toml:"reserved_cores"`    // e.g. "0-1"
	MaxRunners       int      `toml:"max_runners"`       // 0 = unlimited
	DefaultJobMem    Size     `toml:"default_job_mem"`   // cost when nothing better is known
	LightThreshold   Size     `toml:"light_threshold"`   // jobs at or below may overtake
	RunnerOverhead   Size     `toml:"runner_overhead"`   // idle runner footprint
	HoldMax          Duration `toml:"hold_max"`          // max time a heavy head job blocks light jobs
	AdmissionTimeout Duration `toml:"admission_timeout"` // hook wait before failing a job
}

type Linux struct {
	Enabled      bool   `toml:"enabled"`
	Image        string `toml:"image"`
	DockerSocket string `toml:"docker_socket"`
	RunnerUID    int    `toml:"runner_uid"`
	TmpfsSize    Size   `toml:"tmpfs_size"`
	OOMScoreAdj  int    `toml:"oom_score_adj"`
}

type Windows struct {
	Enabled       bool     `toml:"enabled"`
	MaxConcurrent int      `toml:"max_concurrent"` // licences; required when enabled
	Image         string   `toml:"image"`          // dockurr/windows
	StorageDir    string   `toml:"storage_dir"`    // VM disk + state
	VMMem         Size     `toml:"vm_mem"`
	VMCPUs        int      `toml:"vm_cpus"`
	SSHPort       int      `toml:"ssh_port"`
	SSHUser       string   `toml:"ssh_user"`
	SSHKeyFile    string   `toml:"ssh_key_file"`
	RunnerDir     string   `toml:"runner_dir"` // inside the guest
	WorkDir       string   `toml:"work_dir"`   // inside the guest, wiped per job
	IdleTTL       Duration `toml:"idle_ttl"`
	Container     string   `toml:"container"` // container name to manage or adopt
}

type Hang struct {
	Idle        Duration `toml:"idle"`
	MaxDuration Duration `toml:"max_duration"`
	NeverJob    Duration `toml:"never_job"`
	Grace       Duration `toml:"grace"`
	RerunOnLoss bool     `toml:"rerun_on_runner_loss"`
}

type Stale struct {
	CancelClosedPRs     bool     `toml:"cancel_closed_prs"`
	CancelDeletedBranch bool     `toml:"cancel_deleted_branches"`
	Interval            Duration `toml:"interval"`
}

type ImageGC struct {
	HighWatermark int      `toml:"high_watermark_percent"`
	Interval      Duration `toml:"interval"`
	UnusedFor     Duration `toml:"unused_for"`
}

// Cache is a host directory shared with jobs.
type Cache struct {
	Name          string `toml:"name"`
	HostPath      string `toml:"host_path"`
	ContainerPath string `toml:"container_path"`
	Env           string `toml:"env"`
	Mode          string `toml:"mode"` // shared | pooled
	UID           int    `toml:"uid"`
	GID           int    `toml:"gid"`
	DirMode       string `toml:"dir_mode"` // e.g. "2775"
}

// Cost assigns resources to jobs matching a glob over
// "owner/repo/workflow-file/job-name" (path.Match syntax per segment).
type Cost struct {
	Match       string   `toml:"match"`
	Mem         Size     `toml:"mem"`
	PinnedCores int      `toml:"pinned_cores"`
	Devices     []string `toml:"devices"`
}

type Target struct {
	Org         string   `toml:"org"`
	Repo        string   `toml:"repo"` // owner/name
	RunnerGroup string   `toml:"runner_group"`
	OS          []string `toml:"os"`
	Weight      int      `toml:"weight"`
	Max         int      `toml:"max"`
}

// Core converts a config target into a core.Target.
func (t Target) Core() core.Target {
	if t.Org != "" {
		return core.Target{Owner: t.Org, RunnerGroup: t.RunnerGroup}
	}
	owner, repo, _ := strings.Cut(t.Repo, "/")
	return core.Target{Owner: owner, Repo: repo}
}

// OSes returns the target's operating systems (default linux).
func (t Target) OSes() []core.OS {
	if len(t.OS) == 0 {
		return []core.OS{core.Linux}
	}
	out := make([]core.OS, len(t.OS))
	for i, s := range t.OS {
		out[i] = core.OS(s)
	}
	return out
}

// Default paths.
const (
	EtcDir   = "/etc/runner-warden"
	StateDir = "/var/lib/runner-warden"
	CacheDir = "/var/cache/runner-warden"
	RunDir   = "/run/runner-warden"
)

// Path returns the default config path for an instance.
func Path(instance string) string { return filepath.Join(EtcDir, instance, "config.toml") }

// SocketPath is the control socket of an instance.
func SocketPath(instance string) string { return filepath.Join(RunDir, instance+".sock") }

// Load reads, defaults and validates a config file.
func Load(file string) (*Config, error) {
	var c Config
	md, err := toml.DecodeFile(file, &c)
	if err != nil {
		return nil, err
	}
	if und := md.Undecoded(); len(und) > 0 {
		return nil, fmt.Errorf("unknown config keys: %v", und)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Save writes the config atomically with 0640 permissions.
func (c *Config) Save(file string) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o750); err != nil {
		return err
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	if err := toml.NewEncoder(f).Encode(c); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// ApplyDefaults fills every unset field. Prefix has no default.
func (c *Config) ApplyDefaults() {
	if c.SchemaVersion == 0 {
		c.SchemaVersion = SchemaVersion
	}
	if c.Instance == "" {
		c.Instance = c.Prefix
	}
	def := func(s *string, v string) {
		if *s == "" {
			*s = v
		}
	}
	defSize := func(s *Size, v int64) {
		if *s == 0 {
			*s = Size(v)
		}
	}
	defDur := func(d *Duration, v time.Duration) {
		if *d == 0 {
			*d = Duration(v)
		}
	}
	def(&c.StateDir, filepath.Join(StateDir, c.Instance))
	def(&c.CacheDir, filepath.Join(CacheDir, c.Instance))
	def(&c.LogLevel, "info")
	def(&c.GitHub.TokenFile, filepath.Join(EtcDir, c.Instance, "token"))

	defSize(&c.Limits.DefaultJobMem, 2*units.GiB)
	defSize(&c.Limits.LightThreshold, 1*units.GiB)
	defSize(&c.Limits.RunnerOverhead, 300*units.MiB)
	defSize(&c.Limits.SwapMax, 2*units.GiB)
	defDur(&c.Limits.HoldMax, 2*time.Minute)
	defDur(&c.Limits.AdmissionTimeout, 40*time.Minute)

	def(&c.Linux.Image, "ghcr.io/actions/actions-runner:latest")
	def(&c.Linux.DockerSocket, "/var/run/docker.sock")
	if c.Linux.RunnerUID == 0 {
		c.Linux.RunnerUID = 1001
	}
	if c.Linux.OOMScoreAdj == 0 {
		c.Linux.OOMScoreAdj = 500
	}

	def(&c.Windows.Image, "dockurr/windows")
	def(&c.Windows.StorageDir, filepath.Join(c.StateDir, "windows"))
	defSize(&c.Windows.VMMem, 8*units.GiB)
	if c.Windows.VMCPUs == 0 {
		c.Windows.VMCPUs = 4
	}
	if c.Windows.SSHPort == 0 {
		c.Windows.SSHPort = 2222
	}
	def(&c.Windows.SSHUser, "runner")
	def(&c.Windows.RunnerDir, `C:\actions-runner`)
	def(&c.Windows.WorkDir, `C:\rw`)
	defDur(&c.Windows.IdleTTL, 15*time.Minute)
	def(&c.Windows.Container, c.Instance+"-windows")

	defDur(&c.Hang.Idle, 30*time.Minute)
	defDur(&c.Hang.MaxDuration, 6*time.Hour)
	defDur(&c.Hang.NeverJob, 10*time.Minute)
	defDur(&c.Hang.Grace, 3*time.Minute)

	defDur(&c.Stale.Interval, 2*time.Minute)

	if c.ImageGC.HighWatermark == 0 {
		c.ImageGC.HighWatermark = 85
	}
	defDur(&c.ImageGC.Interval, 20*time.Hour)
	defDur(&c.ImageGC.UnusedFor, 24*time.Hour)

	for i := range c.Caches {
		if c.Caches[i].Mode == "" {
			c.Caches[i].Mode = "shared"
		}
		if c.Caches[i].ContainerPath == "" {
			c.Caches[i].ContainerPath = c.Caches[i].HostPath
		}
		if c.Caches[i].UID == 0 {
			c.Caches[i].UID = c.Linux.RunnerUID
			c.Caches[i].GID = c.Linux.RunnerUID
		}
		def(&c.Caches[i].DirMode, "2775")
	}
	for i := range c.Targets {
		if c.Targets[i].Weight == 0 {
			c.Targets[i].Weight = 1
		}
	}
}

// Validate reports every problem found, joined.
func (c *Config) Validate() error {
	var errs []error
	add := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	if c.SchemaVersion > SchemaVersion {
		add("schema_version %d is newer than this binary (%d)", c.SchemaVersion, SchemaVersion)
	}
	if err := naming.ValidatePrefix(c.Prefix); err != nil {
		add("prefix: %w", err)
	}
	if c.Instance != "" && !regexpSafe(c.Instance) {
		add("instance %q: use lowercase letters, digits and hyphens", c.Instance)
	}
	if c.Limits.MemoryBudget <= 0 {
		add("limits.memory_budget is required")
	}
	if c.Windows.Enabled && c.Windows.MaxConcurrent < 1 {
		add("windows.max_concurrent is required when windows is enabled (number of Windows licences)")
	}
	if !c.Linux.Enabled && !c.Windows.Enabled {
		add("enable at least one of [linux] or [windows]")
	}
	for i, ca := range c.Caches {
		if ca.Name == "" || ca.HostPath == "" {
			add("cache[%d]: name and host_path are required", i)
		}
		if ca.Mode != "shared" && ca.Mode != "pooled" {
			add("cache %q: mode must be shared or pooled", ca.Name)
		}
		if !filepath.IsAbs(ca.HostPath) || !filepath.IsAbs(ca.ContainerPath) {
			add("cache %q: paths must be absolute", ca.Name)
		}
	}
	for i, co := range c.Costs {
		if strings.Count(co.Match, "/") != 3 {
			add("cost[%d]: match must be owner/repo/workflow/job, got %q", i, co.Match)
		} else if _, err := path.Match(co.Match, ""); err != nil {
			add("cost[%d]: bad pattern %q: %w", i, co.Match, err)
		}
		if co.Mem <= 0 {
			add("cost[%d]: mem is required", i)
		}
	}
	seen := map[string]bool{}
	for i, t := range c.Targets {
		switch {
		case (t.Org == "") == (t.Repo == ""):
			add("target[%d]: set exactly one of org or repo", i)
		case t.Repo != "" && strings.Count(t.Repo, "/") != 1:
			add("target[%d]: repo must be owner/name", i)
		case t.Repo != "" && t.RunnerGroup != "":
			add("target[%d]: runner_group only applies to org targets", i)
		}
		for _, o := range t.OSes() {
			if o != core.Linux && o != core.Windows {
				add("target[%d]: unsupported os %q", i, o)
			}
			if o == core.Windows && !c.Windows.Enabled {
				add("target[%d]: windows requested but [windows] is not enabled", i)
			}
			if o == core.Linux && !c.Linux.Enabled {
				add("target[%d]: linux requested but [linux] is not enabled", i)
			}
		}
		k := t.Org + t.Repo
		if seen[k] {
			add("target %q listed twice", k)
		}
		seen[k] = true
	}
	return errors.Join(errs...)
}

func regexpSafe(s string) bool { return naming.ValidatePrefix(s) == nil }
