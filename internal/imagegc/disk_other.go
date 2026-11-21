//go:build windows

package imagegc

import "errors"

func diskUsedPercent(string) (int, error) {
	return 0, errors.New("disk usage unsupported on this platform")
}
