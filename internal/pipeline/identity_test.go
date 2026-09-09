package pipeline

import (
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/media"
)

func TestFlatIdentityStillWorks(t *testing.T) {
	cfg := config.Default()
	cfg.Identity.Proxy = "socks5://10.0.0.5:1080"
	cfg.Identity.UserAgent = "yawn/test"

	p, err := newPipeline(t, cfg, media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := p.Identities().Len(); n != 1 {
		t.Fatalf("pool holds %d identities, want the single flat one", n)
	}
	lease, err := p.Identities().Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Identity.Proxy != "socks5://10.0.0.5:1080" {
		t.Errorf("the flat proxy did not reach the identity: %+v", lease.Identity)
	}
}

func TestPoolReplacesTheFlatFields(t *testing.T) {
	cfg := config.Default()
	cfg.Identity.Proxy = "socks5://ignored:1080"
	cfg.Identity.Pool = []config.PoolIdentity{
		{Name: "home"},
		{Name: "vps", Proxy: "socks5://10.0.0.5:1080"},
	}

	p, err := newPipeline(t, cfg, media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := p.Identities().Len(); n != 2 {
		t.Fatalf("pool holds %d identities, want 2", n)
	}
	for _, s := range p.Identities().Status() {
		if s.Proxy == "socks5://ignored:1080" {
			t.Error("the flat proxy leaked into the pool")
		}
	}
}

func TestNoIdentityConfiguredIsAnonymous(t *testing.T) {
	p, err := newPipeline(t, config.Default(), media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if n := p.Identities().Len(); n != 1 {
		t.Fatalf("pool holds %d identities, want one anonymous", n)
	}
	if got := p.Identities().Describe(); !strings.Contains(got, "anonymous") {
		t.Errorf("Describe = %q, want the anonymous identity", got)
	}
}

func TestBrokenIdentityFailsAtStartup(t *testing.T) {
	cfg := config.Default()
	cfg.Identity.Pool = []config.PoolIdentity{{Name: "bad", Proxy: "1.2.3.4:1080"}}

	_, err := newPipeline(t, cfg, media.NewConverter(""))
	if err == nil {
		t.Fatal("a proxy without a scheme must be rejected before any download")
	}
	if !strings.Contains(err.Error(), "identity") {
		t.Errorf("the error should point at the identity section, got %v", err)
	}
}

func TestReleaseIdentityJudgesTheOutcome(t *testing.T) {
	p, err := newPipeline(t, config.Default(), media.NewConverter(""))
	if err != nil {
		t.Fatal(err)
	}
	pool := p.Identities()

	lease, _ := pool.Acquire(t.Context())
	p.releaseIdentity(lease, nil)
	if s := pool.Status()[0]; s.Strikes != 0 {
		t.Errorf("a success left strikes behind: %+v", s)
	}

	lease, _ = pool.Acquire(t.Context())
	p.releaseIdentity(lease, errRemovedVideo)
	if s := pool.Status()[0]; s.Strikes != 0 {
		t.Errorf("a removed video must not be blamed on the identity: %+v", s)
	}

	lease, _ = pool.Acquire(t.Context())
	p.releaseIdentity(lease, errBlocked)
	if s := pool.Status()[0]; s.Strikes != 1 {
		t.Errorf("a refusal must count against the identity: %+v", s)
	}
}

var (
	errRemovedVideo = errTest("video unavailable")
	errBlocked      = wrapBlocked()
)

type errTest string

func (e errTest) Error() string { return string(e) }

func wrapBlocked() error {
	return blockedError{}
}

type blockedError struct{}

func (blockedError) Error() string { return "blocked by bot protection" }
func (blockedError) Unwrap() error { return extract.ErrBlocked }
