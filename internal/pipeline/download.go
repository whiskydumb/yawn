package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/diskspace"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/identity"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/queue"
)

func (p *Pipeline) downloadWorker(ctx context.Context, worker string) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		item, err := p.q.ClaimDownload(ctx, worker, leaseTTL, p.cfg.Download.Retries)
		switch {
		case errors.Is(err, queue.ErrNoWork):
			if !sleepCtx(ctx, idlePoll) {
				return nil
			}
			continue
		case errors.Is(err, context.Canceled):
			return nil
		case err != nil:
			return fmt.Errorf("worker %s: %w", worker, err)
		}

		if err := p.runDownload(ctx, worker, item); err != nil {
			if errors.Is(err, context.Canceled) {
				p.releaseOnShutdown(item)
				return nil
			}
			p.handleDownloadFailure(ctx, item, err)
			p.backoffAfterFailure(ctx, errors.Is(err, extract.ErrBlocked))
			continue
		}
		p.noteSuccess()
	}
}

// chargeBlockedRetry reports whether this item may be requeued for free after
// being blocked, counting the free retries it has already had.
func (p *Pipeline) chargeBlockedRetry(itemID int64) bool {
	p.blockedMu.Lock()
	defer p.blockedMu.Unlock()
	if p.blockedRetries[itemID] >= maxBlockedRetries {
		return false
	}
	p.blockedRetries[itemID]++
	return true
}

// releaseOnShutdown hands a task back. The context is already cancelled, so
// the database write uses one detached from it.
func (p *Pipeline) releaseOnShutdown(item *queue.Item) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if err := p.q.ReleaseDownload(ctx, item.ID); err != nil {
		p.log.Warn("cannot hand the task back", "video", item.VideoID, "err", err)
	}
	p.budget.Release(item.ID)
}

func (p *Pipeline) handleDownloadFailure(ctx context.Context, item *queue.Item, cause error) {
	permanent := errors.Is(cause, extract.ErrUnavailable) ||
		errors.Is(cause, media.ErrCorrupt) ||
		errors.Is(cause, media.ErrConvert)

	dbCtx := context.WithoutCancel(ctx)

	// Being rate limited is not this video's fault, so it must not cost an
	// attempt: five refusals arriving within a minute would otherwise fail a
	// perfectly good video for good.
	if errors.Is(cause, extract.ErrBlocked) && p.chargeBlockedRetry(item.ID) {
		if err := p.q.ReleaseDownload(dbCtx, item.ID); err != nil {
			p.log.Warn("cannot hand the blocked task back", "video", item.VideoID, "err", err)
		}
		p.log.Warn("blocked, requeued without spending an attempt",
			"video", item.VideoID, "err", cause)
		p.publish(events.Event{
			Kind: events.KindStalled, ItemID: item.ID, VideoID: item.VideoID,
			Title: item.Title, Message: "blocked, will retry: " + cause.Error(),
		})
		return
	}
	if permanent {
		// nothing to retry, so free both the space and the budget
		_ = os.RemoveAll(p.itemDir(item.VideoID))
		p.budget.Release(item.ID)
	}
	if err := p.q.MarkFailed(dbCtx, item.ID, cause, permanent, p.cfg.Download.Retries); err != nil {
		p.log.Error("cannot record the failure", "video", item.VideoID, "err", err)
	}
	p.log.Warn("download failed",
		"video", item.VideoID, "title", item.Title, "permanent", permanent, "err", cause)
	p.publish(events.Event{
		Kind: events.KindFailed, ItemID: item.ID, VideoID: item.VideoID,
		Title: item.Title, Message: cause.Error(),
	})
}

