package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/assets"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	"github.com/kylepantall/runner-warden/internal/daemon"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/migrate"
	"github.com/kylepantall/runner-warden/internal/naming"
	"github.com/kylepantall/runner-warden/internal/platform/linux"
)

type setupOpts struct {
	prefix, budget, sweep, runnerGroup string
	orgs, repos, legacy, windowsLabels []string
	tokenStdin, yes, dryRun, retire    bool
	updateVars, allowPublic, noWindows bool
}

func setupCmd() *cobra.Command {
	var o setupOpts
	c := &cobra.Command{
		Use:     "setup",
		Aliases: []string{"install"},
		Short:   "Install or update an instance in one guided run",
		Long: `setup is idempotent and resumable:

  1. preflight checks
  2. prefix (required, no default) and GitHub token (from gh, with a scope check)
  3. orgs / repos to serve
  4. read-only scan: registered runners, local runner services, every runs-on
  5. one plan screen and a single confirmation
  6. job slice + systemd service, scale sets created by the service
  7. label sweep (PR per repo by default) and, with --retire, retirement of
     old local runners once idle`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSetup(cmd, o) },
	}
	f := c.Flags()
	f.StringVar(&o.prefix, "prefix", "", "naming prefix, e.g. my-ci (required on first run)")
	f.StringSliceVar(&o.orgs, "org", nil, "org to serve at org level (repeatable)")
	f.StringSliceVar(&o.repos, "repo", nil, "owner/name repo to serve (repeatable)")
	f.StringVar(&o.runnerGroup, "runner-group", "", "org runner group (default: Default)")
	f.StringVar(&o.budget, "memory-budget", "", "memory for all jobs, e.g. 22GiB (default: detected)")
	f.BoolVar(&o.tokenStdin, "token-stdin", false, "read the GitHub token from stdin instead of `gh auth token`")
	f.StringVar(&o.sweep, "sweep", "pr", "workflow label rewrite: pr | push | print | none")
	f.BoolVar(&o.updateVars, "update-vars", false, "also update Actions variables holding old labels")
	f.BoolVar(&o.retire, "retire", false, "retire old local runners (stop, disable, archive, deregister) once idle")
	f.StringSliceVar(&o.legacy, "legacy-label", nil, "extra label that marks a runs-on as self-hosted (repeatable)")
	f.StringSliceVar(&o.windowsLabels, "windows-label", nil, "legacy label that means Windows (repeatable)")
	f.BoolVar(&o.allowPublic, "allow-public", false, "allow public repos (untrusted code on this host)")
	f.BoolVarP(&o.yes, "yes", "y", false, "do not ask for confirmation")
	f.BoolVar(&o.dryRun, "dry-run", false, "stop after showing the plan")
	return c
}

