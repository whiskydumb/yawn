package lockfile

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireIsExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer first.Release()

	if _, err := Acquire(path); !errors.Is(err, ErrBusy) {
		t.Fatalf("second acquire returned %v, want ErrBusy", err)
	}
}

func TestReleaseAllowsReacquire(t *testing.T) {
	path := filepath.Join(t.TempDir(), "queue.lock")

	first, err := Acquire(path)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}

	second, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	second.Release()
}

func TestReleaseIsSafeOnNil(t *testing.T) {
	var l *Lock
	if err := l.Release(); err != nil {
		t.Errorf("releasing a nil lock returned %v", err)
	}
}

func TestAcquireCreatesMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "queue.lock")
	l, err := Acquire(path)
	if err != nil {
		t.Fatalf("acquire in a missing directory: %v", err)
	}
	l.Release()
}
