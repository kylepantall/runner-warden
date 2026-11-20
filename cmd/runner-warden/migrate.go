package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
	gh "github.com/kylepantall/runner-warden/internal/github"
	"github.com/kylepantall/runner-warden/internal/migrate"
)

func migrateCmd() *cobra.Command {
	c := &cobra.Command{Use: "migrate", Short: "Label sweep and retirement of old runners (setup runs these for you)"}

	var mode string
	var updateVars bool
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Rewrite runs-on in workflows from the saved setup plan (re-reads files first)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, plan, rest, err := migrateContext()
			if err != nil {
				return err
			}
			_ = cfg
			res, err := migrate.Apply(cmd.Context(), plan, migrate.ApplyOptions{Mode: migrate.Mode(mode), UpdateVars: updateVars, GH: rest, Log: newLogger("warn")})
			if err != nil {
				return err
			}
			return printJSON(cmd, res)
		},
	}
	apply.Flags().StringVar(&mode, "mode", "pr", "pr | push | print")
	apply.Flags().BoolVar(&updateVars, "update-vars", false, "also update Actions variables")

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
			r := &migrate.Retirer{StateDir: cfg.StateDir, Run: migrate.ExecRunner, API: rest, Log: newLogger("info")}
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
				if x.NeedsRegistration {
					fmt.Fprintf(cmd.OutOrStdout(), "cd %s && sudo -u $(stat -c %%U %s) ./config.sh %s && sudo systemctl start %s\n", x.Dir, x.Dir, joinArgs(x.Args), x.Name)
				}
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

func joinArgs(a []string) string {
	out := ""
	for i, s := range a {
		if i > 0 {
			out += " "
		}
		out += fmt.Sprintf("%q", s)
	}
	return out
}
