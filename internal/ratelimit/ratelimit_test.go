package ratelimit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNilLimiterIsNoLimit(t *testing.T) {
	var l *Limiter
	if l.Limit() != 0 {
		t.Error("a nil limiter must report no limit")
	}
	src := strings.NewReader("payload")
	if got := l.Reader(context.Background(), src); got != io.Reader(src) {
		t.Error("a nil limiter must hand the reader back untouched")
	}
	if New(0) != nil || New(-1) != nil {
		t.Error("a non-positive speed must yield no limiter")
	}
}

func TestReaderDeliversEveryByte(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 4096) // 32 KiB
	l := New(1 << 30)                                 // fast enough not to slow the test

	got, err := io.ReadAll(l.Reader(context.Background(), bytes.NewReader(payload)))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Error("the throttled reader changed the data")
	}
}

func TestReaderActuallySlowsDown(t *testing.T) {
	const speed = 256 << 10 // 256 KiB/s
	payload := bytes.Repeat([]byte("x"), 512<<10)

	l := New(speed)
	start := time.Now()
	if _, err := io.ReadAll(l.Reader(context.Background(), bytes.NewReader(payload))); err != nil {
		t.Fatalf("read: %v", err)
	}
	elapsed := time.Since(start)

	// the first burst is free, so 512 KiB at 256 KiB/s takes about a second
	if elapsed < 700*time.Millisecond {
		t.Errorf("512 KiB at 256 KiB/s took %v, which is too fast to be limited", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Errorf("took %v, far slower than the limit asks for", elapsed)
	}
}

func TestLimitIsSharedAcrossReaders(t *testing.T) {
	const speed = 256 << 10
	l := New(speed)

	// two readers at once must together stay within the ceiling, which is
	// exactly what a per worker limiter would get wrong
	payload := bytes.Repeat([]byte("x"), 256<<10)
	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			io.ReadAll(l.Reader(context.Background(), bytes.NewReader(payload)))
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)

	if elapsed < 700*time.Millisecond {
		t.Errorf("two readers moved 512 KiB in %v, so the limit was applied per reader", elapsed)
	}
}

func TestReaderRespectsContext(t *testing.T) {
	l := New(1 << 10) // slow enough that the wait will not finish
	payload := bytes.Repeat([]byte("x"), 1<<20)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := io.ReadAll(l.Reader(ctx, bytes.NewReader(payload)))
	if err == nil {
		t.Fatal("a cancelled context must stop the read")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the context deadline, got %v", err)
	}
}

func TestSmallLimitStillWorks(t *testing.T) {
	// a burst below one read would make every read fail, so it is raised
	l := New(100)
	if l.burst < minBurst {
		t.Errorf("burst = %d, want at least %d", l.burst, minBurst)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	buf := make([]byte, 64)
	n, err := l.Reader(ctx, bytes.NewReader(bytes.Repeat([]byte("x"), 64))).Read(buf)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("read at a tiny limit: %v", err)
	}
	if n != 64 {
		t.Errorf("read %d bytes, want 64", n)
	}
}

func TestLargeReadIsSplitToTheBurst(t *testing.T) {
	l := New(64 << 10) // burst becomes 64 KiB
	huge := make([]byte, 1<<20)

	n, err := l.Reader(context.Background(), bytes.NewReader(huge)).Read(huge)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// without the split WaitN would refuse a request bigger than the burst
	if n > l.burst {
		t.Errorf("read %d bytes in one go, the burst is %d", n, l.burst)
	}
	if n == 0 {
		t.Error("the split swallowed the whole read")
	}
}

func TestLimitReportsTheSpeed(t *testing.T) {
	if got := New(3 << 20).Limit(); got != 3<<20 {
		t.Errorf("Limit = %d, want %d", got, 3<<20)
	}
}
