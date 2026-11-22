package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"

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
	// The alternate screen (as top and watch use) keeps frames out of the
	// scrollback; each frame overwrites the last in place.
	fmt.Fprint(out, "\x1b[?1049h\x1b[?25l")
	defer fmt.Fprint(out, "\x1b[?25h\x1b[?1049l")
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
		w, h := termSize()
		_, _ = io.WriteString(out, frame(buf.String(), w, h))
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

// termSize is the terminal's width and height, 0 when unknown.
var termSize = func() (int, int) {
	ws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return 0, 0
	}
	return int(ws.Col), int(ws.Row)
}

// frame turns text into one in-place redraw: cursor home, every line cut to
// the width and cleared to its end, then the rest of the screen cleared.
// Lines past the height are dropped (a frame that scrolls would smear), with
// a note saying how many.
func frame(text string, width, height int) string {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if height > 1 && len(lines) > height {
		hidden := len(lines) - height + 1
		lines = append(lines[:height-1], fmt.Sprintf("… %d more lines: enlarge the window, or use --once", hidden))
	}
	var b strings.Builder
	b.WriteString("\x1b[H")
	for i, l := range lines {
		if width > 0 {
			if r := []rune(l); len(r) > width {
				l = string(r[:width])
			}
		}
		b.WriteString(l)
		b.WriteString("\x1b[K")
		if i < len(lines)-1 {
			b.WriteString("\r\n")
		}
	}
	b.WriteString("\x1b[J")
	return b.String()
}

// machineOf names where a runner runs: its VM or remote host, else this host
// for Linux.
func machineOf(rep daemon.StatusReport, r daemon.RunnerStatus) string {
	if r.OS != "linux" || r.VM != "" {
		return dash(r.VM)
	}
	return dash(rep.Hostname)
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
	if l := cpuLine(s); l != "" {
		fmt.Fprintf(out, "cpu      %s\n", l)
	}
	fmt.Fprintf(out, "jobs     %d held, %d parked · %d runners\n", s.Held, s.Parked, s.Runners)

	busyOn := map[string]int{}
	machine := map[string]string{} // runner -> machine
	for _, r := range rep.Runners {
		m := machineOf(rep, r)
		machine[r.Name] = m
		if r.RunID != 0 {
			busyOn[m]++
		}
	}
	fmt.Fprintln(out)
	mw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	fmt.Fprintln(mw, "MACHINE\tKIND\tSTATE\tRESERVED\tJOBS")
	fmt.Fprintf(mw, "%s\t%s\t%s\t%s\t%d\n", dash(rep.Hostname), "this host", "up", size(s.Ledger.Reserved)+" of "+size(s.Ledger.Budget), busyOn[dash(rep.Hostname)])
	for _, h := range rep.LinuxHostNames {
		state := "reachable"
		if !rep.LinuxHosts[h] {
			state = "UNREACHABLE"
		}
		fmt.Fprintf(mw, "%s\t%s\t%s\t%s\t%d\n", h, "linux host", state, "-", busyOn[h])
	}
	for _, h := range rep.MacHostNames {
		state := "reachable"
		if !rep.MacHosts[h] {
			state = "UNREACHABLE"
		}
		fmt.Fprintf(mw, "%s\t%s\t%s\t%s\t%d\n", h, "macos host", state, "-", busyOn[h])
	}
	vms := rep.WindowsVMs
	if len(vms) == 0 { // daemon older than the client
		vms = slices.Sorted(maps.Keys(rep.VMsUp))
	}
	for _, vm := range vms {
		state, reserved := "stopped", "-"
		if mem, up := rep.VMsUp[vm]; up {
			state, reserved = "up", size(mem)
		}
		fmt.Fprintf(mw, "%s\t%s\t%s\t%s\t%d\n", vm, "windows vm", state, reserved, busyOn[vm])
	}
	for _, h := range slices.Sorted(maps.Keys(rep.WindowsHosts)) {
		state := "reachable"
		if !rep.WindowsHosts[h] {
			state = "UNREACHABLE"
		}
		fmt.Fprintf(mw, "%s\t%s\t%s\t%s\t%d\n", h, "windows host", state, "-", busyOn[h])
	}
	_ = mw.Flush()

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
		fmt.Fprintln(w, "LANE\tJOB\tSTATE\tCOST\tAGE\tMACHINE\tRUNNER")
		for _, l := range busy {
			for _, j := range l.Jobs {
				name := j.Repo + "/" + j.Workflow + " " + j.Name
				if j.Phantom {
					name = j.Repo + " (not yet known)"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", l.Name, name, j.State, jobCost(j), j.Age.Truncate(time.Second), dash(machine[j.Runner]), dash(j.Runner))
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
		fmt.Fprintln(w, "RUNNER\tMACHINE\tTARGET\tOS\tSTATE\tMEM\tPEAK\tOOM\tUP")
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
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s\n", r.Name, machineOf(rep, r), r.Target, r.OS, state,
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
	return line
}

// cpuLine summarises shared CPU and pinned cores, "" when neither is in use.
func cpuLine(s scheduler.Status) string {
	u := s.Ledger
	var parts []string
	if u.CPUBudget > 0 {
		parts = append(parts, fmt.Sprintf("%s of %s reserved, %s free", cpus(u.CPUReserved), cpus(u.CPUBudget), cpus(u.CPUBudget-u.CPUReserved)))
	}
	if u.TotalCores > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d cores pinned", u.PinnedCores, u.TotalCores))
	}
	return strings.Join(parts, " · ")
}

// cpus formats millicpus as logical CPUs ("4", "1.5").
func cpus(m int64) string { return strconv.FormatFloat(float64(m)/1000, 'f', -1, 64) }

// jobCost is the job's memory, plus its CPU or pinned cores.
func jobCost(j scheduler.JobStatus) string {
	switch {
	case j.Pinned > 0:
		return fmt.Sprintf("%s %dc", size(j.Cost), j.Pinned)
	case j.CPU > 0:
		return size(j.Cost) + " " + cpus(j.CPU) + "cpu"
	}
	return size(j.Cost)
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
