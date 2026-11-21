package admission

import (
	"errors"
	"testing"
)

func TestLedgerCPU(t *testing.T) {
	l := New(Config{Budget: 100, CPUBudget: 4000})
	if _, err := l.Reserve("a", Request{Mem: 1, CPU: 3000}); err != nil {
		t.Fatal(err)
	}
	if l.CanFit(Request{Mem: 1, CPU: 2000}) {
		t.Fatal("2 cpus fit in 1 free")
	}
	if _, err := l.Reserve("b", Request{Mem: 1, CPU: 2000}); !errors.Is(err, ErrNoCPU) {
		t.Fatalf("want ErrNoCPU, got %v", err)
	}
	if _, err := l.Resize("a", Request{Mem: 1, CPU: 5000}); !errors.Is(err, ErrNoCPU) {
		t.Fatalf("resize: want ErrNoCPU, got %v", err)
	}
	if _, err := l.ForceResize("a", Request{Mem: 1, CPU: 5000}); err != nil || l.FreeCPU() != -1000 {
		t.Fatalf("force resize: %v free=%d", err, l.FreeCPU())
	}
	l.Release("a")
	if l.FreeCPU() != 4000 || l.Usage().CPUReserved != 0 {
		t.Fatalf("release left %d", l.Usage().CPUReserved)
	}
	if l.FitsEver(Request{CPU: 5000}) || !l.FitsEver(Request{CPU: 4000}) {
		t.Fatal("FitsEver ignores the CPU budget")
	}
}

func TestLedgerCPUUncheckedWithoutBudget(t *testing.T) {
	l := New(Config{Budget: 100})
	if _, err := l.Reserve("a", Request{Mem: 1, CPU: 1 << 40}); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedThreadsComeOutOfCPUBudget(t *testing.T) {
	cores := []Core{{ID: 0, Threads: []int{0, 8}}, {ID: 1, Threads: []int{1, 9}}}
	l := New(Config{Budget: 100, Cores: cores, CPUBudget: 4000})
	if _, err := l.Reserve("p", Request{Mem: 1, PinnedCores: 1}); err != nil {
		t.Fatal(err)
	}
	if got := l.FreeCPU(); got != 2000 {
		t.Fatalf("free=%d after pinning a 2-thread core", got)
	}
	if _, err := l.Reserve("q", Request{Mem: 1, CPU: 3000}); !errors.Is(err, ErrNoCPU) {
		t.Fatalf("want ErrNoCPU, got %v", err)
	}
	l.Release("p")
	if l.FreeCPU() != 4000 {
		t.Fatal("unpinning did not return the threads")
	}
}

func TestCPUBudgetResolution(t *testing.T) {
	cores := []Core{{ID: 0, Threads: []int{0, 8}}, {ID: 1, Threads: []int{1, 9}}}
	if got := CPUBudget(6, cores, nil); got != 6000 {
		t.Fatalf("explicit: %d", got)
	}
	if got := CPUBudget(-1, cores, nil); got != 0 {
		t.Fatalf("disabled: %d", got)
	}
	if got := CPUBudget(0, cores, []int{0}); got != 3000 {
		t.Fatalf("auto minus reserved: %d", got)
	}
	if got := CPUBudget(0, nil, nil); got < 1000 {
		t.Fatalf("auto without topology: %d", got)
	}
}
