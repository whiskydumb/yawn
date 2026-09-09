package identity

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastOptions() Options {
	return Options{
		BaseBackoff: 20 * time.Millisecond,
		MaxBackoff:  80 * time.Millisecond,
		MaxStrikes:  3,
	}
}

func newPool(t *testing.T, ids []Identity, opts Options) *Pool {
	t.Helper()
	p, err := NewPool(ids, opts)
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	return p
}

func TestEmptyPoolYieldsAnonymous(t *testing.T) {
	p := newPool(t, nil, fastOptions())
	if p.Len() != 1 {
		t.Fatalf("pool holds %d identities, want 1", p.Len())
	}
	lease, err := p.Acquire(context.Background())
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if lease.Identity.Name != Anonymous {
		t.Errorf("got %q, want the anonymous identity", lease.Identity.Name)
	}
	p.Release(lease, Success)
}

func TestNamesAreFilledInAndUnique(t *testing.T) {
	p := newPool(t, []Identity{{}, {}}, fastOptions())
	names := map[string]bool{}
	for _, s := range p.Status() {
		names[s.Name] = true
	}
	if len(names) != 2 {
		t.Errorf("unnamed identities did not get distinct names: %v", names)
	}

	if _, err := NewPool([]Identity{{Name: "same"}, {Name: "same"}}, fastOptions()); err == nil {
		t.Error("a duplicate identity name must be rejected")
	}
}

func TestAcquirePrefersAnIdleIdentity(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}, {Name: "b"}}, fastOptions())
	ctx := context.Background()

	first, _ := p.Acquire(ctx)
	second, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// using the same cookies from two workers at once is what the pool exists
	// to avoid
	if first.Identity.Name == second.Identity.Name {
		t.Errorf("both workers got %q while another identity was idle", first.Identity.Name)
	}
}

func TestSmallPoolDoesNotStallManyWorkers(t *testing.T) {
	p := newPool(t, []Identity{{Name: "only"}}, fastOptions())
	ctx := context.Background()

	first, _ := p.Acquire(ctx)
	done := make(chan struct{})
	go func() {
		second, err := p.Acquire(ctx)
		if err == nil {
			p.Release(second, Success)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("a single identity blocked a second worker instead of being reused")
	}
	p.Release(first, Success)
}

func TestBlockedIdentityRests(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}}, fastOptions())
	ctx := context.Background()

	lease, _ := p.Acquire(ctx)
	p.Release(lease, Blocked)

	status := p.Status()[0]
	if status.Strikes != 1 {
		t.Errorf("strikes = %d, want 1", status.Strikes)
	}
	if status.RestingFor == 0 && status.Strikes < fastOptions().MaxStrikes {
		t.Error("a blocked identity should be resting")
	}

	// it comes back on its own once the rest is over
	start := time.Now()
	back, err := p.Acquire(ctx)
	if err != nil {
		t.Fatalf("Acquire after backoff: %v", err)
	}
	if time.Since(start) < 10*time.Millisecond {
		t.Error("the identity was handed out again without resting")
	}
	p.Release(back, Success)
}

func TestSuccessClearsTheBackoff(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}}, fastOptions())
	ctx := context.Background()

	lease, _ := p.Acquire(ctx)
	p.Release(lease, Blocked)
	time.Sleep(30 * time.Millisecond)

	lease, _ = p.Acquire(ctx)
	p.Release(lease, Success)

	if s := p.Status()[0]; s.Strikes != 0 || s.RestingFor != 0 {
		t.Errorf("a success must wipe the slate: %+v", s)
	}
}

func TestFailureIsNotHeldAgainstTheIdentity(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}}, fastOptions())
	lease, _ := p.Acquire(context.Background())
	// a removed video is not the credentials' fault
	p.Release(lease, Failed)

	if s := p.Status()[0]; s.Strikes != 0 || s.RestingFor != 0 {
		t.Errorf("an ordinary failure must not punish the identity: %+v", s)
	}
}

func TestRepeatedBlocksRetireAnIdentity(t *testing.T) {
	opts := fastOptions()
	p := newPool(t, []Identity{{Name: "a"}, {Name: "b"}}, opts)
	ctx := context.Background()

	for i := 0; i < opts.MaxStrikes; i++ {
		lease, err := p.Acquire(ctx)
		if err != nil {
			t.Fatalf("Acquire on strike %d: %v", i, err)
		}
		if lease.Identity.Name != "a" {
			p.Release(lease, Success)
			i--
			continue
		}
		p.Release(lease, Blocked)
		time.Sleep(opts.MaxBackoff + 10*time.Millisecond)
	}

	for _, s := range p.Status() {
		if s.Name == "a" && !s.Dead {
			t.Errorf("identity a survived %d blocks: %+v", opts.MaxStrikes, s)
		}
	}
}

