package winvm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
)

// packageTimeout bounds one member's installs (Visual Studio takes a while).
const packageTimeout = 90 * time.Minute

// packagesKey changes whenever windows.package does.
func packagesKey(ps []config.WindowsPackage) string {
	h := sha256.New()
	for _, p := range ps {
		fmt.Fprintf(h, "%s\x00%s\x00", p.ID, p.Override)
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// packagesScript installs, through winget, each package winget does not
// list; one line per package: "present <id>", "installed <id>" or
// "failed <id> <code>".
func packagesScript(ps []config.WindowsPackage) string {
	var b strings.Builder
	b.WriteString("$ProgressPreference='SilentlyContinue'\n$pkgs = @(\n")
	for _, p := range ps {
		fmt.Fprintf(&b, "  @{ id = '%s'; ov = '%s' }\n", p.ID, p.Override)
	}
	b.WriteString(`)
foreach ($p in $pkgs) {
  winget list --id $p.id -e --accept-source-agreements --disable-interactivity *> $null
  if ($LASTEXITCODE -eq 0) { "present $($p.id)"; continue }
  $a = @('install', '--id', $p.id, '-e', '--silent', '--accept-package-agreements', '--accept-source-agreements', '--disable-interactivity')
  if ($p.ov) { $a += @('--override', $p.ov) }
  & winget @a *> $null
  # 0x8A15002B: already installed (winget did not list it).
  if ($LASTEXITCODE -eq 0 -or $LASTEXITCODE -eq -1978335189) { "installed $($p.id)" } else { "failed $($p.id) $LASTEXITCODE" }
}
`)
	return b.String()
}

// PackagesPending lists up VMs and reachable hosts whose windows.package set
// has not been ensured since it last changed.
func (p *Pool) PackagesPending() []string {
	ps := p.cfg.Windows.Packages
	if len(ps) == 0 {
		return nil
	}
	key := packagesKey(ps)
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, name := range p.order {
		if p.up[name] && p.pkgDone[name] != key {
			out = append(out, name)
		}
	}
	return out
}

// NeedsPackages reports whether vm's windows.package set is not ensured yet.
func (p *Pool) NeedsPackages(vm string) bool {
	ps := p.cfg.Windows.Packages
	if len(ps) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.pkgDone[vm] != packagesKey(ps)
}

// EnsurePackages installs the windows.package entries vm lacks. It returns
// what it installed; a failed install is an error and is retried next time.
// Calls are serialised, so a start waiting for packages and the background
// loop never install the same thing twice at once.
func (p *Pool) EnsurePackages(ctx context.Context, vm string) ([]string, error) {
	p.pkgMu.Lock()
	defer p.pkgMu.Unlock()
	if !p.NeedsPackages(vm) {
		return nil, nil
	}
	v, err := p.vm(vm)
	if err != nil {
		return nil, err
	}
	ps := p.cfg.Windows.Packages
	out, err := p.runScript(ctx, v, packagesScript(ps), "", packageTimeout)
	if err != nil {
		return nil, err
	}
	var installed, failed []string
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) >= 2 && f[0] == "installed":
			installed = append(installed, f[1])
		case len(f) >= 2 && f[0] == "failed":
			failed = append(failed, strings.Join(f[1:], " "))
		}
	}
	if len(failed) > 0 {
		return installed, fmt.Errorf("winget install failed on %s: %s", vm, strings.Join(failed, ", "))
	}
	p.mu.Lock()
	p.pkgDone[vm] = packagesKey(ps)
	p.mu.Unlock()
	return installed, nil
}
