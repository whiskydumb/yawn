package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/identity"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/ratelimit"
	"github.com/whiskydumb/yawn/internal/store"
	"golang.org/x/sync/errgroup"
)

const (
	// leaseTTL is how long a lease lasts; workers renew twice as often.
	leaseTTL = 2 * time.Minute

	idlePoll  = 2 * time.Second
	drainPoll = 3 * time.Second

	// freeSpacePoll is how often free space is rechecked while it is short.
	freeSpacePoll = 15 * time.Second

	// defaultEstimate is what a video is assumed to weigh until it is known.
	defaultEstimate = 400 << 20

	// stallThreshold is how many failures in a row point at a dead network or
	// a ban rather than at a bad video.
	stallThreshold = 6
)

type Options struct {
	Config    config.Config
	Queue     *queue.Store
	Extractor extract.Extractor
	Prober    *media.Prober
	Converter *media.Converter
	Stores    []store.Uploader
	Bus       *events.Bus
	Logger    *slog.Logger

	// Exclusive means the caller has proven no other process is using this
	// database, so every claimed task is crash leftovers and can be reclaimed
	// at once rather than waiting out a lease.
	Exclusive bool
}

type Pipeline struct {
	cfg        config.Config
	q          *queue.Store
	ex         extract.Extractor
	prober     *media.Prober
	converter  *media.Converter
	convert    media.Options
	stores     map[string]store.Uploader
	layout     map[string]string
	bus        *events.Bus
	log        *slog.Logger
	budget     *Budget
	format     string
	exclusive  bool
	identities *identity.Pool
	uploadRate *ratelimit.Limiter

	// consecutiveFails counts failures across all workers. A run of them
	// means something outside is broken, not this particular video.
	consecutiveFails atomic.Int64

	// blockedRetries counts how often each item was handed back because of
	// bot protection. Those retries are free, so they need their own bound.
	blockedMu      sync.Mutex
	blockedRetries map[int64]int
}

// maxBlockedRetries bounds the free retries one item gets while the server is
// rate limiting us. The counter lives in memory on purpose: a restart is a
// fresh chance, which is exactly right after waiting out a block.
const maxBlockedRetries = 3

func New(opts Options) (*Pipeline, error) {
	if opts.Queue == nil || opts.Extractor == nil {
		return nil, errors.New("the pipeline needs a queue and an extractor")
	}
	format, err := opts.Config.Download.FormatSelector()
	if err != nil {
		return nil, err
	}
	log := opts.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	}
	bus := opts.Bus
	if bus == nil {
		bus = events.NewBus(256)
	}

	p := &Pipeline{
		cfg:            opts.Config,
		q:              opts.Queue,
		ex:             opts.Extractor,
		prober:         opts.Prober,
		converter:      opts.Converter,
		stores:         make(map[string]store.Uploader, len(opts.Stores)),
		layout:         make(map[string]string, len(opts.Stores)),
		bus:            bus,
		log:            log,
		budget:         NewBudget(opts.Config.Download.MaxDisk.Bytes()),
		format:         format,
		exclusive:      opts.Exclusive,
		blockedRetries: make(map[int64]int),
		// one limiter for every upload worker: the ceiling is on the tool
		uploadRate: ratelimit.New(opts.Config.Upload.RateLimit.Bytes()),
	}
	for _, s := range opts.Stores {
		p.stores[s.Name()] = s
	}
	for _, sc := range opts.Config.Stores {
		p.layout[sc.Name] = sc.Layout
	}
	pool, err := buildIdentityPool(opts.Config)
	if err != nil {
		return nil, err
	}
	p.identities = pool

	if c := opts.Config.Convert; c.Enabled {
		p.convert = media.Options{
			Mode:       media.Mode(c.Mode),
			Container:  c.Container,
			VideoCodec: c.VideoCodec,
			AudioCodec: c.AudioCodec,
			CRF:        c.CRF,
			Preset:     c.Preset,
			Bitrate:    c.Bitrate,
			ExtraArgs:  c.ExtraArgs,
		}
		// a bad conversion setup must surface now, not after the first
		// download has already cost a few minutes
		if err := p.convert.Validate(); err != nil {
			return nil, fmt.Errorf("convert: %w", err)
		}
	}
	return p, nil
}

