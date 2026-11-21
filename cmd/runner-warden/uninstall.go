package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	dockerclient "github.com/docker/docker/client"
	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/daemon"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/naming"
)

func uninstallCmd() *cobra.Command {
	var purge bool
	c := &cobra.Command{
		Use:   "uninstall",
		Short: "Stop the service and remove its scale sets, containers, VMs and slice",
		Long: "Stops runner-warden@<instance>, deletes its scale sets on GitHub (runners that are " +
			"mid-job are stopped), removes every container, volume and network carrying this " +
			"instance's labels (including Windows VM containers; VM disks are kept), and removes " +
			"the job slice. --purge also deletes the config, token and state dir. Shared cache " +
			"directories are never deleted.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			out := cmd.OutOrStdout()
			unit := "runner-warden@" + cfg.Instance
			_ = exec.CommandContext(ctx, "systemctl", "disable", "--now", unit).Run()
			fmt.Fprintf(out, "✅ stopped %s\n", unit)

			if token, err := gh.Token(cfg.GitHub.TokenFile); err == nil {
				deleteScaleSets(ctx, cfg, token, out)
			} else {
				fmt.Fprintf(out, "⚠️  no token, scale sets left on GitHub: %v\n", err)
			}
			if err := removeOwned(ctx, cfg, out); err != nil {
				fmt.Fprintf(out, "⚠️  docker cleanup: %v\n", err)
			}
			slice := naming.Slice(cfg.Instance)
			_ = exec.CommandContext(ctx, "systemctl", "stop", slice).Run()
			_ = os.Remove(filepath.Join("/etc/systemd/system", slice))
			_ = exec.CommandContext(ctx, "systemctl", "daemon-reload").Run()
			fmt.Fprintf(out, "✅ removed %s\n", slice)
			if purge {
				for _, p := range []string{filepath.Dir(config.Path(cfg.Instance)), cfg.StateDir, cfg.CacheDir, filepath.Join(config.RunDir, cfg.Instance)} {
					if err := os.RemoveAll(p); err != nil {
						fmt.Fprintf(out, "⚠️  remove %s: %v\n", p, err)
					}
				}
				fmt.Fprintln(out, "✅ purged config, token and state")
			}
			return nil
		},
	}
	c.Flags().BoolVar(&purge, "purge", false, "also delete config, token and state (VM disks under the state dir included)")
	return c
}

func deleteScaleSets(ctx context.Context, cfg *config.Config, token string, out interface{ Write([]byte) (int, error) }) {
	lanes := cfg.Limits.Lanes
	if lanes <= 0 {
		lanes = 4
	}
	for n := 1; n <= lanes; n++ {
		sets := gh.NewScaleSets(token, daemon.Version)
		for _, t := range cfg.Targets {
			for _, os := range t.OSes() {
				key := core.ScaleSetKey{Target: t.Core(), OS: os}
				name := naming.LaneName(cfg.Prefix, os, n)
				if _, err := sets.Ensure(ctx, key, name, naming.LaneLabels(cfg.Prefix, os), key.Target.RunnerGroup); err != nil {
					fmt.Fprintf(out, "⚠️  %s %s: %v\n", key.Target, name, err)
					continue
				}
				if err := sets.Delete(ctx, key); err != nil {
					fmt.Fprintf(out, "⚠️  delete %s %s: %v\n", key.Target, name, err)
					continue
				}
				fmt.Fprintf(out, "✅ deleted scale set %s on %s\n", name, key.Target)
			}
		}
	}
}

// removeOwned force-removes everything labelled with this instance.
func removeOwned(ctx context.Context, cfg *config.Config, out interface{ Write([]byte) (int, error) }) error {
	dc, err := dockerclient.NewClientWithOpts(dockerclient.WithHost("unix://"+cfg.Linux.DockerSocket), dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return err
	}
	f := filters.NewArgs(filters.Arg("label", naming.MetaInstance+"="+cfg.Instance), filters.Arg("label", naming.MetaManaged+"=1"))
	cs, err := dc.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil {
		return err
	}
	for _, c := range cs {
		if err := dc.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true, RemoveVolumes: true}); err == nil {
			fmt.Fprintf(out, "✅ removed container %s\n", c.Names)
		}
	}
	ns, _ := dc.NetworkList(ctx, network.ListOptions{Filters: f})
	for _, n := range ns {
		_ = dc.NetworkRemove(ctx, n.ID)
	}
	vs, _ := dc.VolumeList(ctx, volume.ListOptions{Filters: f})
	for _, v := range vs.Volumes {
		_ = dc.VolumeRemove(ctx, v.Name, true)
	}
	return nil
}