func (p *Pipeline) runDownload(ctx context.Context, worker string, item *queue.Item) error {
	estimate := item.ExpectedBytes
	if estimate <= 0 {
		estimate = defaultEstimate
	}
	if err := p.budget.Acquire(ctx, item.ID, estimate); err != nil {
		return err
	}
	if err := p.waitForFreeSpace(ctx, item); err != nil {
		return err
	}

	stopLease := p.keepLease(ctx, item.ID, worker)
	defer stopLease()

	// the identity is taken per video, not per worker: a worker that just got
	// turned away should come back as somebody else
	who, err := p.identities.Acquire(ctx)
	if err != nil {
		return err
	}

	p.publish(events.Event{
		Kind: events.KindDownloadStart, ItemID: item.ID, VideoID: item.VideoID,
		Title: item.Title, Worker: worker,
	})
	p.log.Info("downloading", "worker", worker, "video", item.VideoID,
		"title", item.Title, "identity", who.Identity.Name)

	dir := p.itemDir(item.VideoID)
	result, err := p.fetch(ctx, item, dir, worker, who.Identity)
	p.releaseIdentity(who, err)
	if err != nil {
		return err
	}

	dbCtx := context.WithoutCancel(ctx)
	// metadata is filled in before the check, not after: a video from a link
	// list arrives without a title or duration, and skipping this would send
	// the layout to "unknown" and leave the duration check disabled
	p.backfill(dbCtx, item, result)

	// the file is checked before conversion, not after: feeding ffmpeg a
	// truncated download would produce a valid looking short file and hide
	// the original failure
	if p.prober != nil && p.cfg.Upload.Verify {
		if _, err := p.prober.Verify(ctx, result.Path, item.Duration); err != nil {
			return err
		}
	}

	if p.converting() {
		if err := p.runConvert(ctx, item, result, worker); err != nil {
			return err
		}
	}

	p.budget.Adjust(item.ID, result.Bytes)

	if err := p.q.MarkDownloaded(dbCtx, item.ID, result.Path, result.Bytes, result.FormatID); err != nil {
		return err
	}
	if err := p.q.EnsureTargetsFor(dbCtx, item.ID, p.StoreNames()); err != nil {
		return err
	}

	p.publish(events.Event{
		Kind: events.KindDownloadDone, ItemID: item.ID, VideoID: item.VideoID,
		Title: item.Title, Worker: worker, Done: result.Bytes, Total: result.Bytes,
	})
	p.log.Info("downloaded", "video", item.VideoID, "bytes", result.Bytes, "format", result.FormatID)

	// with no stores configured the pipeline is a plain downloader
	if len(p.stores) == 0 {
		if err := p.q.MarkTargetlessDone(dbCtx, item.ID); err != nil {
			return err
		}
		p.budget.Release(item.ID)
		p.publish(events.Event{
			Kind: events.KindItemDone, ItemID: item.ID, VideoID: item.VideoID, Title: item.Title,
		})
	}
	return nil
}

// releaseIdentity hands the identity back with a verdict on how it did.
func (p *Pipeline) releaseIdentity(who *identity.Lease, err error) {
	switch {
	case err == nil:
		p.identities.Release(who, identity.Success)
	case errors.Is(err, extract.ErrBlocked):
		p.identities.Release(who, identity.Blocked)
		p.log.Warn("identity refused, resting it",
			"identity", who.Identity.Name, "proxy", who.Identity.Proxy)
	default:
		// a removed video says nothing about the credentials
		p.identities.Release(who, identity.Failed)
	}
}

// runConvert rewrites the downloaded file in place and points the result at
// the new one.
func (p *Pipeline) runConvert(ctx context.Context, item *queue.Item, result *extract.Result, worker string) error {
	p.log.Info("converting", "video", item.VideoID, "mode", p.convert.Mode)

	converted, err := p.converter.Convert(ctx, result.Path, p.convert, float64(item.Duration),
		func(pr media.Progress) {
			p.publish(events.Event{
				Kind: events.KindProgress, ItemID: item.ID, VideoID: item.VideoID,
				Title: item.Title, Worker: worker, Store: "ffmpeg",
				Done: int64(pr.Seconds), Total: int64(pr.Duration),
			})
		})
	if err != nil {
		return err
	}

	result.Path = converted.Path
	result.Bytes = converted.Bytes
	p.log.Info("converted", "video", item.VideoID, "bytes", converted.Bytes)
	return nil
}

// waitForFreeSpace holds downloads back while the volume is below min_free.
//
// This is not a duplicate of the budget. The budget only counts yawn's own
// files, while the disk is shared with every other program: without this the
// tool would honour its own ceiling and still fill the drive.
func (p *Pipeline) waitForFreeSpace(ctx context.Context, item *queue.Item) error {
	need := p.cfg.Download.MinFree.Bytes()
	if need <= 0 {
		return nil
	}
	warned := false
	for {
		avail, err := diskspace.Free(p.cfg.WorkDir)
		if err != nil {
			// if the system will not answer, that is no reason to stall
			p.log.Warn("cannot read free space", "dir", p.cfg.WorkDir, "err", err)
			return nil
		}
		if avail >= need {
			if warned {
				p.publish(events.Event{Kind: events.KindResumed, Message: "space freed up, carrying on"})
			}
			return nil
		}
		if !warned {
			warned = true
			msg := fmt.Sprintf("%s free on disk against a %s threshold, waiting for an upload",
				config.Size(avail), p.cfg.Download.MinFree)
			p.log.Warn("low disk space, downloads paused",
				"free", avail, "min_free", need, "video", item.VideoID)
			p.publish(events.Event{Kind: events.KindStalled, Message: msg})
		}
		if !sleepCtx(ctx, freeSpacePoll) {
			return ctx.Err()
		}
	}
}

