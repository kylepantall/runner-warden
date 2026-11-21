package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/kylepantall/runner-warden/internal/backend/winvm"
	"github.com/kylepantall/runner-warden/internal/config"
	"github.com/kylepantall/runner-warden/internal/daemon"
	"github.com/kylepantall/runner-warden/internal/naming"
)

// winPool adds VMsUp (the scheduler's VM probe) to *winvm.Pool.
type winPool struct {
	*winvm.Pool
	names []string
}

func (w winPool) VMsUp() map[string]int64 {
	up := map[string]int64{}
	for _, n := range w.names {
		if w.Up(n) {
			up[n] = w.MemFor(n)
		}
	}
	return up
}

func newWindowsPool(cfg *config.Config, hostID string, log *slog.Logger) (winPool, error) {
	p, err := winvm.New(winvm.Config{
		Instance:     cfg.Instance,
		HostID:       hostID,
		StateDir:     cfg.StateDir,
		Slice:        naming.Slice(cfg.Instance),
		DockerSocket: cfg.Linux.DockerSocket,
		Windows:      cfg.Windows,
		TaskLogon:    cfg.Windows.TaskLogon,
		Log:          log.With("backend", "windows"),
	})
	if err != nil {
		return winPool{}, err
	}
	w := winPool{Pool: p}
	for _, v := range cfg.Windows.VMs {
		w.names = append(w.names, v.Name)
	}
	return w, nil
}

// hostIDForCLI returns the recorded host id (CLI commands run beside the daemon).
func hostIDForCLI(cfg *config.Config) (string, error) {
	b, err := os.ReadFile(daemon.HostIDFile(cfg.StateDir))
	if err == nil && len(b) > 0 {
		return string(b), nil
	}
	return "", errors.New("host id not recorded yet; start the service once (`runner-warden setup`)")
}

func windowsCmd() *cobra.Command {
	c := &cobra.Command{Use: "windows", Short: "Manage Windows VMs and hosts"}
	var userScript string
	provision := &cobra.Command{
		Use:   "provision <vm>",
		Short: "Install a fresh Windows VM (20-40 min) and prepare it for runners",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hostID, err := hostIDForCLI(cfg)
			if err != nil {
				return err
			}
			p, err := newWindowsPool(cfg, hostID, newLogger("info"))
			if err != nil {
				return err
			}
			script := ""
			if userScript != "" {
				b, err := os.ReadFile(userScript) //nolint:gosec // script path supplied by the operator on the CLI
				if err != nil {
					return err
				}
				script = string(b)
			}
			return p.Provision(cmd.Context(), args[0], script)
		},
	}
	provision.Flags().StringVar(&userScript, "script", "", "extra PowerShell provisioning script (toolchains)")

	var from, passwordFile string
	var stopForeign bool
	adopt := &cobra.Command{
		Use:   "adopt <vm> --from <container> --password-file <file>",
		Short: "Take over an existing dockurr/windows VM disk in place",
		Long: "Recreates the VM as a runner-warden container reusing the existing storage, installs our SSH key " +
			"using the existing guest password once, and stops+disables old runner services in the guest. " +
			"The old container is stopped (with --stop-foreign) but never removed.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			hostID, err := hostIDForCLI(cfg)
			if err != nil {
				return err
			}
			p, err := newWindowsPool(cfg, hostID, newLogger("info"))
			if err != nil {
				return err
			}
			f, err := p.InspectForeign(cmd.Context(), from)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Foreign VM %s: storage=%s mem=%d cpus=%d running=%v\n", f.Name, f.StorageDir, f.Mem, f.CPUs, f.Running)
			res, err := p.Adopt(cmd.Context(), args[0], from, passwordFile, stopForeign)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Adopted. Old runner services disabled in the guest: %v\n", res.RunnerServices)
			fmt.Fprintf(cmd.OutOrStdout(), "Next: deregister those runners (runner-warden migrate retire) and remove container %s once you're happy.\n", from)
			return nil
		},
	}
	adopt.Flags().StringVar(&from, "from", "", "existing dockurr container name")
	adopt.Flags().StringVar(&passwordFile, "password-file", "", "file holding the existing guest user's password")
	adopt.Flags().BoolVar(&stopForeign, "stop-foreign", false, "stop the old container if it is running")
	_ = adopt.MarkFlagRequired("from")
	_ = adopt.MarkFlagRequired("password-file")

	hostScript := &cobra.Command{
		Use:   "host-script <host>",
		Short: "Print the PowerShell that prepares a [[windows.host]] for SSH",
		Long: "Prints a script to run once, elevated, on the Windows host: it enables OpenSSH Server " +
			"and trusts runner-warden's key for administrators. Then run `runner-warden windows host-setup <host>`.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !isWindowsHost(cfg, args[0]) {
				return fmt.Errorf("no [[windows.host]] named %q", args[0])
			}
			_, pub, err := winvm.EnsureKey(cfg.Windows.SSHKeyFile)
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), winvm.HostScript(pub))
			return nil
		},
	}
	var hostUserScript string
	hostSetup := &cobra.Command{
		Use:   "host-setup <host>",
		Short: "Install the baseline and runner files on a [[windows.host]] over SSH",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !isWindowsHost(cfg, args[0]) {
				return fmt.Errorf("no [[windows.host]] named %q", args[0])
			}
			hostID, err := hostIDForCLI(cfg)
			if err != nil {
				return err
			}
			p, err := newWindowsPool(cfg, hostID, newLogger("info"))
			if err != nil {
				return err
			}
			script := ""
			if hostUserScript != "" {
				b, err := os.ReadFile(hostUserScript) //nolint:gosec // script path supplied by the operator on the CLI
				if err != nil {
					return err
				}
				script = string(b)
			}
			if err := p.SetupHost(cmd.Context(), args[0], script); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Host %s is ready. Restart the service to start using it.\n", args[0])
			return nil
		},
	}
	hostSetup.Flags().StringVar(&hostUserScript, "script", "", "extra PowerShell setup script (toolchains)")

	c.AddCommand(provision, adopt, hostScript, hostSetup)
	return c
}

func isWindowsHost(cfg *config.Config, name string) bool {
	for _, h := range cfg.Windows.Hosts {
		if h.Name == name {
			return true
		}
	}
	return false
}