func runSetup(cmd *cobra.Command, o setupOpts) error {
	ctx := cmd.Context()
	out := cmd.OutOrStdout()
	in := bufio.NewReader(cmd.InOrStdin())
	interactive := isTTY(os.Stdin) && !o.yes

	step(out, "Preflight")
	pre := doctor(ctx, nil, false)
	printChecks(out, pre)
	if failed(pre) && !o.dryRun {
		return errors.New("preflight failed")
	}

	// Existing instance → resume with its config.
	var cfg *config.Config
	inst := instanceFlag
	if inst == "" && o.prefix != "" {
		inst = o.prefix
	}
	if inst == "" {
		if i, err := resolveInstance(); err == nil {
			inst = i
		}
	}
	if inst != "" {
		if c, err := config.Load(config.Path(inst)); err == nil {
			cfg = c
			fmt.Fprintf(out, "Resuming instance %s (%s)\n", inst, config.Path(inst))
		}
	}
	if cfg == nil {
		cfg = &config.Config{Linux: config.Linux{Enabled: true}}
		step(out, "Prefix")
		p := o.prefix
		for p == "" && interactive {
			p = prompt(out, in, "Prefix for labels and runner names (e.g. my-ci)")
			if err := naming.ValidatePrefix(p); err != nil {
				fmt.Fprintln(out, "  ", err)
				p = ""
			}
		}
		if err := naming.ValidatePrefix(p); err != nil {
			return fmt.Errorf("--prefix: %w", err)
		}
		cfg.Prefix = p
	}
	if o.prefix != "" && o.prefix != cfg.Prefix {
		return fmt.Errorf("instance already uses prefix %q; use `runner-warden prefix set` to change it", cfg.Prefix)
	}
	fmt.Fprintf(out, "Labels: %s, %s   lanes: %s .. %s\n",
		naming.Label(cfg.Prefix, core.Linux), naming.Label(cfg.Prefix, core.Windows),
		naming.LaneName(cfg.Prefix, core.Linux, 1), naming.LaneName(cfg.Prefix, core.Linux, max(cfg.Limits.Lanes, 4)))

	// Targets.
	for _, org := range o.orgs {
		addTarget(cfg, config.Target{Org: org, RunnerGroup: o.runnerGroup})
	}
	for _, r := range o.repos {
		addTarget(cfg, config.Target{Repo: r})
	}
	if len(cfg.Targets) == 0 && interactive {
		for _, s := range splitList(prompt(out, in, "Orgs to serve (comma separated, blank for none)")) {
			addTarget(cfg, config.Target{Org: s, RunnerGroup: o.runnerGroup})
		}
		for _, s := range splitList(prompt(out, in, "Repos to serve as owner/name (comma separated, blank for none)")) {
			addTarget(cfg, config.Target{Repo: s})
		}
	}
	if len(cfg.Targets) == 0 {
		return errors.New("no targets: pass --org and/or --repo")
	}

	// Memory budget.
	if o.budget != "" {
		v, err := units.RAMInBytes(o.budget)
		if err != nil {
			return fmt.Errorf("--memory-budget: %w", err)
		}
		cfg.Limits.MemoryBudget = config.Size(v)
	}
	if cfg.Limits.MemoryBudget == 0 {
		cfg.Limits.MemoryBudget = config.Size(defaultBudget())
	}
	cfg.ApplyDefaults()
	if err := cfg.Validate(); err != nil {
		return err
	}

	// Token.
	step(out, "GitHub access")
	token, err := setupToken(ctx, cfg, o, in)
	if err != nil {
		return err
	}
	rest, err := gh.NewREST(token)
	if err != nil {
		return err
	}
	needs := []string{"repo"}
	for _, t := range cfg.Targets {
		if t.Org != "" {
			needs = append(needs, "admin:org")
			break
		}
	}
	if o.sweep == "pr" || o.sweep == "push" {
		needs = append(needs, "workflow")
	}
	scopes, err := rest.TokenScopes(ctx)
	if err != nil {
		return fmt.Errorf("token check: %w", err)
	}
	if missing := missingScopes(scopes, needs); len(missing) > 0 {
		fmt.Fprintf(out, "❌ token is missing scopes: %s\n   run as your user: gh auth refresh -h github.com -s %s\n   then re-run setup\n",
			strings.Join(missing, ", "), strings.Join(missing, ","))
		return exitCode(2)
	}
	fmt.Fprintf(out, "✅ token scopes: %s\n", strings.Join(scopes, ", "))

	// Public repo guard.
	for _, t := range cfg.Targets {
		if t.Repo == "" {
			continue
		}
		ct := t.Core()
		r, err := rest.GetRepo(ctx, ct.Owner, ct.Repo)
		if err != nil {
			return fmt.Errorf("repo %s: %w", t.Repo, err)
		}
		if !r.Private && !o.allowPublic {
			return fmt.Errorf("repo %s is public: its fork PRs would run on this host; pass --allow-public only if you route forks to hosted runners", t.Repo)
		}
	}

	// Scan.
	step(out, "Scan (read-only)")
	var targets []core.Target
	for _, t := range cfg.Targets {
		targets = append(targets, t.Core())
	}
	rules := migrate.Rules{Prefix: cfg.Prefix, LegacyLabels: o.legacy, WindowsLabels: o.windowsLabels}
	// Learn legacy labels from the runners registered today. A label is
	// Windows-only when no non-Windows runner carries it too.
	winOnly, other := map[string]bool{}, map[string]bool{}
	for _, t := range targets {
		rs, err := rest.ListRunners(ctx, t)
		if err != nil {
			continue
		}
		for _, r := range rs {
			if isOurRunnerName(r.Name) {
				continue
			}
			for _, l := range r.Labels {
				if isDefaultLabel(l) {
					continue
				}
				if !slices.Contains(rules.LegacyLabels, l) {
					rules.LegacyLabels = append(rules.LegacyLabels, l)
				}
				if strings.EqualFold(r.OS, "windows") {
					winOnly[l] = true
				} else {
					other[l] = true
				}
			}
		}
	}
	for l := range winOnly {
		if !other[l] && !slices.Contains(rules.WindowsLabels, l) {
			rules.WindowsLabels = append(rules.WindowsLabels, l)
		}
	}
	plan, err := migrate.Scan(ctx, migrate.ScanInput{
		Targets: targets, Rules: rules, GH: rest, Log: newLogger("warn"),
		HostRoots: []string{"/home/*", "/opt", "/srv"},
	})
	if err != nil {
		return fmt.Errorf("scan: %w", err)
	}
	printPlan(out, cfg, plan, o)
	if o.dryRun {
		return writeJSON(filepath.Join(os.TempDir(), "runner-warden-plan.json"), plan)
	}
	if interactive && !confirm(out, in, "Apply this plan?") {
		return errors.New("aborted")
	}

	// Apply.
	step(out, "Install")
	if err := os.MkdirAll(cfg.StateDir, 0o750); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(cfg.StateDir, "setup-plan.json"), plan); err != nil {
		return err
	}
	if err := writeToken(cfg.GitHub.TokenFile, token); err != nil {
		return err
	}
	if err := cfg.Save(config.Path(cfg.Instance)); err != nil {
		return err
	}
	fmt.Fprintf(out, "✅ config %s\n", config.Path(cfg.Instance))
	if err := linux.EnsureSlice(naming.Slice(cfg.Instance), int64(cfg.Limits.MemoryBudget), int64(cfg.Limits.SwapMax)); err != nil {
		return fmt.Errorf("slice: %w", err)
	}
	fmt.Fprintf(out, "✅ job slice %s (MemoryMax %s)\n", naming.Slice(cfg.Instance), units.BytesSize(float64(cfg.Limits.MemoryBudget)))
	if err := installService(ctx, cfg.Instance); err != nil {
		return err
	}
	fmt.Fprintf(out, "✅ service runner-warden@%s enabled\n", cfg.Instance)
	if err := waitDaemon(ctx, cfg, 2*time.Minute); err != nil {
		return fmt.Errorf("service did not come up: %w (journalctl -u runner-warden@%s)", err, cfg.Instance)
	}
	fmt.Fprintln(out, "✅ daemon up, scale sets live")

	if o.sweep != "none" {
		step(out, "Label sweep")
		mode := migrate.Mode(o.sweep)
		res, err := migrate.Apply(ctx, plan, migrate.ApplyOptions{Mode: mode, UpdateVars: o.updateVars, GH: rest, Log: newLogger("warn")})
		if err != nil {
			return fmt.Errorf("sweep: %w", err)
		}
		for _, r := range res.Repos {
			switch {
			case r.Error != "":
				fmt.Fprintf(out, "❌ %s/%s: %s\n", r.Owner, r.Repo, r.Error)
			case r.PRURL != "":
				fmt.Fprintf(out, "✅ %s/%s: %s\n", r.Owner, r.Repo, r.PRURL)
			case r.Diff != "":
				fmt.Fprintf(out, "--- %s/%s\n%s\n", r.Owner, r.Repo, r.Diff)
			case r.Changed:
				fmt.Fprintf(out, "✅ %s/%s: pushed %s\n", r.Owner, r.Repo, r.Commit)
			}
		}
		for _, v := range res.Vars {
			fmt.Fprintf(out, "   variable %+v\n", v)
		}
	}
	if o.retire && len(plan.Local) > 0 {
		step(out, "Retire old local runners")
		ret := &migrate.Retirer{StateDir: cfg.StateDir, Run: migrate.ExecRunner, API: rest, Log: newLogger("info")}
		m, err := ret.Retire(ctx, plan.Local)
		if err != nil {
			return fmt.Errorf("retire: %w", err)
		}
		for _, r := range m.Runners {
			fmt.Fprintf(out, "   %+v\n", r)
		}
		fmt.Fprintln(out, "   Busy runners stay pending; re-run `runner-warden migrate retire` later.")
	}
	step(out, "Done")
	fmt.Fprintf(out, "Workflows now use: runs-on: %s (or %s)\nCheck: runner-warden status | runner-warden check | runner-warden logs -f\n",
		naming.Label(cfg.Prefix, core.Linux), naming.Label(cfg.Prefix, core.Windows))
	return nil
}

