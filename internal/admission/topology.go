package admission

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SysCPURoot is the default sysfs location of CPU topology.
const SysCPURoot = "/sys/devices/system/cpu"

// ParseCPUList parses a kernel cpu list such as "0-1,4,6-7".
func ParseCPUList(s string) ([]int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		lo, hi, isRange := strings.Cut(part, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			return nil, fmt.Errorf("cpu list %q: %w", s, err)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				return nil, fmt.Errorf("cpu list %q: %w", s, err)
			}
		}
		if b < a {
			return nil, fmt.Errorf("cpu list %q: descending range", s)
		}
		for i := a; i <= b; i++ {
			out = append(out, i)
		}
	}
	sort.Ints(out)
	return out, nil
}

// ReadTopology builds the physical-core list from
// <root>/cpu*/topology/thread_siblings_list. Siblings share one Core whose ID
// is the lowest thread id. An empty root means SysCPURoot. CPUs without
// topology information (offline) are skipped.
func ReadTopology(root string) ([]Core, error) {
	if root == "" {
		root = SysCPURoot
	}
	files, err := filepath.Glob(filepath.Join(root, "cpu[0-9]*", "topology", "thread_siblings_list"))
	if err != nil {
		return nil, err
	}
	seen := map[int]bool{}
	var cores []Core
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // sysfs paths from our own glob of the topology root
		if err != nil {
			return nil, err
		}
		threads, err := ParseCPUList(string(b))
		if err != nil {
			return nil, err
		}
		if len(threads) == 0 || seen[threads[0]] {
			continue
		}
		seen[threads[0]] = true
		cores = append(cores, Core{ID: threads[0], Threads: threads})
	}
	if len(cores) == 0 {
		return nil, fmt.Errorf("no cpu topology under %s", root)
	}
	sort.Slice(cores, func(i, j int) bool { return cores[i].ID < cores[j].ID })
	return cores, nil
}
