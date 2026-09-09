package pipeline

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/events"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/store"
)

func (p *Pipeline) uploadWorker(ctx context.Context, worker string) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		target, err := p.q.ClaimUpload(ctx, worker, leaseTTL, p.cfg.Upload.Retries)
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

		if err := p.runUpload(ctx, worker, target); err != nil {
			if errors.Is(err, context.Canceled) {
				p.releaseUploadOnShutdown(target)
				return nil
			}
			p.handleUploadFailure(ctx, target, err)
			p.backoffAfterFailure(ctx, false)
			continue
		}
		p.noteSuccess()
	}
}

func (p *Pipeline) releaseUploadOnShutdown(t *queue.Target) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if err := p.q.ReleaseUpload(ctx, t.ID); err != nil {
		p.log.Warn("cannot hand the upload back", "video", t.VideoID, "store", t.Store, "err", err)
	}
}

func (p *Pipeline) handleUploadFailure(ctx context.Context, t *queue.Target, cause error) {
	permanent := errors.Is(cause, store.ErrPermanent)
	if err := p.q.MarkTargetFailed(context.WithoutCancel(ctx), t.ID, cause, permanent, p.cfg.Upload.Retries); err != nil {
		p.log.Error("cannot record the upload failure", "video", t.VideoID, "err", err)
	}
	p.log.Warn("upload failed",
		"video", t.VideoID, "store", t.Store, "permanent", permanent, "err", cause)
	p.publish(events.Event{
		Kind: events.KindFailed, ItemID: t.ItemID, VideoID: t.VideoID,
		Title: t.Item.Title, Store: t.Store, Message: cause.Error(),
	})
}

func (p *Pipeline) runUpload(ctx context.Context, worker string, t *queue.Target) error {
	dst, ok := p.stores[t.Store]
	if !ok {
		return fmt.Errorf("%w: store %q vanished from the config", store.ErrPermanent, t.Store)
	}
	dir := filepath.Dir(t.LocalPath)
	prefix := p.prefixFor(t)

	stopLease := p.keepTargetLease(ctx, t.ID, worker)
	defer stopLease()

	p.publish(events.Event{
		Kind: events.KindUploadStart, ItemID: t.ItemID, VideoID: t.VideoID,
		Title: t.Item.Title, Worker: worker, Store: t.Store,
	})

	assets, err := collectAssets(dir, assetBase(t.LocalPath))
	if err != nil {
		return fmt.Errorf("%w: %v", store.ErrPermanent, err)
	}
	req := store.PutRequest{Prefix: prefix, Assets: assets, Throttle: p.throttle()}

	// a run interrupted mid-upload must not send everything again; matching
	// weight is enough of a check for a restart
	if ok, size, err := dst.Has(ctx, prefix); err == nil && ok && size >= req.TotalBytes() {
		p.log.Info("the set is already in the store, skipping", "video", t.VideoID, "store", t.Store)
		return p.finishTarget(ctx, t, prefix, size)
	}

	res, err := dst.Put(ctx, req, func(pr store.Progress) {
		p.publish(events.Event{
			Kind: events.KindProgress, ItemID: t.ItemID, VideoID: t.VideoID,
			Title: t.Item.Title, Worker: worker, Store: t.Store,
			Done: pr.Uploaded, Total: pr.Total,
		})
	})
	if err != nil {
		return err
	}

	p.log.Info("uploaded", "video", t.VideoID, "store", t.Store, "bytes", res.Bytes)
	return p.finishTarget(ctx, t, res.Key, res.Bytes)
}

// finishTarget closes a target and, when it was the last one, removes the
// local copy and returns its disk budget.
func (p *Pipeline) finishTarget(ctx context.Context, t *queue.Target, key string, bytes int64) error {
	dbCtx := context.WithoutCancel(ctx)
	allDone, err := p.q.MarkTargetDone(dbCtx, t.ID, key, bytes)
	if err != nil {
		return err
	}
	p.publish(events.Event{
		Kind: events.KindUploadDone, ItemID: t.ItemID, VideoID: t.VideoID,
		Title: t.Item.Title, Store: t.Store, Done: bytes, Total: bytes,
	})
	if !allDone {
		return nil
	}

	if p.cfg.Upload.DeleteAfter {
		if err := p.removeLocal(t.LocalPath); err != nil {
			// not fatal: no space was freed, but the archive already has the files
			p.log.Warn("cannot remove the local files", "video", t.VideoID, "err", err)
		} else if err := p.q.ClearLocal(dbCtx, t.ItemID); err != nil {
			p.log.Warn("cannot forget the local file", "video", t.VideoID, "err", err)
		}
	}
	p.budget.Release(t.ItemID)

	p.publish(events.Event{
		Kind: events.KindItemDone, ItemID: t.ItemID, VideoID: t.VideoID, Title: t.Item.Title,
	})
	return nil
}

// removeLocal deletes exactly the files that belong to one video.
//
// The directory itself goes only if it ends up empty, so importing an archive
// where many videos share a folder can never take the neighbours with it.
func (p *Pipeline) removeLocal(mediaPath string) error {
	dir := filepath.Dir(mediaPath)
	assets, err := collectAssets(dir, assetBase(mediaPath))
	if err != nil {
		return err
	}
	for _, a := range assets {
		if err := os.Remove(a.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	// leftovers such as an abandoned .part belong to this video too
	if err := removeStragglers(dir, assetBase(mediaPath)); err != nil {
		return err
	}
	// fails while anything is left, which is exactly what we want
	if err := os.Remove(dir); err != nil && !os.IsNotExist(err) {
		p.log.Debug("directory kept, it still holds other files", "dir", dir)
	}
	return nil
}

// removeStragglers clears partial files left next to a finished video.
func removeStragglers(dir, base string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	prefix := base + "."
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (p *Pipeline) prefixFor(t *queue.Target) string {
	layout := p.layout[t.Store]
	if layout == "" {
		layout = config.DefaultLayout
	}
	return store.RenderLayout(layout, store.Vars{
		ID:         t.Item.VideoID,
		Title:      t.Item.Title,
		Uploader:   t.Item.Uploader,
		UploadDate: t.Item.UploadDate,
	})
}

func (p *Pipeline) keepTargetLease(ctx context.Context, targetID int64, owner string) func() {
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(leaseTTL / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := p.q.RenewTargetLease(ctx, targetID, owner, leaseTTL); err != nil {
					p.log.Warn("cannot renew the upload lease", "target", targetID, "err", err)
				}
			}
		}
	}()
	return cancel
}
