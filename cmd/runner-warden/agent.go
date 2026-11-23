package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/autocache"
	"github.com/kylepantall/runner-warden/internal/backend/docker"
	"github.com/kylepantall/runner-warden/internal/backend/linuxhost"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/naming"
	"github.com/kylepantall/runner-warden/internal/platform/linux"
)

// agentCmd serves one request from a daemon that uses this machine as a
// [[linux.host]]: JSON request on stdin, JSON response on stdout.
func agentCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "agent",
		Short:  "Run one backend operation for a remote daemon (used over SSH)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			var req linuxhost.Request
			if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&req); err != nil {
				return fmt.Errorf("agent request: %w", err)
			}
			resp := runAgent(cmd, req)
			return json.NewEncoder(os.Stdout).Encode(resp)
		},
	}
}

func runAgent(cmd *cobra.Command, req linuxhost.Request) linuxhost.Response {
	fail := func(err error) linuxhost.Response {
		r := linuxhost.Dispatch(cmd.Context(), nil, linuxhost.Request{Op: linuxhost.OpPing}, version)
		r.Error = err.Error()
		return r
	}
	if req.Op == linuxhost.OpPing {
		return linuxhost.Dispatch(cmd.Context(), nil, req, version)
	}
	a := req.Config
	if naming.ValidatePrefix(a.Instance) != nil || a.HostID == "" || a.Image == "" {
		return fail(errors.New("agent: instance, host id and image are required"))
	}
	if req.Op == linuxhost.OpCaches {
		return agentCaches(req)
	}
	exe, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	slice := naming.Slice(a.Instance)
	if req.Op == linuxhost.OpPrepare {
		if err := linux.EnsureSlice(slice, 0, -1); err != nil {
			return fail(fmt.Errorf("job slice: %w", err))
		}
	}
	var binds []string
	var env map[string]string
	if req.Op == linuxhost.OpPrepare || req.Op == linuxhost.OpStart {
		// Only starts mount caches; preparing them on every call would
		// reset what an operator changed.
		if binds, env, err = prepareCaches(a.Caches); err != nil {
			return fail(err)
		}
	}
	b, err := docker.New(docker.Config{
		Instance:     a.Instance,
		Prefix:       a.Prefix,
		HostID:       a.HostID,
		StateDir:     filepath.Join(config.StateDir, a.Instance),
		Image:        a.Image,
		Slice:        slice,
		RunnerUID:    a.RunnerUID,
		TmpfsSize:    a.TmpfsSize,
		OOMScoreAdj:  a.OOMScoreAdj,
		HookBinary:   exe,
		HookDir:      daemon.HookDir(a.Instance),
		NoHooks:      true,
		AllowedBinds: a.AllowedBinds,
		CacheBinds:   binds,
		CacheEnv:     env,
		Builds:       a.Builds,
		LogLevel:     a.LogLevel,
		Log:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})),
	})
	if err != nil {
		return fail(err)
	}
	if req.Op == linuxhost.OpStart && req.Spec != nil {
		// The runner's automatic cache: made for the runner user, its use
		// recorded here, where this host's caches are trimmed.
		for _, m := range req.Spec.Mounts {
			if strings.HasPrefix(m.HostPath, daemon.AutoCacheDir(a.Instance)+"/") {
				_ = autocache.TouchDir(m.HostPath, a.RunnerUID)
			}
		}
	}
	return linuxhost.Dispatch(cmd.Context(), b, req, version)
}

// agentCaches measures this machine's automatic caches and trims them.
func agentCaches(req linuxhost.Request) linuxhost.Response {
	resp := linuxhost.Response{Version: version}
	base := daemon.AutoCacheDir(req.Config.Instance)
	us, err := autocache.Scan(base)
	if err != nil {
		resp.Error = err.Error()
		return resp
	}
	inUse := map[string]bool{}
	for _, s := range req.CacheInUse {
		inUse[s] = true
	}
	if req.CacheMax > 0 {
		gone, err := autocache.Evict(base, us, req.CacheMax, inUse)
		resp.Removed = gone
		if err != nil {
			resp.Error = err.Error()
		}
		us = slices.DeleteFunc(us, func(u autocache.Usage) bool {
			return slices.ContainsFunc(gone, func(g autocache.Usage) bool { return g.Scope == u.Scope })
		})
	}
	resp.Caches = us
	return resp
}
