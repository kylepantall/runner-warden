package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/scheduler"
)

func statusCmd() *cobra.Command {
	var asJSON, once bool
	var every time.Duration
	c := &cobra.Command{
		Use:   "status",
		Short: "Watch lanes, jobs, runners and memory (--once or --json for a snapshot)",
		Long: "In a terminal, status refreshes a live table every --interval until Ctrl-C. " +
			"--once prints the table once (also the default when output is not a terminal); " +
			"--json prints one JSON snapshot.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cl, err := controlClient()
			if err != nil {
				return err
			}
			fetch := func(ctx context.Context) (daemon.StatusReport, error) {
				var rep daemon.StatusReport
				if err := cl.Status(ctx, &rep); err != nil {
					return rep, fmt.Errorf("daemon not reachable: %w", err)
				}
				return rep, nil
			}
			out := cmd.OutOrStdout()
			if asJSON || once || !isTTY(os.Stdout) {
				rep, err := fetch(cmd.Context())
				if err != nil {
					return err
				}
				if asJSON {
					e := json.NewEncoder(out)
					e.SetIndent("", "  ")
					return e.Encode(rep)
				}
				renderStatus(out, rep)
				return nil
			}
			return watchStatus(cmd.Context(), out, every, fetch)
		},
	}
	c.Flags().BoolVar(&asJSON, "json", false, "print one JSON snapshot")
	c.Flags().BoolVar(&once, "once", false, "print the table once instead of watching")
	c.Flags().DurationVarP(&every, "interval", "n", 2*time.Second, "refresh interval while watching")
	return c
}

// watchStatus redraws the status table until ctx ends or Ctrl-C. A failed
// fetch is shown in place and retried; it does not end the watch.
func watchStatus(ctx context.Context, out io.Writer, every time.Duration, fetch func(context.Context) (daemon.StatusReport, error)) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if every < 200*time.Millisecond {
		every = 200 * time.Millisecond
	}
	fmt.Fprint(out, "\x1b[?25l") // hide the cursor while redrawing
	defer fmt.Fprint(out, "\x1b[?25h\n")
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "runner-warden status · %s · every %s · Ctrl-C to quit\n\n", time.Now().Format("15:04:05"), every)
		if rep, err := fetch(ctx); err != nil {
			fmt.Fprintf(&buf, "%v (retrying)\n", err)
		} else {
			renderStatus(&buf, rep)
		}
		fmt.Fprint(out, "\x1b[H\x1b[2J")
		_, _ = out.Write(buf.Bytes())
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// renderStatus prints the status tables. Lanes without jobs are summarised
// in one line so busy lanes stay readable.
func renderStatus(out io.Writer, rep daemon.StatusReport) {
	s := rep.Scheduler
	fmt.Fprintf(out, "instance %s  prefix %s  host %s", rep.Instance, rep.Prefix, rep.HostID)
	if s.Draining {
		fmt.Fprint(out, "  DRAINING")
	}
	fmt.Fprintf(out, "\nmemory   %s\n", memoryLine(s))
	fmt.Fprintf(out, "jobs     %d held, %d parked · %d runners\n", s.Held, s.Parked, s.Runners)
	for vm, mem := range rep.VMsUp {
		fmt.Fprintf(out, "windows  vm %s up (%s reserved)\n", vm, size(mem))
	}

	idle := 0
	var busy []scheduler.LaneStatus
	for _, l := range s.Lanes {
		if len(l.Jobs) == 0 {
			idle++
			continue
		}
		busy = append(busy, l)
	}
	fmt.Fprintln(out)
	if len(busy) == 0 {
		fmt.Fprintf(out, "no jobs (%d idle lanes)\n", idle)
	} else {
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "LANE\tJOB\tSTATE\tCOST\tAGE\tRUNNER")
		for _, l := range busy {
			for _, j := range l.Jobs {
				name := j.Repo + "/" + j.Workflow + " " + j.Name
				if j.Phantom {
					name = j.Repo + " (not yet known)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", l.Name, name, j.State, size(j.Cost), j.Age.Truncate(time.Second), dash(j.Runner))
			}
		}
		_ = w.Flush()
		if idle > 0 {
			fmt.Fprintf(out, "+ %d idle lanes\n", idle)
		}
	}

	if len(rep.Runners) > 0 {
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "RUNNER\tTARGET\tOS\tSTATE\tMEM\tPEAK\tOOM\tUP")
		rs := append([]daemon.RunnerStatus(nil), rep.Runners...)
		sort.Slice(rs, func(i, j int) bool { return rs[i].Started.Before(rs[j].Started) })
		for _, r := range rs {
			state := "idle"
			if r.RunID != 0 {
				state = "busy"
			}
			if !r.Alive {
				state = "exited"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, r.Target, r.OS, state,
				size(r.Mem), size(r.MemPeak), r.OOM, time.Since(r.Started).Truncate(time.Second))
		}
		_ = w.Flush()
	}
	if len(rep.Persistent) > 0 {
		fmt.Fprintln(out)
		w := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
		fmt.Fprintln(w, "PERSISTENT\tMEM")
		for _, c := range rep.Persistent {
			fmt.Fprintf(w, "%s\t%s\n", c.Name, size(c.Mem))
		}
		_ = w.Flush()
	}
}

func memoryLine(s scheduler.Status) string {
	u := s.Ledger
	line := fmt.Sprintf("%s reserved of %s, %s free", size(u.Reserved), size(u.Budget), size(u.Free))
	if u.External > 0 {
		line += fmt.Sprintf(", %s outside runners", size(u.External))
	}
	if u.TotalCores > 0 {
		line += fmt.Sprintf(" · %d/%d cores pinned", u.PinnedCores, u.TotalCores)
	}
	return line
}

func size(b int64) string {
	if b < 0 {
		return "-" + units.BytesSize(float64(-b))
	}
	return units.BytesSize(float64(b))
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
