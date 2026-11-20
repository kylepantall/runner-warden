// Package naming is the only place that derives names from the prefix.
package naming

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/kylepantall/runner-warden/internal/core"
)

const (
	// MaxGitHubName is GitHub's limit for runner and scale-set names (M0).
	MaxGitHubName = 64
	MaxPrefix     = 32
	hostIDLen     = 8
	randLen       = 8
)

var prefixRE = regexp.MustCompile(`^[a-z][a-z0-9-]*[a-z0-9]$`)

var reserved = []string{"self-hosted", "linux", "windows", "macos", "x64", "arm64", "arm"}
var reservedPrefixes = []string{"ubuntu-", "windows-", "macos-"}

// ValidatePrefix checks a user-chosen prefix.
func ValidatePrefix(p string) error {
	switch {
	case len(p) < 2 || len(p) > MaxPrefix:
		return fmt.Errorf("prefix must be 2-%d characters, got %d", MaxPrefix, len(p))
	case !prefixRE.MatchString(p):
		return fmt.Errorf("prefix must be lowercase letters, digits and single hyphens, starting with a letter")
	case strings.Contains(p, "--"):
		return fmt.Errorf("prefix must not contain consecutive hyphens")
	}
	for _, r := range reserved {
		if p == r {
			return fmt.Errorf("prefix %q is reserved by GitHub", p)
		}
	}
	for _, r := range reservedPrefixes {
		if strings.HasPrefix(p, r) {
			return fmt.Errorf("prefix must not start with %q (GitHub-hosted label)", r)
		}
	}
	for _, os := range []core.OS{core.Linux, core.Windows, core.MacOS} {
		if l := Label(p, os); len(l) > MaxGitHubName {
			return fmt.Errorf("label %q exceeds %d characters", l, MaxGitHubName)
		}
	}
	return nil
}

// Label is the runs-on label (and scale-set name) for an OS.
func Label(prefix string, os core.OS) string { return prefix + "-" + string(os) }

// LaneName is the scale-set name of lane n (1-based) for an OS. Every lane
// carries Label(prefix, os) as an extra label, so workflows only use that.
func LaneName(prefix string, os core.OS, n int) string {
	return fmt.Sprintf("%s-l%d", Label(prefix, os), n)
}

// RunnerName is <prefix>-<hostid>-<rand>, always within GitHub's limit.
func RunnerName(prefix, hostID string) string {
	b := make([]byte, randLen/2)
	_, _ = rand.Read(b)
	return prefix + "-" + hostID + "-" + hex.EncodeToString(b)
}

// ValidRunnerName reports whether name is exactly what RunnerName(prefix,
// hostID) can produce: <prefix>-<hostid>-<8 lowercase hex>. Names that came
// from a container label, the store or a backend listing must pass this before
// they reach a filesystem path or an API call.
func ValidRunnerName(prefix, hostID, name string) bool {
	head := prefix + "-" + hostID + "-"
	if prefix == "" || hostID == "" || len(name) != len(head)+randLen || !strings.HasPrefix(name, head) {
		return false
	}
	for _, c := range name[len(head):] {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// NewHostID returns a random host id for a fresh install.
func NewHostID() string {
	b := make([]byte, hostIDLen/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Slice is the systemd slice holding every job container of an instance.
func Slice(instance string) string { return "runner-warden-" + instance + ".slice" }

// MetaPrefix is the label/metadata namespace on every resource we create.
const MetaPrefix = "io.github.kylepantall.runner-warden"

// Metadata keys.
const (
	MetaManaged  = MetaPrefix + ".managed"
	MetaInstance = MetaPrefix + ".instance"
	MetaHostID   = MetaPrefix + ".host-id"
	MetaRunner   = MetaPrefix + ".runner"
	MetaRole     = MetaPrefix + ".role" // runner | job
)
