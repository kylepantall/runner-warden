package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
)

func statusCmd() *cobra.Command {
	var asJSON bool
	c := &cobra.Command{
		Use:   "status",
		Short: "Show lanes, jobs, runners and memory",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := controlClient()
			if err != nil {
				return err
			}
			var rep daemon.StatusReport
			if err := cl.Status(cmd.Context(), &rep); err != nil {
				return fmt.Errorf("daemon not reachable: %w", err)
			}
			out := cmd.OutOrStdout()
			if asJSON {
				e := json.NewEncoder(out)
				e.SetIndent("", "  ")
				return e.Encode(rep)
			}
			s := rep.Scheduler
			fmt.Fprintf(out, "instance %s  prefix %s  host %s  draining=%v\n", rep.Instance, rep.Prefix, rep.HostID, s.Draining)
			fmt.Fprintf(out, "memory   %s\n", ledgerLine(s))
			fmt.Fprintf(out, "jobs     %d held, %d parked; %d runners\n\n", s.Held, s.Parked, s.Runners)
			w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
			fmt.Fprintln(w, "LANE\tCAP\tSTARTED\tJOB\tSTATE\tCOST\tAGE\tRUNNER")
			for _, l := range s.Lanes {
				if len(l.Jobs) == 0 {
					fmt.Fprintf(w, "%s\t%d\t%d\t-\t-\t-\t-\t-\n", l.Name, l.Capacity, l.Started)
					continue
				}
				for _, j := range l.Jobs {
					fmt.Fprintf(w, "%s\t%d\t%d\t%s/%s %s\t%s\t%s\t%s\t%s\n", l.Name, l.Capacity, l.Started,
						j.Repo, j.Workflow, j.Name, j.State, units.BytesSize(float64(j.Cost)), j.Age.Truncate(time.Second), j.Runner)
				}
			}
			w.Flush()
			if len(rep.Runners) > 0 {
				fmt.Fprintln(out)
				w = tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "RUNNER\tTARGET\tOS\tSTATE\tMEM\tPEAK\tOOM\tUP")
				sort.Slice(rep.Runners, func(i, j int) bool { return rep.Runners[i].Started.Before(rep.Runners[j].Started) })
				for _, r := range rep.Runners {
					state := "idle"
					if r.RunID != 0 {
						state = "busy"
					}
					if !r.Alive {
						state = "exited"
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, r.Target, r.OS, state,
						units.BytesSize(float64(r.Mem)), units.BytesSize(float64(r.MemPeak)), r.OOM, time.Since(r.Started).Truncate(time.Second))
				}
				w.Flush()
			}
			if len(rep.Persistent) > 0 {
				fmt.Fprintln(out)
				w = tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(w, "PERSISTENT\tMEM")
				for _, c := range rep.Persistent {
					fmt.Fprintf(w, "%s\t%s\n", c.Name, units.BytesSize(float64(c.Mem)))
				}
				w.Flush()
			}
			for vm, mem := range rep.VMsUp {
				fmt.Fprintf(out, "windows vm %s up (%s reserved)\n", vm, units.BytesSize(float64(mem)))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return c
}

func ledgerLine(s any) string {
	b, _ := json.Marshal(s)
	var v struct {
		Ledger map[string]any `json:"ledger"`
	}
	_ = json.Unmarshal(b, &v)
	var parts []string
	for _, k := range []string{"budget", "used", "free", "external"} {
		if x, ok := v.Ledger[k].(float64); ok {
			parts = append(parts, fmt.Sprintf("%s %s", k, units.BytesSize(x)))
		}
	}
	if len(parts) == 0 {
		return fmt.Sprint(v.Ledger)
	}
	return strings.Join(parts, ", ")
}

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
