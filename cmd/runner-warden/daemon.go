package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"

	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/backend/docker"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/daemon"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/imagegc"
	"github.com/kylepantall/runner-warden/internal/naming"
	"github.com/kylepantall/runner-warden/internal/platform/linux"
)

func daemonCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "daemon",
		Short: "Run the service (used by the systemd unit)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			log := newLogger(cfg.LogLevel)
			token, err := gh.Token(cfg.GitHub.TokenFile)
			if err != nil {
				return fmt.Errorf("token: %w", err)
			}
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if cfg.Linux.Enabled {
				if err := linux.EnsureSlice(naming.Slice(cfg.Instance), int64(cfg.Limits.MemoryBudget), int64(cfg.Limits.SwapMax)); err != nil {
					return fmt.Errorf("job slice: %w", err)
				}
			}
			binds, env, err := prepareCaches(cfg)
			if err != nil {
				return err
			}
			loops := map[string]func(context.Context) error{}
			d, err := daemon.New(daemon.Options{
				Config: cfg,
				Log:    log,
				Token:  token,
				MakeBackends: func(hostID string) (map[core.OS]core.Backend, daemon.WindowsPool, error) {
					bs := map[core.OS]core.Backend{}
					if cfg.Linux.Enabled {
						b, err := docker.New(docker.Config{
							Instance:     cfg.Instance,
							Prefix:       cfg.Prefix,
							HostID:       hostID,
							StateDir:     cfg.StateDir,
							Image:        cfg.Linux.Image,
							DockerSocket: cfg.Linux.DockerSocket,
							Slice:        naming.Slice(cfg.Instance),
							RunnerUID:    cfg.Linux.RunnerUID,
							TmpfsSize:    int64(cfg.Linux.TmpfsSize),
							OOMScoreAdj:  cfg.Linux.OOMScoreAdj,
							HookBinary:   exe,
							HookDir:      daemon.HookDir(cfg.Instance),
							CacheBinds:   binds,
							CacheEnv:     env,
							Log:          log.With("backend", "linux"),
						})
						if err != nil {
							return nil, nil, err
						}
						bs[core.Linux] = b
						dc, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+cfg.Linux.DockerSocket), dockerclient.WithAPIVersionNegotiation())
						if err != nil {
							return nil, nil, err
						}
						gc, err := imagegc.New(imagegc.Config{
							Docker:        dc,
							Gates:         []imagegc.PullGate{b.PullGate()},
							HighWatermark: cfg.ImageGC.HighWatermark,
							Interval:      cfg.ImageGC.Interval.D(),
							UnusedFor:     cfg.ImageGC.UnusedFor.D(),
							Protect:       []string{cfg.Linux.Image},
							Log:           log.With("component", "imagegc"),
						})
						if err != nil {
							return nil, nil, err
						}
						loops["imagegc"] = func(ctx context.Context) error { gc.Loop(ctx); return ctx.Err() }
					}
					var win daemon.WindowsPool
					if cfg.Windows.Enabled {
						p, err := newWindowsPool(cfg, hostID, log)
						if err != nil {
							return nil, nil, err
						}
						bs[core.Windows] = p
						win = p
					}
					return bs, win, nil
				},
				Loops: loops,
			})
			if err != nil {
				return err
			}
			defer d.Close()
			return d.Run(cmd.Context())
		},
	}
}

// prepareCaches creates cache directories with the configured owner and
// mode and returns the binds and env vars for runners and job containers.
func prepareCaches(cfg *config.Config) ([]string, map[string]string, error) {
	var binds []string
	env := map[string]string{}
	for _, c := range cfg.Caches {
		if c.Mode != "shared" {
			return nil, nil, fmt.Errorf("cache %q: mode %q is not supported yet; use shared", c.Name, c.Mode)
		}
		mode, err := strconv.ParseUint(c.DirMode, 8, 32)
		if err != nil {
			return nil, nil, fmt.Errorf("cache %q: dir_mode: %w", c.Name, err)
		}
		if err := os.MkdirAll(c.HostPath, 0o755); err != nil {
			return nil, nil, err
		}
		if err := os.Chown(c.HostPath, c.UID, c.GID); err != nil && !errors.Is(err, fs.ErrPermission) {
			return nil, nil, err
		}
		perm := fs.FileMode(mode) & fs.ModePerm
		if mode&0o2000 != 0 {
			perm |= fs.ModeSetgid
		}
		if err := os.Chmod(c.HostPath, perm); err != nil && !errors.Is(err, fs.ErrPermission) {
			return nil, nil, err
		}
		binds = append(binds, c.HostPath+":"+c.ContainerPath)
		if c.Env != "" {
			env[c.Env] = c.ContainerPath
		}
	}
	return binds, env, nil
}
