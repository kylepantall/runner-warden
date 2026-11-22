package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/backend/winvm"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/hostsetup"
	"github.com/kylepantall/runner-warden/internal/naming"
)

func hostCmd() *cobra.Command {
	c := &cobra.Command{Use: "host", Short: "Link other machines (Linux, WSL, Windows, Mac) to this instance"}
	c.AddCommand(hostAddCmd(), hostListCmd())
	return c
}

type hostAddFlags struct {
	kind, address, user, name, targets, distro, vm, tart string
	jobs                                                 int
	yes, noRestart                                       bool
}

func hostAddCmd() *cobra.Command {
	var f hostAddFlags
	c := &cobra.Command{
		Use:   "add",
		Short: "Wizard: link a machine over SSH, check and prepare it, add it to the config",
		Long: "Asks what kind of machine to link and how to reach it, shows the one step to run on it " +
			"(trusting this instance's SSH key), checks it over SSH and offers to fix what is missing, " +
			"then adds it to the config and restarts the service. Every question has a flag, so it also runs " +
			"unattended with --yes.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			inst, err := resolveInstance()
			if err != nil {
				return err
			}
			w := &hostWizard{
				out: cmd.OutOrStdout(), in: bufio.NewReader(cmd.InOrStdin()), f: f,
				path: config.Path(inst), inst: inst,
				dial: func(addr, user, keyFile, knownHosts string) (hostsetup.Commander, error) {
					signer, _, err := winvm.EnsureKey(keyFile)
					if err != nil {
						return nil, err
					}
					return winvm.NewSSHCommander(winvm.SSHConfig{Addr: addr, User: user, Signer: signer, KnownHosts: knownHosts, DialTimeout: 15 * time.Second}), nil
				},
				restart: func(ctx context.Context) error {
					unit := "runner-warden@" + inst
					if out, err := exec.CommandContext(ctx, "systemctl", "restart", unit).CombinedOutput(); err != nil {
						return fmt.Errorf("restart %s: %w: %s", unit, err, out)
					}
					return nil
				},
				setupWindows: func(ctx context.Context, cfg *config.Config, name string) error {
					hostID, err := hostIDForCLI(cfg)
					if err != nil {
						return err
					}
					p, err := newWindowsPool(cfg, hostID, newLogger("warn"))
					if err != nil {
						return err
					}
					return p.SetupHost(ctx, name, "")
				},
				waitUp: waitHostUp,
			}
			return w.run(cmd.Context())
		},
	}
	fl := c.Flags()
	fl.StringVar(&f.kind, "kind", "", "linux, wsl, windows or macos")
	fl.StringVar(&f.address, "address", "", "host name or IP (a Tailscale name works well), optionally :port")
	fl.StringVar(&f.user, "user", "", "SSH user on the machine")
	fl.StringVar(&f.name, "name", "", "name for the machine in this instance")
	fl.StringVar(&f.targets, "targets", "", "comma-separated owners or owner/repo it serves (default: all targets)")
	fl.StringVar(&f.distro, "distro", "", "WSL distro (wsl)")
	fl.StringVar(&f.vm, "vm", "", "tart VM (macos)")
	fl.StringVar(&f.tart, "tart", "/opt/homebrew/bin/tart", "tart binary on the Mac (macos)")
	fl.IntVar(&f.jobs, "jobs", 0, "jobs at once (linux, wsl; default 2)")
	fl.BoolVar(&f.yes, "yes", false, "accept defaults and every fix without asking")
	fl.BoolVar(&f.noRestart, "no-restart", false, "edit the config only; restart the service yourself")
	return c
}

type hostWizard struct {
	out  io.Writer
	in   *bufio.Reader
	f    hostAddFlags
	path string
	inst string

	dial         func(addr, user, keyFile, knownHosts string) (hostsetup.Commander, error)
	restart      func(ctx context.Context) error
	setupWindows func(ctx context.Context, cfg *config.Config, name string) error
	waitUp       func(ctx context.Context, name string, limit time.Duration) error
}

// ask prompts with a default; with --yes, or a flag value, it does not ask.
func (w *hostWizard) ask(q, flagVal, def string) string {
	if flagVal != "" {
		return flagVal
	}
	if w.f.yes {
		return def
	}
	if def != "" {
		q += " [" + def + "]"
	}
	if a := prompt(w.out, w.in, q); a != "" {
		return a
	}
	return def
}

// yesNo asks a question that defaults to yes.
func (w *hostWizard) yesNo(q string) bool {
	if w.f.yes {
		fmt.Fprintf(w.out, "%s [Y/n]: y\n", q)
		return true
	}
	a := strings.ToLower(prompt(w.out, w.in, q+" [Y/n]"))
	return a == "" || a == "y" || a == "yes"
}