// backfill copies what became known after the download into the database and
// into the in-memory item, which the layout and the check both use.
func (p *Pipeline) backfill(ctx context.Context, item *queue.Item, res *extract.Result) {
	if res.Title == "" && res.UploadDate == "" && res.Duration == 0 {
		return
	}
	if err := p.q.UpdateMetadata(ctx, item.ID, res.Title, res.Uploader, res.UploadDate, res.Duration); err != nil {
		p.log.Warn("cannot store metadata", "video", item.VideoID, "err", err)
		return
	}
	if item.Title == "" {
		item.Title = res.Title
	}
	if item.Uploader == "" {
		item.Uploader = res.Uploader
	}
	if item.UploadDate == "" {
		item.UploadDate = res.UploadDate
	}
	if item.Duration == 0 {
		item.Duration = res.Duration
	}
}

// fetch downloads the video, reusing a finished file if one is already there.
func (p *Pipeline) fetch(ctx context.Context, item *queue.Item, dir, worker string, who identity.Identity) (*extract.Result, error) {
	// a previous run may have finished the file and died before writing the
	// database; refetching it costs minutes and one more request to youtube
	if path, size := findMedia(dir); path != "" && size > 0 {
		if p.prober == nil || !p.cfg.Upload.Verify {
			return &extract.Result{Path: path, Bytes: size, FormatID: item.FormatID}, nil
		}
		if _, err := p.prober.Verify(ctx, path, item.Duration); err == nil {
			p.log.Info("file already on disk, skipping the download", "video", item.VideoID)
			return &extract.Result{Path: path, Bytes: size, FormatID: item.FormatID}, nil
		}
		// a broken stub gets in the way of resuming, so drop it and refetch
		_ = os.Remove(path)
	}

	req := p.buildRequest(item, dir, who)
	last := time.Now()
	return p.ex.Download(ctx, req, func(pr extract.Progress) {
		if pr.Stage != "download" {
			return
		}
		// progress reaches the database rarely; it has no use for a hundred
		// writes a second
		if time.Since(last) > 5*time.Second {
			last = time.Now()
			_ = p.q.UpdateProgress(context.WithoutCancel(ctx), item.ID, pr.Downloaded, pr.Total)
		}
		p.publish(events.Event{
			Kind: events.KindProgress, ItemID: item.ID, VideoID: item.VideoID,
			Title: item.Title, Worker: worker,
			Done: pr.Downloaded, Total: pr.Total, Speed: pr.Speed, ETA: pr.ETA,
		})
	})
}

func (p *Pipeline) buildRequest(item *queue.Item, dir string, who identity.Identity) extract.Request {
	d := p.cfg.Download
	return extract.Request{
		URL:       item.URL,
		OutputDir: dir,
		Template:  "%(title)s [%(id)s].%(ext)s",
		Format:    p.format,
		Container: d.Container,

		Fragments:    d.Fragments,
		Retries:      d.Retries,
		SleepBetween: time.Duration(d.SleepRequests * float64(time.Second)),
		RateLimit:    p.perWorkerRate(),

		Subtitles:     d.Subtitles.Enabled,
		SubtitleAuto:  d.Subtitles.Auto,
		SubtitleLangs: d.Subtitles.Langs,

		WriteInfoJSON:    d.Metadata.InfoJSON,
		WriteDescription: d.Metadata.Description,
		WriteThumbnail:   d.Metadata.Thumbnail,
		EmbedMetadata:    d.Metadata.Embed,

		CookiesFile:        who.CookiesFile,
		CookiesFromBrowser: who.CookiesFromBrowser,
		Proxy:              who.Proxy,
		UserAgent:          who.UserAgent,
	}
}

// perWorkerRate splits the overall speed ceiling between workers. The user
// caps the tool, not each worker: handing every worker the full limit would
// use three times the promised bandwidth on three workers.
func (p *Pipeline) perWorkerRate() int64 {
	limit := p.cfg.Download.RateLimit.Bytes()
	if limit <= 0 {
		return 0
	}
	workers := int64(p.cfg.Download.Workers)
	if workers < 1 {
		workers = 1
	}
	per := limit / workers
	if per < 1 {
		per = 1
	}
	return per
}

// keepLease renews the lease for as long as a slow operation runs, returning
// the stop function the caller must call.
func (p *Pipeline) keepLease(ctx context.Context, itemID int64, owner string) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(leaseTTL / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if err := p.q.RenewLease(ctx, itemID, owner, leaseTTL); err != nil {
					p.log.Warn("cannot renew the lease", "item", itemID, "err", err)
				}
			}
		}
	}()
	return cancel
}

// sleepCtx waits, returning false when the wait was cut short by a cancel.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
