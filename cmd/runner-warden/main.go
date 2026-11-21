// Command runner-warden manages ephemeral self-hosted GitHub Actions runners.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/hookapi"
)

var (
	version = "dev"
	commit  = ""
	date    = ""
)

var instanceFlag string

func main() {
	daemon.Version = version
	root := &cobra.Command{
		Use:           "runner-warden",
		Short:         "Autoscaling manager for self-hosted GitHub Actions runners",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVar(&instanceFlag, "instance", "", "instance name (default: the only configured instance)")
	root.AddCommand(
		versionCmd(), daemonCmd(), hookCmd(), statusCmd(), drainCmd(), killCmd(), logsCmd(), checkCmd(),
		setupCmd(), migrateCmd(), configCmd(), windowsCmd(), uninstallCmd(),
	)
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if err := root.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "runner-warden:", err)
		var ec exitCode
		if errors.As(err, &ec) {
			os.Exit(int(ec))
		}
		os.Exit(1)
	}
}

// exitCode lets commands choose a non-1 exit status.
type exitCode int

func (e exitCode) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		Run: func(cmd *cobra.Command, _ []string) {
			fmt.Fprintf(cmd.OutOrStdout(), "runner-warden %s", version)
			if commit != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " (%s, %s)", commit, date)
			}
			fmt.Fprintln(cmd.OutOrStdout())
		},
	}
}

// resolveInstance returns --instance, or the single configured instance.
func resolveInstance() (string, error) {
	if instanceFlag != "" {
		return instanceFlag, nil
	}
	entries, err := filepath.Glob(filepath.Join(config.EtcDir, "*", "config.toml"))
	if err != nil {
		return "", err
	}
	switch len(entries) {
	case 0:
		return "", errors.New("no instance configured; run `runner-warden setup`")
	case 1:
		return filepath.Base(filepath.Dir(entries[0])), nil
	default:
		var names []string
		for _, e := range entries {
			names = append(names, filepath.Base(filepath.Dir(e)))
		}
		return "", fmt.Errorf("several instances configured (%s); pass --instance", strings.Join(names, ", "))
	}
}

func loadConfig() (*config.Config, error) {
	inst, err := resolveInstance()
	if err != nil {
		return nil, err
	}
	return config.Load(config.Path(inst))
}

func newLogger(level string) *slog.Logger {
	var l slog.Level
	_ = l.UnmarshalText([]byte(level))
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func controlClient() (*hookapi.Client, error) {
	inst, err := resolveInstance()
	if err != nil {
		return nil, err
	}
	return hookapi.NewClient(config.SocketPath(inst)), nil
}

func hookCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "hook job-started|job-completed",
		Short:  "Runner job hook (called inside runner containers)",
		Hidden: true,
		Args:   cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			code := hookapi.RunHook(cmd.Context(), args[0], os.Getenv, os.Stderr, hookapi.HookOptions{
				Log: slog.New(slog.NewTextHandler(os.Stderr, nil)),
			})
			if code != 0 {
				return exitCode(code)
			}
			return nil
		},
	}
}
