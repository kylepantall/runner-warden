package winvm

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"time"
)

// runnerRec is the minimal persisted state of one runner, so a restarted
// daemon can adopt or clean it.
type runnerRec struct {
	Name      string    `json:"name"`
	VM        string    `json:"vm"`
	StartedAt time.Time `json:"started_at"`
	CPUBase   float64   `json:"cpu_base,omitempty"` // container counters at job start
	IOBase    int64     `json:"io_base,omitempty"`
	Restarted bool      `json:"restarted,omitempty"` // VM was restarted under it
	Slot      int       `json:"slot,omitempty"`      // runner install on a multi-job host; 0/1 = windows.runner_dir
}

type stateFile struct {
	Runners []*runnerRec `json:"runners"`
}

var runnerNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (p *Pool) vmStateDir(vm string) string {
	return filepath.Join(p.cfg.StateDir, "windows", vm)
}
func (p *Pool) stateFile(vm string) string    { return filepath.Join(p.vmStateDir(vm), "runners.json") }
func (p *Pool) knownHosts(vm string) string   { return filepath.Join(p.vmStateDir(vm), "known_hosts") }
func (p *Pool) passwordFile(vm string) string { return filepath.Join(p.vmStateDir(vm), "password") }

func (p *Pool) loadState(vm string) ([]*runnerRec, error) {
	b, err := os.ReadFile(p.stateFile(vm))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var sf stateFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, err
	}
	for _, r := range sf.Runners {
		r.VM = vm
	}
	return sf.Runners, nil
}

// saveStateLocked writes the VM's runners atomically. Caller holds p.mu.
func (p *Pool) saveStateLocked(vm string) error {
	sf := stateFile{Runners: []*runnerRec{}}
	for _, r := range p.runners {
		if r.VM == vm {
			sf.Runners = append(sf.Runners, r)
		}
	}
	b, err := json.MarshalIndent(sf, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(p.vmStateDir(vm), 0o700); err != nil {
		return err
	}
	tmp := p.stateFile(vm) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.stateFile(vm))
}