func (w *hostWizard) choose(q string, opts []string, flagVal string) (string, error) {
	if flagVal != "" {
		if !slices.Contains(opts, flagVal) {
			return "", fmt.Errorf("%q is not one of %s", flagVal, strings.Join(opts, ", "))
		}
		return flagVal, nil
	}
	if w.f.yes || len(opts) == 1 {
		return opts[0], nil
	}
	fmt.Fprintln(w.out, q)
	for i, o := range opts {
		fmt.Fprintf(w.out, "  %d) %s\n", i+1, o)
	}
	for {
		a := prompt(w.out, w.in, "Choose [1]")
		if a == "" {
			return opts[0], nil
		}
		if n, err := strconv.Atoi(a); err == nil && n >= 1 && n <= len(opts) {
			return opts[n-1], nil
		}
		if slices.Contains(opts, a) {
			return a, nil
		}
		fmt.Fprintln(w.out, "Pick a number from the list.")
	}
}

var kindLabels = map[hostsetup.Kind]string{
	hostsetup.KindLinux:   "linux   - a Linux machine with Docker (overflow Linux jobs)",
	hostsetup.KindWSL:     "wsl     - Linux jobs in WSL on a Windows PC",
	hostsetup.KindWindows: "windows - Windows jobs on a Windows PC",
	hostsetup.KindMac:     "macos   - macOS jobs in a tart VM on a Mac",
}

func (w *hostWizard) kind() (hostsetup.Kind, error) {
	kinds := []hostsetup.Kind{hostsetup.KindLinux, hostsetup.KindWSL, hostsetup.KindWindows, hostsetup.KindMac}
	if w.f.kind != "" {
		if !slices.Contains(kinds, hostsetup.Kind(w.f.kind)) {
			return "", fmt.Errorf("--kind must be linux, wsl, windows or macos")
		}
		return hostsetup.Kind(w.f.kind), nil
	}
	if w.f.yes {
		return "", errors.New("--kind is required with --yes")
	}
	labels := make([]string, len(kinds))
	for i, k := range kinds {
		labels[i] = kindLabels[k]
	}
	pick, err := w.choose("What kind of machine?", labels, "")
	if err != nil {
		return "", err
	}
	return kinds[slices.Index(labels, pick)], nil
}

func defaultName(address string, k hostsetup.Kind) string {
	n := strings.ToLower(strings.SplitN(strings.SplitN(address, ".", 2)[0], ":", 2)[0])
	n = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' {
			return r
		}
		return '-'
	}, n)
	if k == hostsetup.KindWSL {
		n += "-wsl"
	}
	return strings.Trim(n, "-")
}

func sshAddr(a string) string {
	if strings.Contains(a, ":") {
		return a
	}
	return a + ":22"
}

