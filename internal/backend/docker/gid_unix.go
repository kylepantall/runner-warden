//go:build !windows

package docker

import (
	"fmt"
	"os"
	"syscall"
)

// socketGID returns the owning group of a file (the Docker socket).
func socketGID(path string) (int, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no stat info for %s", path)
	}
	return int(st.Gid), nil
}