func addTarget(cfg *config.Config, t config.Target) {
	for _, x := range cfg.Targets {
		if x.Org == t.Org && x.Repo == t.Repo {
			return
		}
	}
	cfg.Targets = append(cfg.Targets, t)
}

func setupToken(ctx context.Context, cfg *config.Config, o setupOpts, in *bufio.Reader) (string, error) {
	if o.tokenStdin {
		b, err := io.ReadAll(in)
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if t, err := gh.Token(cfg.GitHub.TokenFile); err == nil {
		return t, nil
	}
	args := []string{"auth", "token"}
	var c *exec.Cmd
	if u := os.Getenv("SUDO_USER"); u != "" && os.Geteuid() == 0 {
		c = exec.CommandContext(ctx, "sudo", append([]string{"-u", u, "-H", "gh"}, args...)...)
	} else {
		c = exec.CommandContext(ctx, "gh", args...)
	}
	b, err := c.Output()
	if err != nil {
		return "", fmt.Errorf("no token: run `gh auth login` as your user first, or pass --token-stdin (%w)", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func missingScopes(have, need []string) []string {
	if slices.Contains(have, gh.ScopesUnknown) {
		return nil // fine-grained token: permissions are checked on use
	}
	var miss []string
	for _, n := range need {
		if slices.Contains(have, n) {
			continue
		}
		// admin:org implies write:org/read:org; repo covers public_repo.
		if n == "workflow" && slices.Contains(have, "workflow") {
			continue
		}
		miss = append(miss, n)
	}
	return miss
}

func isOurRunnerName(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) < 3 {
		return false
	}
	return len(parts[len(parts)-1]) == 8 && len(parts[len(parts)-2]) == 8
}

var defaultLabels = []string{"self-hosted", "linux", "windows", "macos", "x64", "arm64", "arm", "x86"}

func isDefaultLabel(l string) bool { return slices.Contains(defaultLabels, strings.ToLower(l)) }

func printPlan(w io.Writer, cfg *config.Config, p *migrate.Plan, o setupOpts) {
	lanes := max(cfg.Limits.Lanes, 1)
	fmt.Fprintf(w, "\nPLAN for instance %s (prefix %s)\n", cfg.Instance, cfg.Prefix)
	fmt.Fprintf(w, "  memory budget  %s for all jobs (slice hard cap, never throttled)\n", units.BytesSize(float64(cfg.Limits.MemoryBudget)))
	for _, t := range cfg.Targets {
		for _, os := range t.OSes() {
			fmt.Fprintf(w, "  scale sets     %s: %d lanes labelled %s\n", t.Core(), lanes, naming.Label(cfg.Prefix, os))
		}
	}
	reg := 0
	for _, t := range p.Targets {
		reg += len(t.Runners)
	}
	fmt.Fprintf(w, "  legacy labels  %s\n", strings.Join(p.Rules.LegacyLabels, ", "))
	fmt.Fprintf(w, "  registered     %d existing runners at the targets (left running until retired)\n", reg)
	fmt.Fprintf(w, "  local runners  %d on this host", len(p.Local))
	if o.retire {
		fmt.Fprint(w, " → retired once idle (archived, rollback possible)\n")
	} else {
		fmt.Fprint(w, " → kept (pass --retire)\n")
	}
	changes, skips, repos := 0, 0, 0
	for _, r := range p.Repos {
		if r.HasChanges() {
			repos++
		}
		for _, f := range r.Files {
			changes += len(f.Changes)
			skips += len(f.Skipped)
		}
	}
	fmt.Fprintf(w, "  label sweep    %d runs-on in %d repos → %s (%d left alone, see plan JSON)\n", changes, repos, o.sweep, skips)
	for _, v := range p.Vars {
		fmt.Fprintf(w, "  variable       %s (%s): %q → %q %s\n", v.Name, v.Scope, v.Old, v.New, v.Reason)
	}
	fmt.Fprintln(w)
}

func installService(ctx context.Context, inst string) error {
	unit := "/etc/systemd/system/runner-warden@.service"
	if cur, err := os.ReadFile(unit); err != nil || string(cur) != string(assets.SystemdUnit) {
		if err := os.WriteFile(unit, assets.SystemdUnit, 0o644); err != nil {
			return err
		}
	}
	for _, a := range [][]string{{"daemon-reload"}, {"enable", "runner-warden@" + inst}, {"restart", "runner-warden@" + inst}} {
		if out, err := exec.CommandContext(ctx, "systemctl", a...).CombinedOutput(); err != nil {
			return fmt.Errorf("systemctl %s: %v: %s", strings.Join(a, " "), err, out)
		}
	}
	return nil
}

func waitDaemon(ctx context.Context, cfg *config.Config, timeout time.Duration) error {
	instanceFlag = cfg.Instance
	cl, err := controlClient()
	if err != nil {
		return err
	}
	deadline := time.Now().Add(timeout)
	var last error
	for time.Now().Before(deadline) {
		var rep daemon.StatusReport
		if last = cl.Status(ctx, &rep); last == nil && len(rep.Scheduler.Lanes) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if last == nil {
		last = errors.New("no lanes reported")
	}
	return last
}

// defaultBudget is 75% of RAM minus 2 GiB for the host, at least 2 GiB.
func defaultBudget() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 4 * units.GiB
	}
	var kb int64
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "MemTotal:") {
			fmt.Sscanf(strings.TrimSpace(strings.TrimPrefix(l, "MemTotal:")), "%d", &kb)
		}
	}
	v := kb*1024*3/4 - 2*units.GiB
	return max(v, 2*units.GiB)
}

func writeToken(path, token string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(token+"\n"), 0o600)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o640)
}

func step(w io.Writer, s string) { fmt.Fprintf(w, "\n== %s\n", s) }

func prompt(w io.Writer, in *bufio.Reader, q string) string {
	fmt.Fprintf(w, "%s: ", q)
	s, _ := in.ReadString('\n')
	return strings.TrimSpace(s)
}

func confirm(w io.Writer, in *bufio.Reader, q string) bool {
	a := strings.ToLower(prompt(w, in, q+" [y/N]"))
	return a == "y" || a == "yes"
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func isTTY(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}
