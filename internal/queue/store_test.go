package queue

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("cannot open test database: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	items := make([]NewItem, 0, len(ids))
	for _, id := range ids {
		items = append(items, NewItem{VideoID: id, URL: "https://youtu.be/" + id, Title: id})
	}
	if _, err := s.AddItems(context.Background(), 0, items); err != nil {
		t.Fatalf("cannot seed the queue: %v", err)
	}
}

func TestAddItemsIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	added, err := s.AddItems(ctx, 0, []NewItem{
		{VideoID: "aaa", URL: "u1"}, {VideoID: "bbb", URL: "u2"},
	})
	if err != nil || added != 2 {
		t.Fatalf("first insert: added=%d err=%v", added, err)
	}

	added, err = s.AddItems(ctx, 0, []NewItem{
		{VideoID: "aaa", URL: "u1"}, {VideoID: "ccc", URL: "u3"},
	})
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if added != 1 {
		t.Errorf("second insert added %d, want 1 (only the new video)", added)
	}
}

func TestClaimDownloadHandsOutEachItemOnce(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa", "bbb")

	first, err := s.ClaimDownload(ctx, "worker-1", time.Minute, 3)
	if err != nil {
		t.Fatalf("first claim: %v", err)
	}
	second, err := s.ClaimDownload(ctx, "worker-2", time.Minute, 3)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if first.VideoID == second.VideoID {
		t.Fatalf("two workers got the same video %s", first.VideoID)
	}
	if _, err := s.ClaimDownload(ctx, "worker-3", time.Minute, 3); !errors.Is(err, ErrNoWork) {
		t.Errorf("third claim returned %v, want ErrNoWork", err)
	}
}

func TestExpiredLeaseIsReclaimed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	if _, err := s.ClaimDownload(ctx, "worker-1", -time.Minute, 3); err != nil {
		t.Fatalf("claim with an expired lease: %v", err)
	}
	if _, err := s.ClaimDownload(ctx, "worker-2", time.Minute, 3); !errors.Is(err, ErrNoWork) {
		t.Fatalf("the task must not be available before reclaim, got %v", err)
	}

	n, err := s.ReclaimExpired(ctx)
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if n != 1 {
		t.Errorf("reclaimed %d tasks, want 1", n)
	}
	if _, err := s.ClaimDownload(ctx, "worker-2", time.Minute, 3); err != nil {
		t.Errorf("the task must be available after reclaim: %v", err)
	}
}

