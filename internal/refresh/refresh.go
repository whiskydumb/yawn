package refresh

import (
	"context"
	"fmt"
	"strings"

	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/source"
)

// Result is what refreshing one source produced.
type Result struct {
	Source queue.Source
	Seen   int
	Added  int
	Err    error
}

type Refresher struct {
	Queue     *queue.Store
	Extractor extract.Extractor

	// Limit caps how many videos are taken from one source; zero means all.
	Limit int
}

// listPrefix marks a source that is a local file rather than a URL.
const listPrefix = "file://"

// Source re-expands one source and records anything new.
//
// A source pointing at a single video is skipped: it can never grow, and
// asking youtube about it again would spend a request to learn nothing.
func (r *Refresher) Source(ctx context.Context, src queue.Source) Result {
	res := Result{Source: src}

	if src.Kind == queue.SourceVideo {
		return res
	}
	if strings.HasPrefix(src.URL, listPrefix) {
		res.Added, res.Seen, res.Err = r.refreshList(ctx, src)
	} else {
		res.Added, res.Seen, res.Err = r.refreshRemote(ctx, src)
	}
	if res.Err == nil {
		if err := r.Queue.MarkSourceSynced(ctx, src.ID); err != nil {
			res.Err = err
		}
	}
	return res
}

func (r *Refresher) refreshRemote(ctx context.Context, src queue.Source) (added, seen int, err error) {
	listing, err := r.Extractor.Resolve(ctx, src.URL)
	if err != nil {
		return 0, 0, err
	}
	entries := listing.Entries
	if r.Limit > 0 && len(entries) > r.Limit {
		entries = entries[:r.Limit]
	}

	items := make([]queue.NewItem, 0, len(entries))
	for _, e := range entries {
		items = append(items, queue.NewItem{
			VideoID:    e.ID,
			URL:        e.URL,
			Title:      e.Title,
			Uploader:   e.Uploader,
			UploadDate: e.UploadDate,
			Duration:   e.Duration,
		})
	}
	added, err = r.Queue.AddItems(ctx, src.ID, items)
	return added, len(items), err
}

// refreshList re-reads a link list, which may have grown since it was added.
func (r *Refresher) refreshList(ctx context.Context, src queue.Source) (added, seen int, err error) {
	path := strings.TrimPrefix(src.URL, listPrefix)
	urls, err := source.ReadList(path)
	if err != nil {
		return 0, 0, err
	}
	if r.Limit > 0 && len(urls) > r.Limit {
		urls = urls[:r.Limit]
	}

	items := make([]queue.NewItem, 0, len(urls))
	for _, u := range urls {
		id := source.ExtractID(u)
		if id == "" {
			continue
		}
		items = append(items, queue.NewItem{VideoID: id, URL: u})
	}
	added, err = r.Queue.AddItems(ctx, src.ID, items)
	return added, len(items), err
}

// All refreshes every source, reporting each one as it finishes.
//
// One source failing does not stop the rest: a channel that was deleted must
// not keep the others from catching up.
func (r *Refresher) All(ctx context.Context, onResult func(Result)) ([]Result, error) {
	sources, err := r.Queue.Sources(ctx)
	if err != nil {
		return nil, err
	}

	var out []Result
	for _, src := range sources {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if src.Kind == queue.SourceVideo {
			continue
		}
		res := r.Source(ctx, src)
		out = append(out, res)
		if onResult != nil {
			onResult(res)
		}
	}
	return out, nil
}

// Totals sums up a batch of results.
func Totals(results []Result) (added, seen, failed int) {
	for _, r := range results {
		if r.Err != nil {
			failed++
			continue
		}
		added += r.Added
		seen += r.Seen
	}
	return added, seen, failed
}

// Label is how a source should be named in output.
func Label(src queue.Source) string {
	if src.Title != "" {
		return src.Title
	}
	return strings.TrimPrefix(src.URL, listPrefix)
}

// AddSource registers a new source and expands it for the first time.
func (r *Refresher) AddSource(ctx context.Context, in source.Input) (Result, error) {
	url := in.URL
	title := ""
	kind := in.Kind

	if in.FromList != "" {
		// source.Parse labels every line of a list as a video, but the source
		// itself is the file: it can grow, and a refresh has to re-read it
		kind = queue.SourceList
		url = listPrefix + in.FromList
		title = in.FromList
	} else if in.Kind != queue.SourceVideo {
		listing, err := r.Extractor.Resolve(ctx, url)
		if err != nil {
			return Result{}, err
		}
		title = listing.Title
		if title == "" {
			title = listing.Uploader
		}
	}

	id, err := r.Queue.AddSource(ctx, kind, url, title)
	if err != nil {
		return Result{}, err
	}
	src := queue.Source{ID: id, Kind: kind, URL: url, Title: title}

	// a single video has no listing to expand, so it goes in directly
	if kind == queue.SourceVideo {
		vid := source.ExtractID(url)
		if vid == "" {
			return Result{}, fmt.Errorf("cannot tell the video id from %s", url)
		}
		added, err := r.Queue.AddItems(ctx, id, []queue.NewItem{{VideoID: vid, URL: url}})
		return Result{Source: src, Seen: 1, Added: added}, err
	}
	return r.Source(ctx, src), nil
}
