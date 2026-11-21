package main

import (
	"errors"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/dockerproxy"
)

// proxyCmd serves one runner's Docker API proxy. The daemon starts it as a
// transient systemd unit, outside its own cgroup, so restarting the daemon
// does not cut the runner's running docker exec/attach streams.
func proxyCmd() *cobra.Command {
	var spec string
	c := &cobra.Command{
		Use:    "proxy",
		Short:  "Serve a runner's Docker proxy (started by the daemon)",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if spec == "" {
				return errors.New("--spec is required")
			}
			s, err := dockerproxy.ReadSpec(spec)
			if err != nil {
				return err
			}
			return dockerproxy.Serve(cmd.Context(), s, dockerproxy.ServeOptions{Log: newLogger(s.LogLevel).With("component", "dockerproxy")})
		},
	}
	c.Flags().StringVar(&spec, "spec", "", "path to the JSON proxy spec")
	return c
}
