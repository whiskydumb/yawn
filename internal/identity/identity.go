package identity

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Identity is one way of presenting ourselves to the site.
type Identity struct {
	Name               string
	CookiesFile        string
	CookiesFromBrowser string
	Proxy              string
	UserAgent          string
}

// Anonymous is the identity used when nothing is configured: no cookies, no
// proxy, whatever address the machine has.
const Anonymous = "anonymous"

// Validate rejects an identity that cannot work, before a single download.
func (i Identity) Validate() error {
	if i.CookiesFile != "" && i.CookiesFromBrowser != "" {
		return errors.New("set either cookies_file or cookies_from_browser, not both")
	}
	if i.CookiesFile != "" {
		if _, err := os.Stat(i.CookiesFile); err != nil {
			return fmt.Errorf("cookies file %s is unreadable: %w", i.CookiesFile, err)
		}
	}
	if i.Proxy != "" {
		// checked before url.Parse, whose message for a bare host:port is
		// "first path segment in URL cannot contain colon" and helps nobody
		if !strings.Contains(i.Proxy, "://") {
			return fmt.Errorf("proxy %q has no scheme, write it as http://host:port or socks5://host:port",
				i.Proxy)
		}
		u, err := url.Parse(i.Proxy)
		if err != nil {
			return fmt.Errorf("proxy %q does not parse: %w", i.Proxy, err)
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return fmt.Errorf("proxy %q: unsupported scheme %q, use http, https, socks5 or socks5h",
				i.Proxy, u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("proxy %q has no host", i.Proxy)
		}
	}
	return nil
}

// Outcome is how a download that used an identity ended.
type Outcome int

const (
	// Success resets the identity's backoff.
	Success Outcome = iota

	// Blocked means a bot wall or a 429: the identity needs a rest, and the
	// video itself is fine.
	Blocked

	// Failed is anything else. It is not held against the identity, since a
	// removed video is not the credentials' fault.
	Failed
)

// Options tune the backoff.
type Options struct {
	// BaseBackoff is how long a blocked identity rests the first time.
	BaseBackoff time.Duration

	// MaxBackoff caps the doubling.
	MaxBackoff time.Duration

	// MaxStrikes is how many blocks in a row retire an identity for this run.
	MaxStrikes int
}

// DefaultOptions are deliberately patient: being turned away is a signal to
// slow down, and hammering makes it worse.
func DefaultOptions() Options {
	return Options{
		BaseBackoff: time.Minute,
		MaxBackoff:  30 * time.Minute,
		MaxStrikes:  5,
	}
}

type entry struct {
	id       Identity
	inUse    int
	strikes  int
	restUntl time.Time
	lastUsed time.Time
	dead     bool
	uses     int
}

// Pool hands out identities and remembers which ones are resting.
type Pool struct {
	mu   sync.Mutex
	opts Options
	all  []*entry

	// waiters is closed and replaced whenever an identity becomes available,
	// which is how a caller blocked on an empty pool learns to look again.
	waiters chan struct{}
}

// Lease is a borrowed identity. It must be returned with Release.
type Lease struct {
	Identity Identity
	entry    *entry
}

// NewPool builds a pool. An empty list yields a single anonymous identity, so
// callers never have to special case "no identities configured".
func NewPool(ids []Identity, opts Options) (*Pool, error) {
	if opts.BaseBackoff <= 0 {
		opts.BaseBackoff = DefaultOptions().BaseBackoff
	}
	if opts.MaxBackoff <= 0 {
		opts.MaxBackoff = DefaultOptions().MaxBackoff
	}
	if opts.MaxStrikes <= 0 {
		opts.MaxStrikes = DefaultOptions().MaxStrikes
	}
	if len(ids) == 0 {
		ids = []Identity{{Name: Anonymous}}
	}

	seen := make(map[string]struct{}, len(ids))
	entries := make([]*entry, 0, len(ids))
	for i, id := range ids {
		if id.Name == "" {
			id.Name = fmt.Sprintf("identity-%d", i+1)
		}
		if _, dup := seen[id.Name]; dup {
			return nil, fmt.Errorf("identity %q is declared twice", id.Name)
		}
		seen[id.Name] = struct{}{}
		if err := id.Validate(); err != nil {
			return nil, fmt.Errorf("identity %q: %w", id.Name, err)
		}
		entries = append(entries, &entry{id: id})
	}
	return &Pool{opts: opts, all: entries, waiters: make(chan struct{})}, nil
}

// Len is how many identities the pool holds.
func (p *Pool) Len() int { return len(p.all) }

// ErrAllDead means every identity has been retired and nothing can run.
var ErrAllDead = errors.New("every identity is exhausted")

// Acquire borrows an identity, waiting while all of them are resting.
//
// An idle identity is preferred over a busy one: using the same cookies from
// three workers at once is exactly what the pool exists to avoid. When none
// is idle the least recently used one is reused rather than blocking, because
// a small pool must not stall a large worker count.
func (p *Pool) Acquire(ctx context.Context) (*Lease, error) {
	for {
		p.mu.Lock()
		best, wait, alive := p.pickLocked()
		if best != nil {
			best.inUse++
			best.uses++
			best.lastUsed = time.Now()
			p.mu.Unlock()
			return &Lease{Identity: best.id, entry: best}, nil
		}
		if !alive {
			p.mu.Unlock()
			return nil, ErrAllDead
		}
		waiters := p.waiters
		p.mu.Unlock()

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-waiters:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// pickLocked returns the identity to use, or how long to wait for the first
// one to finish resting.
func (p *Pool) pickLocked() (best *entry, wait time.Duration, alive bool) {
	now := time.Now()
	soonest := time.Duration(0)

	for _, e := range p.all {
		if e.dead {
			continue
		}
		alive = true
		if e.restUntl.After(now) {
			d := e.restUntl.Sub(now)
			if soonest == 0 || d < soonest {
				soonest = d
			}
			continue
		}
		if best == nil || better(e, best) {
			best = e
		}
	}
	if soonest == 0 {
		soonest = time.Second
	}
	return best, soonest, alive
}

// better prefers an idle identity, then the one used longest ago.
func better(a, b *entry) bool {
	if a.inUse != b.inUse {
		return a.inUse < b.inUse
	}
	return a.lastUsed.Before(b.lastUsed)
}

// Release returns an identity and records how the download went.
func (p *Pool) Release(l *Lease, outcome Outcome) {
	if l == nil || l.entry == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	e := l.entry
	if e.inUse > 0 {
		e.inUse--
	}

	switch outcome {
	case Success:
		e.strikes = 0
		e.restUntl = time.Time{}
	case Blocked:
		e.strikes++
		if e.strikes >= p.opts.MaxStrikes {
			e.dead = true
			e.restUntl = time.Time{}
		} else {
			e.restUntl = time.Now().Add(p.backoffFor(e.strikes))
		}
	case Failed:
		// not the identity's fault, so nothing is held against it
	}
	p.broadcastLocked()
}

// Retire takes an identity out of the run for good: rejected cookies or a
// proxy that will not answer are not going to fix themselves.
func (p *Pool) Retire(l *Lease, reason string) {
	if l == nil || l.entry == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if l.entry.inUse > 0 {
		l.entry.inUse--
	}
	l.entry.dead = true
	p.broadcastLocked()
}

func (p *Pool) backoffFor(strikes int) time.Duration {
	d := p.opts.BaseBackoff
	for i := 1; i < strikes; i++ {
		d *= 2
		if d >= p.opts.MaxBackoff {
			return p.opts.MaxBackoff
		}
	}
	return d
}

func (p *Pool) broadcastLocked() {
	close(p.waiters)
	p.waiters = make(chan struct{})
}

// Status is a snapshot of one identity, for `yawn status` and the dashboard.
type Status struct {
	Name       string
	Proxy      string
	Uses       int
	Strikes    int
	Dead       bool
	RestingFor time.Duration
}

// Status reports the state of every identity, ordered by name.
func (p *Pool) Status() []Status {
	p.mu.Lock()
	defer p.mu.Unlock()

	now := time.Now()
	out := make([]Status, 0, len(p.all))
	for _, e := range p.all {
		s := Status{
			Name:    e.id.Name,
			Proxy:   e.id.Proxy,
			Uses:    e.uses,
			Strikes: e.strikes,
			Dead:    e.dead,
		}
		if e.restUntl.After(now) {
			s.RestingFor = e.restUntl.Sub(now)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Describe renders the pool as one line for the run header.
func (p *Pool) Describe() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.all) == 1 && p.all[0].id.Name == Anonymous {
		return "anonymous, no cookies or proxy"
	}
	names := make([]string, 0, len(p.all))
	for _, e := range p.all {
		names = append(names, e.id.Name)
	}
	sort.Strings(names)
	return fmt.Sprintf("%d identities: %s", len(names), strings.Join(names, ", "))
}
