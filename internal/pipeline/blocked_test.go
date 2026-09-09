package pipeline

import "testing"

func TestBlockedRetriesAreFreeButBounded(t *testing.T) {
	p := &Pipeline{blockedRetries: make(map[int64]int)}

	for i := 1; i <= maxBlockedRetries; i++ {
		if !p.chargeBlockedRetry(1) {
			t.Fatalf("free retry %d was refused; rate limiting is not the video's fault", i)
		}
	}
	if p.chargeBlockedRetry(1) {
		t.Error("free retries must be bounded, or a permanent block loops forever")
	}
}

func TestBlockedRetriesAreCountedPerItem(t *testing.T) {
	p := &Pipeline{blockedRetries: make(map[int64]int)}

	for i := 0; i < maxBlockedRetries; i++ {
		p.chargeBlockedRetry(1)
	}
	if !p.chargeBlockedRetry(2) {
		t.Error("one exhausted video must not spend another video's retries")
	}
}
