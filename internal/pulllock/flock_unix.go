//go:build !windows

package pulllock

import (
	"errors"
	"os"
	"syscall"
)

// tryFlock attempts a non-blocking flock; false means it is held elsewhere.
func tryFlock(f *os.File, exclusive bool) (bool, error) {
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	for {
		err := syscall.Flock(int(f.Fd()), how|syscall.LOCK_NB) //nolint:gosec // fd fits int
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}
