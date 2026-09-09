package pipeline

import (
	"context"
	"fmt"
	"sync"
)

// Budget keeps the total weight of files on disk under a ceiling. This is
// what lets yawn mirror an archive larger than the disk: a download takes
// budget, an upload returns it once the file is deleted.
//
// The reservation outlives the handover between worker pools, which is why
// this is not a semaphore.Weighted: there Acquire and Release must be paired
// in one goroutine, and here two different pools sit between them.
type Budget struct {
	mu       sync.Mutex
	capacity int64
	used     int64
	held     map[int64]int64 // itemID -> reserved bytes
	waiters  chan struct{}   // closed on every release
}

// NewBudget creates a budget. A capacity of zero or less means no limit.
func NewBudget(capacity int64) *Budget {
	return &Budget{
		capacity: capacity,
		held:     make(map[int64]int64),
		waiters:  make(chan struct{}),
	}
}

func (b *Budget) Capacity() int64 { return b.capacity }

func (b *Budget) Used() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

// Acquire reserves room for a video, waiting when there is none. A request
// larger than the whole budget is clamped to it, otherwise one oversized
// video would wedge the pipeline forever.
func (b *Budget) Acquire(ctx context.Context, itemID, n int64) error {
	if b.capacity <= 0 {
		return nil
	}
	if n <= 0 {
		n = 1
	}
	if n > b.capacity {
		n = b.capacity
	}
	for {
		b.mu.Lock()
		// a repeat reservation for the same video tops up the difference
		// instead of charging again, or every retry would count the file anew
		need := n - b.held[itemID]
		if need <= 0 {
			b.mu.Unlock()
			return nil
		}
		if b.used+need <= b.capacity {
			b.used += need
			b.held[itemID] += need
			b.mu.Unlock()
			return nil
		}
		wait := b.waiters
		b.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
	}
}

// Adjust corrects a reservation once the real file size is known. Anything
// over-reserved goes back to the budget immediately.
func (b *Budget) Adjust(itemID, actual int64) {
	if b.capacity <= 0 {
		return
	}
	if actual < 0 {
		actual = 0
	}
	if actual > b.capacity {
		actual = b.capacity
	}
	b.mu.Lock()
	prev, ok := b.held[itemID]
	if !ok {
		b.mu.Unlock()
		return
	}
	delta := actual - prev
	if b.used+delta < 0 {
		delta = -b.used
	}
	b.used += delta
	b.held[itemID] = prev + delta
	if delta < 0 {
		b.broadcastLocked()
	}
	b.mu.Unlock()
}

// Release returns the budget held by a video. Calling it twice is harmless.
func (b *Budget) Release(itemID int64) {
	if b.capacity <= 0 {
		return
	}
	b.mu.Lock()
	n, ok := b.held[itemID]
	if ok {
		delete(b.held, itemID)
		b.used -= n
		if b.used < 0 {
			b.used = 0
		}
		b.broadcastLocked()
	}
	b.mu.Unlock()
}

// Charge takes budget without waiting, for files a previous run left on
// disk. The ceiling may be exceeded: lying to ourselves about used space is
// worse than briefly going over.
func (b *Budget) Charge(itemID, n int64) {
	if b.capacity <= 0 || n <= 0 {
		return
	}
	b.mu.Lock()
	b.used += n
	b.held[itemID] += n
	b.mu.Unlock()
}

func (b *Budget) broadcastLocked() {
	close(b.waiters)
	b.waiters = make(chan struct{})
}

func (b *Budget) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return fmt.Sprintf("%d/%d bytes in flight", b.used, b.capacity)
}
