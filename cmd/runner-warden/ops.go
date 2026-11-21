package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
)

func drainCmd() *cobra.Command {
	var off bool
	c := &cobra.Command{
		Use:   "drain",
		Short: "Stop taking new jobs (running jobs finish); --off resumes",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := controlClient()
			if err != nil {
				return err
			}
			return cl.Drain(cmd.Context(), !off)
		},
	}
	c.Flags().BoolVar(&off, "off", false, "resume taking jobs")
	return c
}

func killCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "kill <runner>",
		Short: "Cancel the runner's job and stop it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cl, err := controlClient()
			if err != nil {
				return err
			}
			return cl.Kill(cmd.Context(), args[0])
		},
	}
}

func logsCmd() *cobra.Command {
	var follow bool
	var grep, since string
	c := &cobra.Command{
		Use:   "logs [runner|run-id]",
		Short: "Show service logs, optionally filtered to a runner or run id",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst, err := resolveInstance()
			if err != nil {
				return err
			}
			a := []string{"-u", "runner-warden@" + inst, "-o", "cat", "--no-pager"}
			if follow {
				a = append(a, "-f")
			}
			if since != "" {
				a = append(a, "--since", since)
			}
			if len(args) == 1 {
				grep = args[0]
			}
			if grep != "" {
				a = append(a, "--grep", grep)
			}
			j := exec.CommandContext(cmd.Context(), "journalctl", a...)
			j.Stdout, j.Stderr = cmd.OutOrStdout(), cmd.ErrOrStderr()
			return j.Run()
		},
	}
	c.Flags().BoolVarP(&follow, "follow", "f", false, "follow")
	c.Flags().StringVar(&grep, "grep", "", "filter")
	c.Flags().StringVar(&since, "since", "", "journalctl --since value (e.g. 1h, today)")
	return c
}

func checkCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Health check: service, daemon, slice, Docker, disk",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			results := doctor(cmd.Context(), cfg, true)
			printChecks(cmd.OutOrStdout(), results)
			if failed(results) {
				return exitCode(2)
			}
			return nil
		},
	}
}

func configCmd() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Inspect the config"}
	c.AddCommand(&cobra.Command{
		Use:   "validate [file]",
		Short: "Validate a config file (default: the instance's)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			path := ""
			if len(args) == 1 {
				path = args[0]
			} else {
				inst, err := resolveInstance()
				if err != nil {
					return err
				}
				path = config.Path(inst)
			}
			if _, err := config.Load(path); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "ok")
			return nil
		},
	})
	return c
}

var _ = os.Stdout
