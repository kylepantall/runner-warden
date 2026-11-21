//go:build windows

package pulllock

import "os"

// The Docker proxy is Linux-only; on Windows the lock is a no-op.
func tryFlock(*os.File, bool) (bool, error) { return true, nil }
