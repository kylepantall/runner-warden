//go:build !windows

package imagegc

import "syscall"

func diskUsedPercent(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	total := st.Blocks
	if total == 0 {
		return 0, nil
	}
	free := st.Bavail
	return int((total - free) * 100 / total), nil
}
