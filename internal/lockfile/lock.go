package lockfile

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("file is locked by another process")

type Lock struct {
	path string
	file *os.File
}

// Acquire takes an exclusive lock without waiting, returning ErrBusy if
// another process holds it.
func Acquire(path string) (*Lock, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("cannot create lock directory %s: %w", dir, err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file %s: %w", path, err)
	}
	if err := lockFile(f); err != nil {
		f.Close()
		if errors.Is(err, ErrBusy) {
			return nil, ErrBusy
		}
		return nil, fmt.Errorf("cannot take the lock on %s: %w", path, err)
	}
	// the pid is written for whoever finds a stale file and wonders who holds
	// it; it plays no part in the locking itself
	f.Truncate(0)
	f.Seek(0, 0)
	fmt.Fprintf(f, "%d\n", os.Getpid())
	f.Sync()

	return &Lock{path: path, file: f}, nil
}

func (l *Lock) Release() error {
	if l == nil || l.file == nil {
		return nil
	}
	err := unlockFile(l.file)
	closeErr := l.file.Close()
	l.file = nil
	os.Remove(l.path)
	if err != nil {
		return err
	}
	return closeErr
}
