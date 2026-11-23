// Package linuxhost runs Linux runners on separate machines. The daemon keeps
// the scale sets and the scheduler; a remote host only runs the Docker
// backend, driven one operation at a time by "runner-warden agent" over SSH.
// Hosts join a hostpool.Pool.
package linuxhost

import (
	"time"

	"github.com/kylepantall/runner-warden/internal/autocache"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// Operations an agent serves.
const (
	OpPing    = "ping"
	OpPrepare = "prepare"
	OpStart   = "start"
	OpStats   = "stats"
	OpStop    = "stop"
	OpCleanup = "cleanup"
	OpList    = "list"
	OpAdopt   = "adopt"
	OpCaches  = "caches" // measure the automatic caches, trim them to CacheMax
)

// AgentConfig is the part of the daemon's Linux configuration a remote host
// runs with. Paths that are the host's own (state, sockets, binary) are not
// sent: the agent derives them from Instance.
type AgentConfig struct {
	Instance        string            `json:"instance"`
	Prefix          string            `json:"prefix"`
	HostID          string            `json:"host_id"`
	Image           string            `json:"image"`
	RunnerUID       int               `json:"runner_uid,omitempty"`
	TmpfsSize       int64             `json:"tmpfs_size,omitempty"`
	OOMScoreAdj     int               `json:"oom_score_adj,omitempty"`
	AllowedBinds    []string          `json:"allowed_binds,omitempty"`
	PersistentNames []string          `json:"persistent_names,omitempty"`
	Caches          []config.Cache    `json:"caches,omitempty"`
	Builds          map[string]string `json:"builds,omitempty"` // image tag -> Dockerfile, built on demand
	LogLevel        string            `json:"log_level,omitempty"`
}

// Request is one agent call, sent as JSON on stdin.
type Request struct {
	Op     string           `json:"op"`
	Config AgentConfig      `json:"config"`
	Spec   *core.RunnerSpec `json:"spec,omitempty"`
	Handle *core.Handle     `json:"handle,omitempty"`
	Runner string           `json:"runner,omitempty"`
	Grace  time.Duration    `json:"grace,omitempty"`
	// OpCaches: the budget and the scopes in use on the host.
	CacheMax   int64    `json:"cache_max,omitempty"`
	CacheInUse []string `json:"cache_in_use,omitempty"`
}

// Response is the agent's answer, written as JSON on stdout.
type Response struct {
	Version string            `json:"version"`
	Arch    string            `json:"arch,omitempty"`
	Handle  core.Handle       `json:"handle,omitzero"`
	Handles []core.Handle     `json:"handles,omitempty"`
	Usage   core.Usage        `json:"usage,omitzero"`
	Caches  []autocache.Usage `json:"caches,omitempty"` // kept, most recent first
	Removed []autocache.Usage `json:"removed,omitempty"`
	Error   string            `json:"error,omitempty"`
}
