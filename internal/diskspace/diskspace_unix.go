//go:build !windows

package diskspace

import (
	"fmt"
	"syscall"
)

func free(dir string) (int64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return 0, fmt.Errorf("cannot read free space on %s: %w", dir, err)
	}
	// Bavail rather than Bfree: some blocks are reserved for root and an
	// ordinary process cannot use them.
	return int64(st.Bavail) * int64(st.Bsize), nil
}