func TestAllDeadIsReported(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}}, fastOptions())
	lease, _ := p.Acquire(context.Background())
	p.Retire(lease, "cookies rejected")

	_, err := p.Acquire(context.Background())
	if !errors.Is(err, ErrAllDead) {
		t.Errorf("want ErrAllDead once nothing is left, got %v", err)
	}
}

func TestAcquireRespectsContext(t *testing.T) {
	opts := fastOptions()
	opts.BaseBackoff = time.Hour
	opts.MaxBackoff = time.Hour
	p := newPool(t, []Identity{{Name: "a"}}, opts)

	lease, _ := p.Acquire(context.Background())
	p.Release(lease, Blocked) // resting for an hour

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := p.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want the context deadline, got %v", err)
	}
}

func TestReleaseWakesAWaiter(t *testing.T) {
	opts := fastOptions()
	opts.BaseBackoff = time.Hour
	opts.MaxBackoff = time.Hour
	p := newPool(t, []Identity{{Name: "a"}, {Name: "b"}}, opts)
	ctx := context.Background()

	// put both to rest, then free one and make sure the waiter notices at
	// once instead of sitting out the hour
	for _, want := range []string{"a", "b"} {
		l, _ := p.Acquire(ctx)
		_ = want
		p.Release(l, Blocked)
	}

	woke := make(chan struct{})
	go func() {
		if l, err := p.Acquire(ctx); err == nil {
			p.Release(l, Success)
		}
		close(woke)
	}()

	time.Sleep(20 * time.Millisecond)
	// a success on any identity clears its rest and should wake the waiter
	p.mu.Lock()
	p.all[0].restUntl = time.Time{}
	p.broadcastLocked()
	p.mu.Unlock()

	select {
	case <-woke:
	case <-time.After(time.Second):
		t.Fatal("the waiter slept through an identity becoming available")
	}
}

func TestValidate(t *testing.T) {
	dir := t.TempDir()
	cookies := filepath.Join(dir, "cookies.txt")
	if err := os.WriteFile(cookies, []byte("# Netscape HTTP Cookie File\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		id      Identity
		wantErr string
	}{
		{"empty is fine", Identity{}, ""},
		{"existing cookies", Identity{CookiesFile: cookies}, ""},
		{"missing cookies", Identity{CookiesFile: filepath.Join(dir, "nope.txt")}, "unreadable"},
		{"both cookie sources", Identity{CookiesFile: cookies, CookiesFromBrowser: "firefox"}, "not both"},
		{"http proxy", Identity{Proxy: "http://user:pass@host:8080"}, ""},
		{"socks proxy", Identity{Proxy: "socks5://host:1080"}, ""},
		{"proxy without a scheme", Identity{Proxy: "1.2.3.4:1080"}, "no scheme"},
		{"proxy with a wrong scheme", Identity{Proxy: "ftp://host:21"}, "unsupported scheme"},
		{"proxy without a host", Identity{Proxy: "socks5://"}, "no host"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.id.Validate()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", c.wantErr)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not mention %q", err, c.wantErr)
			}
		})
	}
}

func TestNewPoolValidatesEveryIdentity(t *testing.T) {
	_, err := NewPool([]Identity{{Name: "ok"}, {Name: "bad", Proxy: "ftp://x"}}, fastOptions())
	if err == nil {
		t.Fatal("a broken identity must be rejected at startup, not mid-run")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("the error should name the identity, got %v", err)
	}
}

func TestBackoffDoubles(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}}, Options{
		BaseBackoff: time.Second, MaxBackoff: 8 * time.Second, MaxStrikes: 99,
	})
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 8 * time.Second}
	for i, w := range want {
		if got := p.backoffFor(i + 1); got != w {
			t.Errorf("backoffFor(%d) = %v, want %v", i+1, got, w)
		}
	}
}

func TestConcurrentUseIsSafe(t *testing.T) {
	p := newPool(t, []Identity{{Name: "a"}, {Name: "b"}, {Name: "c"}}, fastOptions())
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, err := p.Acquire(ctx)
			if err != nil {
				return
			}
			outcome := Success
			if i%5 == 0 {
				outcome = Failed
			}
			p.Release(l, outcome)
		}(i)
	}
	wg.Wait()

	for _, s := range p.Status() {
		if s.Dead {
			t.Errorf("identity %s died on ordinary traffic: %+v", s.Name, s)
		}
	}
}

func TestDescribe(t *testing.T) {
	if got := newPool(t, nil, fastOptions()).Describe(); !strings.Contains(got, "anonymous") {
		t.Errorf("Describe = %q, want it to mention the anonymous identity", got)
	}
	got := newPool(t, []Identity{{Name: "home"}, {Name: "vps"}}, fastOptions()).Describe()
	if !strings.Contains(got, "home") || !strings.Contains(got, "vps") || !strings.Contains(got, "2") {
		t.Errorf("Describe = %q, want it to list both identities", got)
	}
}