// buildIdentityPool turns the config into a rotation, validating every entry
// so a typo in a proxy surfaces now rather than on the first download.
func buildIdentityPool(cfg config.Config) (*identity.Pool, error) {
	configured := cfg.Identity.Identities()
	ids := make([]identity.Identity, 0, len(configured))
	for _, c := range configured {
		ids = append(ids, identity.Identity{
			Name:               c.Name,
			CookiesFile:        c.CookiesFile,
			CookiesFromBrowser: c.CookiesFromBrowser,
			Proxy:              c.Proxy,
			UserAgent:          c.UserAgent,
		})
	}
	opts := identity.DefaultOptions()
	if d := cfg.Identity.BaseBackoff.Duration(); d > 0 {
		opts.BaseBackoff = d
	}
	if d := cfg.Identity.MaxBackoff.Duration(); d > 0 {
		opts.MaxBackoff = d
	}
	if cfg.Identity.MaxStrikes > 0 {
		opts.MaxStrikes = cfg.Identity.MaxStrikes
	}
	pool, err := identity.NewPool(ids, opts)
	if err != nil {
		return nil, fmt.Errorf("identity: %w", err)
	}
	return pool, nil
}

// UploadRate reports the upload ceiling in bytes per second, or zero.
func (p *Pipeline) UploadRate() int64 { return p.uploadRate.Limit() }

// throttle returns the upload limiter as an interface value.
//
// The nil check is not decoration: putting a nil *Limiter into an interface
// makes a non-nil interface holding a nil pointer, so a plain assignment
// would leave every request looking throttled and only work because the
// method tolerates a nil receiver.
func (p *Pipeline) throttle() store.Throttle {
	if p.uploadRate == nil {
		return nil
	}
	return p.uploadRate
}

// Identities exposes the pool for the run header and the dashboard.
func (p *Pipeline) Identities() *identity.Pool { return p.identities }

// converting reports whether the conversion pass is on.
func (p *Pipeline) converting() bool {
	return p.converter != nil && p.convert.Mode != ""
}

