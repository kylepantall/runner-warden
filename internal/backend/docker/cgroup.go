package docker

import (
	"bufio"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// expandSlice turns "a-b-c.slice" into systemd's nested path
// "a.slice/a-b.slice/a-b-c.slice".
func expandSlice(name string) string {
	base := strings.TrimSuffix(name, ".slice")
	if base == "" || base == "-" {
		return ""
	}
	parts := strings.Split(base, "-")
	out := make([]string, 0, len(parts))
	for i := range parts {
		out = append(out, strings.Join(parts[:i+1], "-")+".slice")
	}
	return filepath.Join(out...)
}

// findCgroup locates a container's cgroup v2 directory under root. The systemd
// driver nests "x-y.slice" as x.slice/x-y.slice; the cgroupfs driver uses the
// bare id.
func findCgroup(root, slice, id string) (string, bool) {
	trim := strings.Trim(slice, "/")
	candidates := []string{
		filepath.Join(root, expandSlice(slice), "docker-"+id+".scope"),
		filepath.Join(root, trim, "docker-"+id+".scope"),
		filepath.Join(root, trim, id),
		filepath.Join(root, "system.slice", "docker-"+id+".scope"),
	}
	for _, c := range candidates {
		if st, err := os.Stat(c); err == nil && st.IsDir() {
			return c, true
		}
	}
	// Last resort: search below the slice's top-level ancestor.
	top := root
	if e := expandSlice(slice); e != "" {
		top = filepath.Join(root, strings.SplitN(e, string(filepath.Separator), 2)[0])
	} else if trim != "" {
		top = filepath.Join(root, trim)
	}
	var found string
	want := "docker-" + id + ".scope"
	_ = filepath.WalkDir(top, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fs.SkipDir
		}
		if d.IsDir() && (d.Name() == want || d.Name() == id) {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	return found, found != ""
}

type cgroupSample struct {
	cpuSeconds float64
	memCurrent int64
	memPeak    int64
	anon       int64
	oomKills   int
	ioBytes    int64
}

// readCgroup reads the cgroup v2 files of one container. ok is false when the
// core files (cpu.stat, memory.current) are unreadable.
func readCgroup(dir string) (s cgroupSample, ok bool) {
	cpu, err1 := readKV(filepath.Join(dir, "cpu.stat"))
	cur, err2 := readUint(filepath.Join(dir, "memory.current"))
	if err1 != nil || err2 != nil {
		return s, false
	}
	s.cpuSeconds = float64(cpu["usage_usec"]) / 1e6
	s.memCurrent = clampInt64(cur)
	if v, err := readUint(filepath.Join(dir, "memory.peak")); err == nil {
		s.memPeak = clampInt64(v)
	}
	if m, err := readKV(filepath.Join(dir, "memory.stat")); err == nil {
		s.anon = clampInt64(m["anon"])
	}
	if m, err := readKV(filepath.Join(dir, "memory.events")); err == nil {
		s.oomKills = int(min(m["oom_kill"], math.MaxInt32))
	}
	s.ioBytes = readIOBytes(filepath.Join(dir, "io.stat"))
	return s, true
}

// clampInt64 converts a kernel counter without wrapping on overflow.
func clampInt64(v uint64) int64 {
	if v > math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(v)
}

func readUint(path string) (uint64, error) {
	b, err := os.ReadFile(path) //nolint:gosec // cgroup file path built from the runner cgroup dir
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readKV parses "key value" lines.
func readKV(path string) (map[string]uint64, error) {
	f, err := os.Open(path) //nolint:gosec // cgroup file path built from the runner cgroup dir
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]uint64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		if n, err := strconv.ParseUint(strings.TrimSpace(v), 10, 64); err == nil {
			out[k] = n
		}
	}
	return out, sc.Err()
}

// readIOBytes sums rbytes and wbytes over every device in io.stat
// ("8:0 rbytes=1 wbytes=2 rios=..."). Missing file counts as zero.
func readIOBytes(path string) int64 {
	f, err := os.Open(path) //nolint:gosec // cgroup file path built from the runner cgroup dir
	if err != nil {
		return 0
	}
	defer f.Close()
	var total int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		for _, field := range strings.Fields(sc.Text())[1:] {
			k, v, ok := strings.Cut(field, "=")
			if !ok || (k != "rbytes" && k != "wbytes") {
				continue
			}
			if n, err := strconv.ParseInt(v, 10, 64); err == nil {
				total += n
			}
		}
	}
	return total
}

// dirSize totals regular file sizes below dir; a missing dir is 0.
func dirSize(dir string) int64 {
	var n int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			n += info.Size()
		}
		return nil
	})
	return n
}
