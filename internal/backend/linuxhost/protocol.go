// Package linuxhost runs Linux runners on separate machines. The daemon keeps
// the scale sets and the scheduler; a remote host only runs the Docker
// backend, driven one operation at a time by "runner-warden agent" over SSH.
//
// Pool combines this host's backend with the remote ones. A runner goes to a
// remote host when its spec carries HostEnv; its handle's BackendID is then
// "<host>/<container id>", which is how every later call finds the host.
package linuxhost

import (
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
)

// HostEnv in a RunnerSpec's Env names the remote host to start it on. The
// pool removes it before the spec reaches a backend.
const HostEnv = "RUNNER_WARDEN_HOST"

// HostOf splits a BackendID into the remote host and the id on that host;
// host is "" for a runner on this machine.
func HostOf(backendID string) (host, id string) {
	if h, rest, ok := strings.Cut(backendID, "/"); ok {
		return h, rest
	}
	return "", backendID
}

func onHost(host, id string) string { return host + "/" + id }

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
)

// AgentConfig is the part of the daemon's Linux configuration a remote host
// runs with. Paths that are the host's own (state, sockets, binary) are not
// sent: the agent derives them from Instance.
type AgentConfig struct {
	Instance        string         `json:"instance"`
	Prefix          string         `json:"prefix"`
	HostID          string         `json:"host_id"`
	Image           string         `json:"image"`
	RunnerUID       int            `json:"runner_uid,omitempty"`
	TmpfsSize       int64          `json:"tmpfs_size,omitempty"`
	OOMScoreAdj     int            `json:"oom_score_adj,omitempty"`
	AllowedBinds    []string       `json:"allowed_binds,omitempty"`
	PersistentNames []string       `json:"persistent_names,omitempty"`
	Caches          []config.Cache `json:"caches,omitempty"`
	LogLevel        string         `json:"log_level,omitempty"`
}

// Request is one agent call, sent as JSON on stdin.
type Request struct {
	Op     string           `json:"op"`
	Config AgentConfig      `json:"config"`
	Spec   *core.RunnerSpec `json:"spec,omitempty"`
	Handle *core.Handle     `json:"handle,omitempty"`
	Runner string           `json:"runner,omitempty"`
	Grace  time.Duration    `json:"grace,omitempty"`
}

// Response is the agent's answer, written as JSON on stdout.
type Response struct {
	Version string        `json:"version"`
	Arch    string        `json:"arch,omitempty"`
	Handle  core.Handle   `json:"handle,omitzero"`
	Handles []core.Handle `json:"handles,omitempty"`
	Usage   core.Usage    `json:"usage,omitzero"`
	Error   string        `json:"error,omitempty"`
}