func TestReclaimAllIgnoresLiveLeases(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	// a lease an hour into the future: the ordinary reclaim must not touch it
	if _, err := s.ClaimDownload(ctx, "crashed-process", time.Hour, 3); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if n, err := s.ReclaimExpired(ctx); err != nil || n != 0 {
		t.Fatalf("ReclaimExpired returned %d (err=%v), a live lease must be left alone", n, err)
	}

	n, err := s.ReclaimAll(ctx)
	if err != nil {
		t.Fatalf("ReclaimAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("ReclaimAll returned %d, want 1", n)
	}
	if _, err := s.ClaimDownload(ctx, "fresh", time.Minute, 3); err != nil {
		t.Errorf("the task must be available right after ReclaimAll: %v", err)
	}
}

func TestReleaseDoesNotBurnAttempt(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	it, err := s.ClaimDownload(ctx, "w", time.Minute, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if it.Attempts != 1 {
		t.Fatalf("attempts=%d after the claim, want 1", it.Attempts)
	}
	if err := s.ReleaseDownload(ctx, it.ID); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := s.ClaimDownload(ctx, "w", time.Minute, 3)
	if err != nil {
		t.Fatalf("second claim: %v", err)
	}
	if again.Attempts != 1 {
		t.Errorf("attempts=%d after a graceful release, want 1: stopping must not cost an attempt", again.Attempts)
	}
}

func TestFailedItemRetriesUntilLimit(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")
	const maxAttempts = 2

	for i := 1; i <= maxAttempts; i++ {
		it, err := s.ClaimDownload(ctx, "w", time.Minute, maxAttempts)
		if err != nil {
			t.Fatalf("claim on attempt %d: %v", i, err)
		}
		if err := s.MarkFailed(ctx, it.ID, errors.New("network dropped"), false, maxAttempts); err != nil {
			t.Fatalf("mark failed: %v", err)
		}
	}
	if _, err := s.ClaimDownload(ctx, "w", time.Minute, maxAttempts); !errors.Is(err, ErrNoWork) {
		t.Fatalf("the task must not be handed out once retries are spent, got %v", err)
	}
	st, err := s.Stats(ctx)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.ByState[StateFailed] != 1 {
		t.Errorf("failed=%d, want 1 (states: %v)", st.ByState[StateFailed], st.ByState)
	}
}

func TestPermanentErrorSkipsRetries(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	it, err := s.ClaimDownload(ctx, "w", time.Minute, 10)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkFailed(ctx, it.ID, errors.New("video removed"), true, 10); err != nil {
		t.Fatalf("mark permanently failed: %v", err)
	}
	if _, err := s.ClaimDownload(ctx, "w", time.Minute, 10); !errors.Is(err, ErrNoWork) {
		t.Errorf("a permanent error must not be retried, got %v", err)
	}
}

func TestItemClosesOnlyWhenEveryTargetIsDone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")
	stores := []string{"local", "r2"}

	it, err := s.ClaimDownload(ctx, "w", time.Minute, 3)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := s.MarkDownloaded(ctx, it.ID, "/tmp/aaa.mp4", 1000, "299+140"); err != nil {
		t.Fatalf("mark downloaded: %v", err)
	}
	created, err := s.EnsureTargets(ctx, stores)
	if err != nil || created != 2 {
		t.Fatalf("create targets: created=%d err=%v", created, err)
	}

	first, err := s.ClaimUpload(ctx, "u1", time.Minute, 3)
	if err != nil {
		t.Fatalf("claim upload: %v", err)
	}
	if first.LocalPath != "/tmp/aaa.mp4" {
		t.Errorf("upload got path %q, want /tmp/aaa.mp4", first.LocalPath)
	}
	allDone, err := s.MarkTargetDone(ctx, first.ID, "key1", 1000)
	if err != nil {
		t.Fatalf("close the first target: %v", err)
	}
	if allDone {
		t.Fatal("the video closed after one target out of two, the file would be deleted too early")
	}

	second, err := s.ClaimUpload(ctx, "u2", time.Minute, 3)
	if err != nil {
		t.Fatalf("claim the second upload: %v", err)
	}
	allDone, err = s.MarkTargetDone(ctx, second.ID, "key2", 1000)
	if err != nil {
		t.Fatalf("close the second target: %v", err)
	}
	if !allDone {
		t.Fatal("the video must close once every target is done")
	}

	st, _ := s.Stats(ctx)
	if st.ByState[StateDone] != 1 {
		t.Errorf("done=%d, want 1 (states: %v)", st.ByState[StateDone], st.ByState)
	}
}

func TestUploadIsNotOfferedBeforeDownload(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	if _, err := s.EnsureTargets(ctx, []string{"local"}); err != nil {
		t.Fatalf("create targets: %v", err)
	}
	if _, err := s.ClaimUpload(ctx, "u", time.Minute, 3); !errors.Is(err, ErrNoWork) {
		t.Errorf("a video that is not downloaded must not reach upload, got %v", err)
	}
}

func TestPruneTargetsDropsStoresRemovedFromConfig(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	it, _ := s.ClaimDownload(ctx, "w", time.Minute, 3)
	if err := s.MarkDownloaded(ctx, it.ID, "/tmp/aaa.mp4", 10, "f"); err != nil {
		t.Fatalf("mark downloaded: %v", err)
	}
	if _, err := s.EnsureTargets(ctx, []string{"local", "gone"}); err != nil {
		t.Fatalf("create targets: %v", err)
	}
	n, err := s.PruneTargets(ctx, []string{"local"})
	if err != nil {
		t.Fatalf("prune targets: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d targets, want 1", n)
	}
	targets, _ := s.Targets(ctx, it.ID)
	if len(targets) != 1 || targets[0].Store != "local" {
		t.Errorf("after pruning got %v, want only local", targets)
	}
}

func TestUpdateMetadataKeepsExistingValues(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AddItems(ctx, 0, []NewItem{
		{VideoID: "aaa", URL: "u", Title: "known title"},
	}); err != nil {
		t.Fatal(err)
	}
	it, _ := s.ClaimDownload(ctx, "w", time.Minute, 3)

	if err := s.UpdateMetadata(ctx, it.ID, "other title", "uploader", "20190201", 42); err != nil {
		t.Fatalf("update metadata: %v", err)
	}
	got, err := s.Item(ctx, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != "known title" {
		t.Errorf("title was overwritten: %q", got.Title)
	}
	if got.UploadDate != "20190201" || got.Duration != 42 || got.Uploader != "uploader" {
		t.Errorf("missing fields were not filled in: %+v", got)
	}
}

func TestImportDownloadedAddsANewVideo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.ImportDownloaded(ctx,
		NewItem{VideoID: "aaa", Title: "on disk", Duration: 49}, "/archive/aaa.mp4", 2048)
	if err != nil || !ok {
		t.Fatalf("import: ok=%v err=%v", ok, err)
	}

	// it must go straight to upload, never back through the downloader
	if _, err := s.ClaimDownload(ctx, "w", time.Minute, 3); !errors.Is(err, ErrNoWork) {
		t.Error("an imported video must not be offered for download")
	}
	if _, err := s.EnsureTargets(ctx, []string{"local"}); err != nil {
		t.Fatal(err)
	}
	target, err := s.ClaimUpload(ctx, "u", time.Minute, 3)
	if err != nil {
		t.Fatalf("an imported video should be ready to upload: %v", err)
	}
	if target.LocalPath != "/archive/aaa.mp4" {
		t.Errorf("upload got path %q", target.LocalPath)
	}
	if target.Item.Duration != 49 {
		t.Errorf("duration was lost: %+v", target.Item)
	}
}

func TestImportDownloadedMovesAQueuedVideo(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	ok, err := s.ImportDownloaded(ctx, NewItem{VideoID: "aaa"}, "/archive/aaa.mp4", 100)
	if err != nil || !ok {
		t.Fatalf("import: ok=%v err=%v", ok, err)
	}
	if _, err := s.ClaimDownload(ctx, "w", time.Minute, 3); !errors.Is(err, ErrNoWork) {
		t.Error("a video found on disk must not be downloaded again")
	}
	st, _ := s.Stats(ctx)
	if st.ByState[StateDownloaded] != 1 {
		t.Errorf("states: %v, want one downloaded", st.ByState)
	}
}

func TestImportDownloadedKeepsExistingTitles(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	if _, err := s.AddItems(ctx, 0, []NewItem{{VideoID: "aaa", URL: "u", Title: "known"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ImportDownloaded(ctx, NewItem{VideoID: "aaa", Title: "guessed"}, "/p.mp4", 1); err != nil {
		t.Fatal(err)
	}
	it, _ := s.Item(ctx, 1)
	if it.Title != "known" {
		t.Errorf("title = %q, want the one already recorded", it.Title)
	}
}

func TestImportDownloadedLeavesFinishedVideosAlone(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seed(t, s, "aaa")

	it, _ := s.ClaimDownload(ctx, "w", time.Minute, 3)
	if err := s.MarkDownloaded(ctx, it.ID, "/tmp/aaa.mp4", 10, "f"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnsureTargets(ctx, []string{"local"}); err != nil {
		t.Fatal(err)
	}
	target, _ := s.ClaimUpload(ctx, "u", time.Minute, 3)
	if _, err := s.MarkTargetDone(ctx, target.ID, "key", 10); err != nil {
		t.Fatal(err)
	}

	// the local copy of a finished video was deleted on purpose, so a stale
	// file on disk must not drag it back into the queue
	ok, err := s.ImportDownloaded(ctx, NewItem{VideoID: "aaa"}, "/archive/aaa.mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("a finished video must not be reopened by an import")
	}
	st, _ := s.Stats(ctx)
	if st.ByState[StateDone] != 1 {
		t.Errorf("states: %v, want it still done", st.ByState)
	}
}