func (p *Pipeline) StoreNames() []string {
	out := make([]string, 0, len(p.stores))
	for name := range p.stores {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Run works the queue until it is empty or the context is cancelled.
// Cancelling is a graceful shutdown: workers finish the current step, hand
// their leases back and exit, and the next run picks up where this one left off.
func (p *Pipeline) Run(ctx context.Context) error {
	if err := p.prepare(ctx); err != nil {
		return err
	}

	// an own context so the supervisor can stop the workers once the work is
	// gone without touching the caller's context
	runCtx, stop := context.WithCancel(ctx)
	defer stop()

	g, gctx := errgroup.WithContext(runCtx)

	for i := 0; i < p.cfg.Download.Workers; i++ {
		name := fmt.Sprintf("dl-%d", i+1)
		g.Go(func() error { return p.downloadWorker(gctx, name) })
	}
	if len(p.stores) > 0 {
		for i := 0; i < p.cfg.Upload.Workers; i++ {
			name := fmt.Sprintf("up-%d", i+1)
			g.Go(func() error { return p.uploadWorker(gctx, name) })
		}
	}
	g.Go(func() error {
		p.watchDrain(gctx, stop)
		return nil
	})

	err := g.Wait()
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func (p *Pipeline) prepare(ctx context.Context) error {
	if err := p.ex.Available(ctx); err != nil {
		return err
	}
	if p.prober != nil && p.cfg.Upload.Verify {
		if err := p.prober.Available(ctx); err != nil {
			return err
		}
	}
	if p.converting() {
		if err := p.converter.Available(ctx); err != nil {
			return err
		}
		if err := p.converter.SupportsEncoders(ctx, p.convert.VideoCodec, p.convert.AudioCodec); err != nil {
			return err
		}
	}
	for _, s := range p.stores {
		if err := s.Available(ctx); err != nil {
			return fmt.Errorf("store %s is unavailable: %w", s.Name(), err)
		}
	}

	reclaim := p.q.ReclaimExpired
	if p.exclusive {
		reclaim = p.q.ReclaimAll
	}
	reclaimed, err := reclaim(ctx)
	if err != nil {
		return err
	}
	if reclaimed > 0 {
		p.log.Info("reclaimed abandoned tasks", "count", reclaimed)
		p.publish(events.Event{
			Kind:    events.KindResumed,
			Message: fmt.Sprintf("reclaimed %d tasks from the previous run", reclaimed),
		})
	}

	names := p.StoreNames()
	if _, err := p.q.PruneTargets(ctx, names); err != nil {
		return err
	}
	if _, err := p.q.EnsureTargets(ctx, names); err != nil {
		return err
	}

	// whatever is already on disk has to hold budget, or the pipeline will
	// think it has twice the room it really does
	onDisk, err := p.q.ItemsOnDisk(ctx)
	if err != nil {
		return err
	}
	for _, it := range onDisk {
		size := it.LocalBytes
		if size <= 0 {
			size = defaultEstimate
		}
		p.budget.Charge(it.ID, size)
	}
	if len(onDisk) > 0 {
		p.log.Info("left on disk by the previous run", "items", len(onDisk), "bytes", p.budget.Used())
	}
	return nil
}

// watchDrain stops the workers once there is nothing left to do.
func (p *Pipeline) watchDrain(ctx context.Context, stop context.CancelFunc) {
	t := time.NewTicker(drainPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			busy, err := p.q.HasWork(ctx, p.maxAttempts())
			if err != nil {
				p.log.Warn("cannot check for remaining work", "err", err)
				continue
			}
			if !busy {
				p.log.Info("queue is empty, stopping")
				stop()
				return
			}
		}
	}
}

func (p *Pipeline) maxAttempts() int {
	n := p.cfg.Download.Retries
	if p.cfg.Upload.Retries > n {
		n = p.cfg.Upload.Retries
	}
	if n < 1 {
		n = 1
	}
	return n
}

func (p *Pipeline) publish(e events.Event) {
	p.bus.Publish(e)
}

// backoffAfterFailure waits longer the more failures pile up in a row.
//
// The counter is shared on purpose. If the network is down there is no point
// grinding through the queue at full speed; that only burns the retry budget
// of every video at once.
// blocked errors trip it at once: a 429 is the server saying slow down, and
// there is nothing to gain by finding that out five more times in a row.
func (p *Pipeline) backoffAfterFailure(ctx context.Context, blocked bool) {
	n := p.consecutiveFails.Add(1)
	threshold := int64(stallThreshold)
	if blocked {
		threshold = 1
	}
	if n < threshold {
		return
	}
	wait := time.Duration(n-threshold+1) * 10 * time.Second
	if wait > 5*time.Minute {
		wait = 5 * time.Minute
	}
	p.publish(events.Event{
		Kind:    events.KindStalled,
		Message: fmt.Sprintf("%d failures in a row, pausing for %s, this looks like a dead network or a ban", n, wait),
	})
	p.log.Warn("consecutive failures, pausing", "count", n, "wait", wait)
	select {
	case <-ctx.Done():
	case <-time.After(wait):
	}
}

func (p *Pipeline) noteSuccess() {
	if p.consecutiveFails.Swap(0) >= stallThreshold {
		p.publish(events.Event{Kind: events.KindResumed, Message: "connectivity is back"})
	}
}

// itemDir is one directory per video rather than a shared dump. Sidecars are
// then collected without guessing names, and deleting after upload cannot
// touch someone else's file.
func (p *Pipeline) itemDir(videoID string) string {
	return filepath.Join(p.cfg.WorkDir, videoID)
}

// assetBase is the shared filename prefix of a video and its sidecars:
// "title [id].mp4" gives "title [id]", which "title [id].info.json" and
// "title [id].ru.vtt" also start with.
func assetBase(mediaPath string) string {
	name := filepath.Base(mediaPath)
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// collectAssets gathers one video and its sidecars.
//
// Files are matched by base name rather than by directory. yawn's own layout
// puts one video per directory, but an imported archive may keep dozens side
// by side, and taking the whole directory there would upload and then delete
// somebody else's videos.
func collectAssets(dir, base string) ([]store.Asset, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	prefix := base + "."
	var out []store.Asset
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if base != "" && !strings.HasPrefix(name, prefix) {
			continue
		}
		// unfinished pieces do not travel to the archive. the convert marker
		// sits before the extension, so it is matched anywhere in the name
		if strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".ytdl") ||
			strings.HasSuffix(name, ".yawn-part") || strings.Contains(name, ".yawn-convert") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, store.Asset{
			Path: filepath.Join(dir, name),
			Rel:  name,
			Size: info.Size(),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Rel < out[j].Rel })
	if len(out) == 0 {
		return nil, fmt.Errorf("found no finished files for %q in %s", base, dir)
	}
	return out, nil
}

var mediaExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true, ".m4a": true,
	".mp3": true, ".opus": true, ".flv": true, ".mov": true,
}

// findMedia looks for a finished media file, returning "" when there is none.
func findMedia(dir string) (string, int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", 0
	}
	var best string
	var bestSize int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !mediaExtensions[strings.ToLower(filepath.Ext(e.Name()))] {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.Size() > bestSize {
			best, bestSize = filepath.Join(dir, e.Name()), info.Size()
		}
	}
	return best, bestSize
}

// DiskUsage reports how much of the disk budget is currently held.
func (p *Pipeline) DiskUsage() (used, capacity int64) {
	return p.budget.Used(), p.budget.Capacity()
}
