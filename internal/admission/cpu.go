package admission

import "runtime"

// CPUBudget resolves the configured shared CPU budget to millicpus: a
// positive value is used as is, 0 means every logical CPU outside reserved,
// and a negative value disables the CPU check (0).
func CPUBudget(cfg float64, cores []Core, reserved []int) int64 {
	switch {
	case cfg > 0:
		return MilliCPU(cfg)
	case cfg < 0:
		return 0
	}
	skip := map[int]bool{}
	for _, c := range reserved {
		skip[c] = true
	}
	n := 0
	for _, c := range cores {
		for _, t := range c.Threads {
			if !skip[t] {
				n++
			}
		}
	}
	if n == 0 {
		n = runtime.NumCPU() - len(reserved)
	}
	return int64(max(n, 1)) * 1000
}
