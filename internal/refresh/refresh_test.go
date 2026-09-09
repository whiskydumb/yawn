package refresh

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/queue"
	"github.com/whiskydumb/yawn/internal/source"
)

// fakeExtractor answers with whatever the test put in it and counts calls.
type fakeExtractor struct {
	listings map[string]*extract.Listing
	errs     map[string]error
	calls    map[string]int
}

func newFake() *fakeExtractor {
	return &fakeExtractor{
		listings: map[string]*extract.Listing{},
		errs:     map[string]error{},
		calls:    map[string]int{},
	}
}

func (f *fakeExtractor) Name() string                    { return "fake" }
func (f *fakeExtractor) Available(context.Context) error { return nil }
func (f *fakeExtractor) Resolve(_ context.Context, url string) (*extract.Listing, error) {
	f.calls[url]++
	if err, ok := f.errs[url]; ok {
		return nil, err
	}
	if l, ok := f.listings[url]; ok {
		return l, nil
	}
	return nil, errors.New("no such listing")
}
func (f *fakeExtractor) Info(context.Context, string) (*extract.Info, error) { return nil, nil }
func (f *fakeExtractor) Download(context.Context, extract.Request, extract.ProgressFunc) (*extract.Result, error) {
	return nil, nil
}

func listing(ids ...string) *extract.Listing {
	l := &extract.Listing{Title: "a channel"}
	for _, id := range ids {
		l.Entries = append(l.Entries, extract.Entry{ID: id, URL: "https://youtu.be/" + id, Title: id})
	}
	return l
}

func newRefresher(t *testing.T, ex extract.Extractor) (*Refresher, *queue.Store) {
	t.Helper()
	q, err := queue.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })
	return &Refresher{Queue: q, Extractor: ex}, q
}

func TestAddSourceExpandsAChannel(t *testing.T) {
	ex := newFake()
	ex.listings["https://www.youtube.com/@chan"] = listing("aaaaaaaaaaa", "bbbbbbbbbbb")
	r, q := newRefresher(t, ex)

	res, err := r.AddSource(context.Background(),
		source.Input{Kind: queue.SourceChannel, URL: "https://www.youtube.com/@chan"})
	if err != nil {
		t.Fatalf("AddSource: %v", err)
	}
	if res.Added != 2 || res.Seen != 2 {
		t.Errorf("added=%d seen=%d, want 2 and 2", res.Added, res.Seen)
	}
	st, _ := q.Stats(context.Background())
	if st.Total != 2 {
		t.Errorf("queue holds %d videos, want 2", st.Total)
	}
}

func TestRefreshPicksUpNewVideos(t *testing.T) {
	ex := newFake()
	url := "https://www.youtube.com/@chan"
	ex.listings[url] = listing("aaaaaaaaaaa")
	r, q := newRefresher(t, ex)
	ctx := context.Background()

	if _, err := r.AddSource(ctx, source.Input{Kind: queue.SourceChannel, URL: url}); err != nil {
		t.Fatal(err)
	}

	// the channel publishes something
	ex.listings[url] = listing("aaaaaaaaaaa", "bbbbbbbbbbb")

	results, err := r.All(ctx, nil)
	if err != nil {
		t.Fatalf("All: %v", err)
	}
	added, seen, failed := Totals(results)
	if added != 1 || failed != 0 {
		t.Errorf("added=%d seen=%d failed=%d, want only the new video", added, seen, failed)
	}
	st, _ := q.Stats(ctx)
	if st.Total != 2 {
		t.Errorf("queue holds %d videos, want 2", st.Total)
	}
}

func TestRefreshIsIdempotent(t *testing.T) {
	ex := newFake()
	url := "https://www.youtube.com/@chan"
	ex.listings[url] = listing("aaaaaaaaaaa", "bbbbbbbbbbb")
	r, _ := newRefresher(t, ex)
	ctx := context.Background()

	if _, err := r.AddSource(ctx, source.Input{Kind: queue.SourceChannel, URL: url}); err != nil {
		t.Fatal(err)
	}
	results, _ := r.All(ctx, nil)
	added, _, _ := Totals(results)
	if added != 0 {
		t.Errorf("a refresh with nothing new added %d videos", added)
	}
}

