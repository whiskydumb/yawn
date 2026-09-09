package pipeline

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestBudgetBlocksUntilReleased(t *testing.T) {
	b := NewBudget(100)
	ctx := context.Background()

	if err := b.Acquire(ctx, 1, 60); err != nil {
		t.Fatalf("first reservation: %v", err)
	}

	acquired := make(chan struct{})
	go func() {
		if err := b.Acquire(ctx, 2, 60); err == nil {
			close(acquired)
		}
	}()

	select {
	case <-acquired:
		t.Fatal("the second reservation went through with no budget for it")
	case <-time.After(50 * time.Millisecond):
	}

	b.Release(1)

	select {
	case <-acquired:
	case <-time.After(time.Second):
		t.Fatal("the second reservation never went through after a release")
	}
	if got := b.Used(); got != 60 {
		t.Errorf("used = %d, want 60", got)
	}
}

func TestBudgetClampsOversizedRequest(t *testing.T) {
	b := NewBudget(100)
	// a video heavier than the whole budget must not wedge the pipeline
	done := make(chan error, 1)
	go func() { done <- b.Acquire(context.Background(), 1, 500) }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("an oversized reservation returned an error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("an oversized reservation deadlocked")
	}
	if got := b.Used(); got != 100 {
		t.Errorf("used = %d, want it clamped to the ceiling of 100", got)
	}
}

func TestBudgetAdjustReturnsOverestimate(t *testing.T) {
	b := NewBudget(100)
	ctx := context.Background()
	if err := b.Acquire(ctx, 1, 80); err != nil {
		t.Fatal(err)
	}
	// the real file turned out lighter, the difference must come back at once
	b.Adjust(1, 30)
	if got := b.Used(); got != 30 {
		t.Fatalf("used = %d after the adjustment, want 30", got)
	}
	if err := b.Acquire(ctx, 2, 60); err != nil {
		t.Errorf("the freed room was not enough: %v", err)
	}
}

func TestBudgetAdjustWakesWaiter(t *testing.T) {
	b := NewBudget(100)
	ctx := context.Background()
	if err := b.Acquire(ctx, 1, 100); err != nil {
		t.Fatal(err)
	}

	woke := make(chan struct{})
	go func() {
		if err := b.Acquire(ctx, 2, 50); err == nil {
			close(woke)
		}
	}()
	time.Sleep(20 * time.Millisecond)
	b.Adjust(1, 40)

	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("adjusting the size did not wake the waiter")
	}
}

func TestBudgetRespectsContextCancel(t *testing.T) {
	b := NewBudget(10)
	if err := b.Acquire(context.Background(), 1, 10); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := b.Acquire(ctx, 2, 5); err != context.Canceled {
		t.Errorf("want context.Canceled, got %v", err)
	}
}

func TestBudgetReleaseIsIdempotent(t *testing.T) {
	b := NewBudget(100)
	if err := b.Acquire(context.Background(), 1, 50); err != nil {
		t.Fatal(err)
	}
	b.Release(1)
	b.Release(1)
	if got := b.Used(); got != 0 {
		t.Errorf("used = %d after a double release, want 0", got)
	}
}

func TestBudgetRetryDoesNotDoubleCharge(t *testing.T) {
	b := NewBudget(100)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := b.Acquire(ctx, 1, 40); err != nil {
			t.Fatalf("reservation %d: %v", i, err)
		}
	}
	if got := b.Used(); got != 40 {
		t.Errorf("used = %d after five retries of one video, want 40", got)
	}
}

func TestBudgetZeroCapacityIsUnlimited(t *testing.T) {
	b := NewBudget(0)
	for i := int64(0); i < 100; i++ {
		if err := b.Acquire(context.Background(), i, 1<<30); err != nil {
			t.Fatalf("an unlimited budget refused: %v", err)
		}
	}
}

func TestBudgetIsRaceFree(t *testing.T) {
	b := NewBudget(1000)
	ctx := context.Background()
	var wg sync.WaitGroup
	for i := int64(0); i < 50; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			if err := b.Acquire(ctx, id, 100); err != nil {
				return
			}
			b.Adjust(id, 50)
			b.Release(id)
		}(i)
	}
	wg.Wait()
	if got := b.Used(); got != 0 {
		t.Errorf("used = %d once everything is done, want 0", got)
	}
}
