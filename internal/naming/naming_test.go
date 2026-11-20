package naming

import (
	"strings"
	"testing"

	"github.com/kylepantall/runner-warden/internal/core"
)

func TestValidatePrefix(t *testing.T) {
	good := []string{"my-ci", "ab", "acme-it-platform", strings.Repeat("a", MaxPrefix)}
	bad := []string{"", "a", "My-ci", "-ci", "ci-", "a--b", "linux", "ubuntu-ci", "a_b", strings.Repeat("a", MaxPrefix+1)}
	for _, p := range good {
		if err := ValidatePrefix(p); err != nil {
			t.Errorf("%q: unexpected error %v", p, err)
		}
	}
	for _, p := range bad {
		if err := ValidatePrefix(p); err == nil {
			t.Errorf("%q: expected error", p)
		}
	}
}

func TestDerivedNamesFitGitHubLimit(t *testing.T) {
	p := strings.Repeat("a", MaxPrefix)
	host := NewHostID()
	for i := 0; i < 100; i++ {
		if n := RunnerName(p, host); len(n) > MaxGitHubName {
			t.Fatalf("runner name %d chars", len(n))
		}
	}
	for _, os := range []core.OS{core.Linux, core.Windows, core.MacOS} {
		if l := Label(p, os); len(l) > MaxGitHubName {
			t.Fatalf("label %d chars", len(l))
		}
	}
}
