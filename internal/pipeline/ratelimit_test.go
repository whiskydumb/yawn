package pipeline

import (
	"testing"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/media"
)

func TestUploadRateComesFromTheConfig(t *testing.T) {
	cfg := config.Default()
	cfg.Upload.RateLimit = 3 << 20

	p, err := newPipeline(t, cfg, media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.UploadRate(); got != 3<<20 {
		t.Errorf("UploadRate = %d, want %d", got, 3<<20)
	}
	if p.throttle() == nil {
		t.Error("a configured limit must reach the upload requests")
	}
}

func TestNoUploadLimitLeavesRequestsUnthrottled(t *testing.T) {
	p, err := newPipeline(t, config.Default(), media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := p.UploadRate(); got != 0 {
		t.Errorf("UploadRate = %d, want 0 by default", got)
	}
	// a nil *Limiter placed in an interface would be a non-nil interface, and
	// every upload would take the throttled path for nothing
	if p.throttle() != nil {
		t.Error("an unset limit must leave the throttle interface nil")
	}
}