func (w *hostWizard) run(ctx context.Context) error {
	cfg, err := config.Load(w.path)
	if err != nil {
		return err
	}
	step(w.out, "Machine")
	k, err := w.kind()
	if err != nil {
		return err
	}
	h := hostsetup.Host{Kind: k}
	if h.Address = w.ask("Address (host name or IP; a Tailscale name works well)", w.f.address, ""); h.Address == "" {
		return errors.New("an address is required")
	}
	defUser := map[hostsetup.Kind]string{hostsetup.KindLinux: "root"}[k]
	if h.SSHUser = w.ask("SSH user on that machine", w.f.user, defUser); h.SSHUser == "" {
		return errors.New("an SSH user is required")
	}
	h.Name = w.ask("Name for it here", w.f.name, defaultName(h.Address, k))
	if err := naming.ValidatePrefix(h.Name); err != nil {
		return fmt.Errorf("name %q: use lowercase letters, digits and hyphens", h.Name)
	}

	keyFile, known := cfg.Linux.SSHKeyFile, filepath.Join(cfg.StateDir, "linux", h.Name, "known_hosts")
	switch k {
	case hostsetup.KindWindows:
		keyFile, known = cfg.Windows.SSHKeyFile, filepath.Join(cfg.StateDir, "windows", h.Name, "known_hosts")
	case hostsetup.KindMac:
		keyFile, known = cfg.MacOS.SSHKeyFile, filepath.Join(cfg.StateDir, "macos", h.Name, "known_hosts")
	}
	_, pub, err := winvm.EnsureKey(keyFile)
	if err != nil {
		return err
	}
	step(w.out, "Trust this instance's key on "+h.Address)
	if err := w.keyStep(cfg, h, pub); err != nil {
		return err
	}
	c, err := w.dial(sshAddr(h.Address), h.SSHUser, keyFile, known)
	if err != nil {
		return err
	}
	for {
		_, errs, code, err := c.Command(ctx, "echo ok", "")
		if err == nil && code == 0 {
			fmt.Fprintf(w.out, "✅ SSH to %s@%s works\n", h.SSHUser, h.Address)
			break
		}
		if err == nil {
			err = fmt.Errorf("exit %d: %s", code, strings.TrimSpace(errs))
		}
		fmt.Fprintf(w.out, "❌ SSH failed: %v\n", err)
		if w.f.yes || !w.yesNo("Try again?") {
			return errors.New("cannot reach the machine over SSH")
		}
	}

	step(w.out, "Check "+h.Name)
	var checks []hostsetup.Check
	switch k {
	case hostsetup.KindLinux:
		h.Exec = "sudo -n"
		if h.SSHUser == "root" {
			h.Exec = "env"
		}
		checks = hostsetup.LinuxChecks(ctx, c, h.Exec, "")
	case hostsetup.KindWSL:
		ds, err := hostsetup.WSLDistros(ctx, c)
		if err != nil {
			return fmt.Errorf("list WSL distros: %w", err)
		}
		if len(ds) == 0 {
			return errors.New("no WSL distro found (install one: wsl --install -d Ubuntu)")
		}
		d, err := w.choose("Which WSL distro?", ds, w.f.distro)
		if err != nil {
			return err
		}
		h.Exec = "wsl -d " + d + " -u root --"
		checks = append(hostsetup.LinuxChecks(ctx, c, h.Exec, d), hostsetup.WSLKeepaliveCheck(ctx, c, d))
	case hostsetup.KindMac:
		vms, err := hostsetup.TartVMs(ctx, c, w.f.tart)
		if err != nil {
			return fmt.Errorf("tart on the Mac (%s): %w", w.f.tart, err)
		}
		var names []string
		for _, v := range vms {
			if v.Running {
				names = append(names, v.Name)
			}
		}
		if len(names) == 0 {
			return errors.New("no running tart VM: start one (tart run <vm> --no-graphics, kept up by a launchd agent) and try again")
		}
		if h.VM, err = w.choose("Which tart VM?", names, w.f.vm); err != nil {
			return err
		}
		checks, h.GuestUID = hostsetup.MacChecks(ctx, c, w.f.tart, h.VM)
	case hostsetup.KindWindows:
		checks = []hostsetup.Check{{Name: "OpenSSH", OK: true, Detail: "baseline and runner files are installed after the config is saved"}}
	}
	if err := w.resolve(ctx, checks); err != nil {
		return err
	}

	step(w.out, "Jobs")
	var all []string
	for _, t := range cfg.Targets {
		all = append(all, t.Org+t.Repo)
	}
	h.Targets = splitList(w.ask("Orgs or repos it serves (comma separated)", w.f.targets, strings.Join(all, ",")))
	if len(h.Targets) == 0 {
		return errors.New("it must serve at least one org or repo")
	}
	if k == hostsetup.KindLinux || k == hostsetup.KindWSL {
		def := 2
		if w.f.jobs > 0 {
			def = w.f.jobs
		}
		n, err := strconv.Atoi(w.ask("Jobs at once", "", strconv.Itoa(def)))
		if err != nil || n < 1 {
			return errors.New("jobs at once must be a positive number")
		}
		h.MaxConcurrent = n
	}
	return w.save(ctx, h)
}

// keyStep shows what to run on the machine so it trusts our key.
func (w *hostWizard) keyStep(cfg *config.Config, h hostsetup.Host, pub string) error {
	line := "mkdir -p ~/.ssh && chmod 700 ~/.ssh && echo '" + pub + "' >> ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys"
	switch h.Kind {
	case hostsetup.KindLinux:
		fmt.Fprintf(w.out, "On %s, as %s, run:\n\n  %s\n\n", h.Address, h.SSHUser, line)
		if h.SSHUser != "root" {
			fmt.Fprintf(w.out, "%s also needs sudo without a password (or link it as root).\n", h.SSHUser)
		}
	case hostsetup.KindMac:
		fmt.Fprintf(w.out, "On the Mac: System Settings › General › Sharing › turn on Remote Login.\nThen in Terminal, as %s, run:\n\n  %s\n\n", h.SSHUser, line)
	case hostsetup.KindWindows, hostsetup.KindWSL:
		script := filepath.Join(cfg.StateDir, h.Name+"-setup.ps1")
		if err := os.WriteFile(script, []byte(winvm.HostScript(pub)), 0o644); err != nil { //nolint:gosec // a public key and setup steps, meant to be copied
			return err
		}
		fmt.Fprintf(w.out, "On the PC, in PowerShell opened as Administrator (signed in as %s), run this.\nIt turns on OpenSSH Server and trusts this instance's key (also saved to %s):\n\n%s\n", h.SSHUser, script, winvm.HostScript(pub))
	}
	if !w.f.yes {
		prompt(w.out, w.in, "Press Enter when that is done")
	}
	return nil
}

