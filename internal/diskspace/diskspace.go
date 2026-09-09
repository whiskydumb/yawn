package diskspace

import (
	"fmt"
	"os"
	"path/filepath"
)

// Free returns the free bytes on the volume holding path. A path that does
// not exist yet is walked up to its nearest existing parent.
func Free(path string) (int64, error) {
	probe, err := existingAncestor(path)
	if err != nil {
		return 0, err
	}
	return free(probe)
}

func existingAncestor(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("cannot resolve path %q: %w", path, err)
	}
	for {
		if _, err := os.Stat(abs); err == nil {
			return abs, nil
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return "", fmt.Errorf("found no existing directory for path %q", path)
		}
		abs = parent
	}
}
