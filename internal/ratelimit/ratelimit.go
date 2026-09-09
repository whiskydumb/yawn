package ratelimit

import (
	"context"
	"fmt"
	"io"

	"golang.org/x/time/rate"
)

// minBurst keeps the limiter usable at tiny speeds: the burst has to be at
// least one read, or every read would exceed it and fail outright.
const minBurst = 32 << 10

// Limiter caps a shared byte rate.
type Limiter struct {
	limiter *rate.Limiter
	burst   int
}

// New builds a limiter for the given speed in bytes per second.
// A speed of zero or less means no limit, and nil is returned.
func New(bytesPerSecond int64) *Limiter {
	if bytesPerSecond <= 0 {
		return nil
	}
	burst := int(bytesPerSecond)
	if burst < minBurst {
		burst = minBurst
	}
	return &Limiter{
		limiter: rate.NewLimiter(rate.Limit(bytesPerSecond), burst),
		burst:   burst,
	}
}

// Limit reports the configured speed in bytes per second, or zero.
func (l *Limiter) Limit() int64 {
	if l == nil {
		return 0
	}
	return int64(l.limiter.Limit())
}

// Reader wraps r so reading from it respects the shared limit.
// A nil limiter returns the reader untouched, which keeps the fast path free
// of any wrapping at all.
func (l *Limiter) Reader(ctx context.Context, r io.Reader) io.Reader {
	if l == nil {
		return r
	}
	return &limitedReader{ctx: ctx, r: r, l: l}
}

type limitedReader struct {
	ctx context.Context
	r   io.Reader
	l   *Limiter
}

func (lr *limitedReader) Read(p []byte) (int, error) {
	// a read larger than the burst can never be granted, so it is split
	if len(p) > lr.l.burst {
		p = p[:lr.l.burst]
	}
	n, err := lr.r.Read(p)
	if n <= 0 {
		return n, err
	}
	// the wait comes after the read: the bytes are already in hand, and
	// waiting first would just add a delay before the very last chunk
	if waitErr := lr.l.limiter.WaitN(lr.ctx, n); waitErr != nil {
		// Callers tell a graceful stop from a real failure by matching on the
		// context errors, so those have to survive. A cancelled context gives
		// its own error straight from the limiter.
		if ctxErr := lr.ctx.Err(); ctxErr != nil {
			return n, ctxErr
		}
		// The limiter also refuses up front when the wait would outlast a
		// deadline, before that deadline has actually passed. Splitting reads
		// to the burst rules out its only other failure, so this branch is
		// the deadline talking and is reported as such.
		if _, hasDeadline := lr.ctx.Deadline(); hasDeadline {
			return n, fmt.Errorf("%w: %v", context.DeadlineExceeded, waitErr)
		}
		return n, waitErr
	}
	return n, err
}