// resolve prints the checks and runs the fixes the operator accepts.
func (w *hostWizard) resolve(ctx context.Context, checks []hostsetup.Check) error {
	for _, ch := range checks {
		if ch.OK {
			fmt.Fprintf(w.out, "✅ %s: %s\n", ch.Name, ch.Detail)
			continue
		}
		fmt.Fprintf(w.out, "❌ %s: %s\n", ch.Name, ch.Detail)
		if ch.Fix == nil {
			if ch.Name == "Xcode in the guest" {
				continue // optional
			}
			return fmt.Errorf("fix %q on the machine, then run this again", ch.Name)
		}
		if !w.yesNo(ch.FixMsg + "?") {
			return fmt.Errorf("%s is required", ch.Name)
		}
		if err := ch.Fix(ctx); err != nil {
			return fmt.Errorf("%s: %w", ch.Name, err)
		}
		fmt.Fprintf(w.out, "✅ %s: done\n", ch.Name)
	}
	return nil
}

// save writes the config (keeping a backup), validates it, prepares a
// Windows host and restarts the service.
func (w *hostWizard) save(ctx context.Context, h hostsetup.Host) error {
	src, err := os.ReadFile(w.path)
	if err != nil {
		return err
	}
	next, err := hostsetup.AddHost(string(src), h)
	if err != nil {
		return err
	}
	step(w.out, "Config")
	fmt.Fprintf(w.out, "%s\n", strings.TrimSpace(h.Block()))
	if !w.yesNo("\nAdd this to " + w.path + "?") {
		return errors.New("nothing changed")
	}
	backup := w.path + ".bak-" + time.Now().UTC().Format("20060102T150405")
	if err := os.WriteFile(backup, src, 0o600); err != nil { //nolint:gosec // beside the config path the CLI resolved
		return err
	}
	if err := os.WriteFile(w.path, []byte(next), 0o640); err != nil { //nolint:gosec // same mode setup writes
		return err
	}
	cfg, err := config.Load(w.path)
	if err != nil {
		_ = os.WriteFile(w.path, src, 0o640) //nolint:gosec // restoring the original
		return fmt.Errorf("the new config does not validate, left unchanged: %w", err)
	}
	fmt.Fprintf(w.out, "✅ saved (previous config: %s)\n", backup)
	if h.Kind == hostsetup.KindWindows {
		fmt.Fprintln(w.out, "Installing the baseline and runner files on the PC (a few minutes)…")
		if err := w.setupWindows(ctx, cfg, h.Name); err != nil {
			return fmt.Errorf("windows host setup: %w (run `runner-warden windows host-setup %s` to retry)", err, h.Name)
		}
	}
	if w.f.noRestart {
		fmt.Fprintf(w.out, "Restart runner-warden@%s to start using %s.\n", w.inst, h.Name)
		return nil
	}
	if err := w.restart(ctx); err != nil {
		return err
	}
	fmt.Fprintf(w.out, "Restarted; waiting for %s to come up…\n", h.Name)
	if err := w.waitUp(ctx, h.Name, 5*time.Minute); err != nil {
		return fmt.Errorf("%w; see `runner-warden logs`", err)
	}
	fmt.Fprintf(w.out, "✅ %s is linked. `runner-warden status` shows it under MACHINE.\n", h.Name)
	return nil
}

// waitHostUp polls the daemon until the host is reachable.
func waitHostUp(ctx context.Context, name string, limit time.Duration) error {
	cl, err := controlClient()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		var rep daemon.StatusReport
		if err := cl.Status(ctx, &rep); err == nil {
			for _, m := range []map[string]bool{rep.LinuxHosts, rep.MacHosts, rep.WindowsHosts} {
				if m[name] {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return fmt.Errorf("%s did not come up within %s", name, limit)
}

func hostListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List linked machines and whether they are reachable",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			var rep daemon.StatusReport
			if cl, err := controlClient(); err == nil {
				_ = cl.Status(cmd.Context(), &rep)
			}
			state := func(m map[string]bool, n string) string {
				if m == nil {
					return "?"
				}
				if m[n] {
					return "reachable"
				}
				return "UNREACHABLE"
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "NAME\tKIND\tADDRESS\tSTATE\tSERVES")
			for _, h := range cfg.Linux.Hosts {
				kind := "linux"
				if strings.HasPrefix(h.Exec, "wsl ") {
					kind = "wsl"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, kind, h.Address, state(rep.LinuxHosts, h.Name), strings.Join(h.Targets, ","))
			}
			for _, h := range cfg.Windows.Hosts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, "windows", h.Address, state(rep.WindowsHosts, h.Name), strings.Join(h.Targets, ","))
			}
			for _, h := range cfg.MacOS.Hosts {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", h.Name, "macos", h.Address, state(rep.MacHosts, h.Name), strings.Join(h.Targets, ","))
			}
			return tw.Flush()
		},
	}
}
