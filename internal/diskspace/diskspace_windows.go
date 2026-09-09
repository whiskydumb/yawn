//go:build windows

package diskspace

import (
	"fmt"

	"golang.org/x/sys/windows"
)

func free(dir string) (int64, error) {
	pathPtr, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, fmt.Errorf("invalid path %q: %w", dir, err)
	}
	// FreeBytesAvailableToCaller rather than the volume total: with disk
	// quotas on those differ, and we can only write what is available to us.
	var availableToCaller, totalBytes, totalFree uint64
	if err := windows.GetDiskFreeSpaceEx(pathPtr, &availableToCaller, &totalBytes, &totalFree); err != nil {
		return 0, fmt.Errorf("cannot read free space on %s: %w", dir, err)
	}
	return int64(availableToCaller), nil
}