func TestSingleVideoSourceIsNotReResolved(t *testing.T) {
	ex := newFake()
	r, _ := newRefresher(t, ex)
	ctx := context.Background()

	url := "https://www.youtube.com/watch?v=aaaaaaaaaaa"
	if _, err := r.AddSource(ctx, source.Input{Kind: queue.SourceVideo, URL: url}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.All(ctx, nil); err != nil {
		t.Fatal(err)
	}
	// a single video cannot grow, so asking youtube again would buy nothing
	if n := ex.calls[url]; n != 0 {
		t.Errorf("the extractor was called %d times for a single video source", n)
	}
}

func TestOneBrokenSourceDoesNotStopTheRest(t *testing.T) {
	ex := newFake()
	good := "https://www.youtube.com/@good"
	bad := "https://www.youtube.com/@bad"
	ex.listings[good] = listing("aaaaaaaaaaa")
	ex.listings[bad] = listing("bbbbbbbbbbb")
	r, _ := newRefresher(t, ex)
	ctx := context.Background()

	for _, u := range []string{bad, good} {
		if _, err := r.AddSource(ctx, source.Input{Kind: queue.SourceChannel, URL: u}); err != nil {
			t.Fatal(err)
		}
	}

	// the channel goes away, the other one publishes
	ex.errs[bad] = errors.New("channel not found")
	ex.listings[good] = listing("aaaaaaaaaaa", "ccccccccccc")

	results, err := r.All(ctx, nil)
	if err != nil {
		t.Fatalf("All returned %v, one dead channel must not abort the sweep", err)
	}
	added, _, failed := Totals(results)
	if failed != 1 {
		t.Errorf("failed=%d, want 1", failed)
	}
	if added != 1 {
		t.Errorf("added=%d, the healthy channel should still have caught up", added)
	}
}

func TestListSourceIsReReadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "list.txt")
	if err := os.WriteFile(path, []byte("https://youtu.be/aaaaaaaaaaa\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	r, q := newRefresher(t, newFake())
	ctx := context.Background()

	inputs, err := source.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddSource(ctx, inputs[0]); err != nil {
		t.Fatal(err)
	}

	// the file grows, which a refresh should notice without any network
	if err := os.WriteFile(path,
		[]byte("https://youtu.be/aaaaaaaaaaa\nhttps://youtu.be/bbbbbbbbbbb\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := r.All(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if added, _, _ := Totals(results); added != 1 {
		t.Errorf("added=%d, want the one new link", added)
	}
	st, _ := q.Stats(ctx)
	if st.Total != 2 {
		t.Errorf("queue holds %d videos, want 2", st.Total)
	}
}

func TestLimitCapsWhatOneSourceContributes(t *testing.T) {
	ex := newFake()
	url := "https://www.youtube.com/@chan"
	ex.listings[url] = listing("aaaaaaaaaaa", "bbbbbbbbbbb", "ccccccccccc")
	r, _ := newRefresher(t, ex)
	r.Limit = 2

	res, err := r.AddSource(context.Background(), source.Input{Kind: queue.SourceChannel, URL: url})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 {
		t.Errorf("added=%d, want the limit of 2", res.Added)
	}
}

func TestSyncedAtIsRecorded(t *testing.T) {
	ex := newFake()
	url := "https://www.youtube.com/@chan"
	ex.listings[url] = listing("aaaaaaaaaaa")
	r, q := newRefresher(t, ex)
	ctx := context.Background()

	if _, err := r.AddSource(ctx, source.Input{Kind: queue.SourceChannel, URL: url}); err != nil {
		t.Fatal(err)
	}
	sources, _ := q.Sources(ctx)
	if len(sources) != 1 || sources[0].SyncedAt == nil {
		t.Errorf("the source was not marked as synced: %+v", sources)
	}
}

func TestLabelPrefersTheTitle(t *testing.T) {
	if got := Label(queue.Source{Title: "a channel", URL: "u"}); got != "a channel" {
		t.Errorf("Label = %q", got)
	}
	if got := Label(queue.Source{URL: "file:///tmp/list.txt"}); !strings.HasSuffix(got, "list.txt") {
		t.Errorf("Label = %q, want the plain path for a list", got)
	}
}
