package admission

import (
	"path"
	"strings"
	"time"

	"github.com/kylepantall/runner-warden/internal/config"
)

// Rule assigns resources to jobs whose "owner/repo/workflow-file/job-name"
// matches. Each of the four segments is matched with path.Match semantics; a
// "/" inside the job name is matched like any other character.
type Rule struct {
	Match       string
	Mem         int64
	PinnedCores int
	Devices     []string
	Image       string
}

// RulesFromConfig converts config [[cost]] entries, keeping their order.
func RulesFromConfig(cs []config.Cost) []Rule {
	out := make([]Rule, len(cs))
	for i, c := range cs {
		out[i] = Rule{Match: c.Match, Mem: int64(c.Mem), PinnedCores: c.PinnedCores, Devices: c.Devices, Image: c.Image}
	}
	return out
}

// WorkflowFile extracts the workflow file basename from a workflow ref such as
// "owner/repo/.github/workflows/ci.yml@refs/heads/main".
func WorkflowFile(ref string) string {
	if i := strings.IndexByte(ref, '@'); i >= 0 {
		ref = ref[:i]
	}
	return path.Base(ref)
}

// JobKey is the string rules match against.
func JobKey(owner, repo, workflowRef, job string) string {
	return strings.ToLower(owner) + "/" + strings.ToLower(repo) + "/" + WorkflowFile(workflowRef) + "/" + job
}

// MatchRule finds the first rule matching the job.
func MatchRule(rules []Rule, owner, repo, workflowRef, job string) (Rule, bool) {
	segs := [4]string{strings.ToLower(owner), strings.ToLower(repo), WorkflowFile(workflowRef), job}
	for _, r := range rules {
		if matchSegments(r.Match, segs) {
			return r, true
		}
	}
	return Rule{}, false
}

func matchSegments(pattern string, segs [4]string) bool {
	p := strings.SplitN(pattern, "/", 4)
	if len(p) != 4 {
		return false
	}
	for i := range segs {
		pat := p[i]
		if i < 2 {
			pat = strings.ToLower(pat)
		}
		// '*' must be allowed to cross '/' inside a segment (job names).
		val := strings.ReplaceAll(segs[i], "/", "\x00")
		ok, err := path.Match(pat, val)
		if err != nil || !ok {
			return false
		}
	}
	return true
}

// Waiter is a job waiting for a grant.
type Waiter struct {
	ID    string
	Group string // fairness group (target)
	Cost  int64
	Since time.Time // age: first time the job was seen
	Seq   int       // arrival order tiebreak
}

// Oldest returns the index of the oldest waiter, or -1 for an empty slice.
func Oldest(ws []Waiter) int {
	best := -1
	for i, w := range ws {
		if best < 0 || older(w, ws[best]) { //nolint:gosec // best >= 0 is checked by the short-circuit above
			best = i
		}
	}
	return best
}

func older(a, b Waiter) bool {
	if !a.Since.Equal(b.Since) {
		return a.Since.Before(b.Since)
	}
	return a.Seq < b.Seq
}

// Policy is the anti-starvation rule: once the oldest waiter is older than
// ReserveAfter, only light jobs (Cost <= LightThreshold) may overtake it.
type Policy struct {
	ReserveAfter   time.Duration
	LightThreshold int64
}

// Blocks reports whether cand must wait so that oldest can be admitted.
func (p Policy) Blocks(oldest, cand Waiter, now time.Time) bool {
	if oldest.ID == cand.ID {
		return false
	}
	return now.Sub(oldest.Since) > p.ReserveAfter && cand.Cost > p.LightThreshold
}

// SortFair orders waiters for granting: lowest granted/weight first, then
// oldest first. granted is memory per group; weight(group) <= 0 counts as 1.
func SortFair(ws []Waiter, granted map[string]int64, weight func(group string) int) {
	w := func(g string) int64 {
		if weight == nil {
			return 1
		}
		if n := weight(g); n > 0 {
			return int64(n)
		}
		return 1
	}
	// compare ga/wa < gb/wb without floating point: ga*wb < gb*wa
	sortStable(ws, func(a, b Waiter) bool {
		l, r := granted[a.Group]*w(b.Group), granted[b.Group]*w(a.Group)
		if l != r {
			return l < r
		}
		return older(a, b)
	})
}

func sortStable(ws []Waiter, less func(a, b Waiter) bool) {
	// insertion sort: n is tiny and this keeps the order deterministic.
	for i := 1; i < len(ws); i++ {
		for j := i; j > 0 && less(ws[j], ws[j-1]); j-- {
			ws[j], ws[j-1] = ws[j-1], ws[j]
		}
	}
}
