package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/core"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/migrate"
)

func migrateCmd() *cobra.Command {
	c := &cobra.Command{Use: "migrate", Short: "Label sweep and retirement of old runners (setup runs these for you)"}

	var mode string
	var updateVars bool
	var branch string
	var onlyRepos []string
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Rewrite runs-on in workflows from the saved setup plan (re-reads files first)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, plan, rest, err := migrateContext()
			if err != nil {
				return err
			}
			_ = cfg
			if len(onlyRepos) > 0 {
				keep := plan.Repos[:0]
				for _, r := range plan.Repos {
					for _, want := range onlyRepos {
						if strings.EqualFold(want, r.Owner+"/"+r.Repo) {
							keep = append(keep, r)
						}
					}
				}
				plan.Repos = keep
			}
			res, err := migrate.Apply(cmd.Context(), plan, migrate.ApplyOptions{Mode: migrate.Mode(mode), UpdateVars: updateVars, Branch: branch, GH: rest, Log: newLogger("warn")})
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	apply.Flags().StringVar(&mode, "mode", "pr", "pr | push | print")
	apply.Flags().BoolVar(&updateVars, "update-vars", false, "also update Actions variables")
	apply.Flags().StringSliceVar(&onlyRepos, "repo", nil, "only these owner/name repos (repeatable)")
	apply.Flags().StringVar(&branch, "branch", "", "PR branch name (default ci/runner-warden-labels-<timestamp>; match the repo's branch naming rules)")

	retire := &cobra.Command{
		Use:   "retire",
		Short: "Retire old local runners once idle (stop, disable, archive, deregister); resumable",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, plan, rest, err := migrateContext()
			if err != nil {
				return err
			}
			if len(plan.Local) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no local runners in the plan")
				return nil
			}
			r := &migrate.Retirer{StateDir: cfg.StateDir, Run: migrate.ExecRunner, API: rest, Log: newLogger("info"), Allowed: cfgTargets(cfg)}
			for _, lr := range plan.Local {
				fmt.Fprintf(cmd.ErrOrStderr(), "candidate %s %s eligible=%t\n", lr.Name, lr.Dir, lr.Eligible)
			}
			m, err := r.Retire(cmd.Context(), plan.Local)
			if err != nil {
				return err
			}
			return printJSON(cmd, m)
		},
	}

	rollback := &cobra.Command{
		Use:   "rollback <manifest.json>",
		Short: "Restore archived runners and print the commands that re-register them",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, _, rest, err := migrateContext()
			if err != nil {
				return err
			}
			r := &migrate.Retirer{StateDir: cfg.StateDir, Run: migrate.ExecRunner, API: rest, Log: newLogger("info")}
			rr, err := r.Rollback(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, x := range rr {
				if !x.NeedsRegistration {
					continue
				}
				line, err := rollbackLine(x)
				if err != nil {
					return fmt.Errorf("runner %q: %w", x.Name, err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}

	scan := &cobra.Command{
		Use:   "plan",
		Short: "Print the saved setup plan (JSON)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, plan, _, err := migrateContext()
			if err != nil {
				return err
			}
			return printJSON(cmd, plan)
		},
	}
	c.AddCommand(scan, apply, retire, rollback)
	return c
}

func migrateContext() (cfgT *config.Config, plan *migrate.Plan, rest *gh.REST, err error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	b, err := os.ReadFile(filepath.Join(cfg.StateDir, "setup-plan.json"))
	if err != nil {
		return nil, nil, nil, errors.New("no saved plan; run `runner-warden setup` first")
	}
	plan = &migrate.Plan{}
	if err := json.Unmarshal(b, plan); err != nil {
		return nil, nil, nil, err
	}
	token, err := gh.Token(cfg.GitHub.TokenFile)
	if err != nil {
		return nil, nil, nil, err
	}
	rest, err = gh.NewREST(token)
	return cfg, plan, rest, err
}

func printJSON(cmd *cobra.Command, v any) error {
	e := json.NewEncoder(cmd.OutOrStdout())
	e.SetIndent("", "  ")
	return e.Encode(v)
}

func cfgTargets(cfg *config.Config) []core.Target {
	var out []core.Target
	for _, t := range cfg.Targets {
		out = append(out, t.Core())
	}
	return out
}

var errUnsafeRunner = errors.New("runner has characters that are unsafe in a shell command")

// shQuote POSIX single-quotes s.
func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// rollbackLine builds the shell line that re-registers a restored runner.
// Every field is single-quoted; names and labels must match a strict charset
// and the unit is the recorded one, not derived from the runner name.
func rollbackLine(x migrate.ReRegister) (string, error) {
	if !migrate.ValidIdent(x.Name) || !migrate.ValidDir(x.Dir) || (x.Unit != "" && !migrate.ValidUnit(x.Unit)) {
		return "", errUnsafeRunner
	}
	for _, l := range x.Labels {
		if !migrate.ValidIdent(l) {
			return "", errUnsafeRunner
		}
	}
	q := make([]string, len(x.Args))
	for i, a := range x.Args {
		q[i] = shQuote(a)
	}
	line := fmt.Sprintf("cd %s && sudo -u \"$(stat -c %%U %s)\" %s", shQuote(x.Dir), shQuote(x.Dir), strings.Join(q, " "))
	if x.Unit != "" {
		line += " && sudo systemctl start " + shQuote(x.Unit)
	}
	return line, nil
}
