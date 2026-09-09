package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/store"
	"gopkg.in/yaml.v3"
)

func newStore(t *testing.T, root string) *Store {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("path: "+strings.ReplaceAll(root, "\\", "/")), &doc); err != nil {
		t.Fatalf("cannot build options: %v", err)
	}
	u, err := New("test", "{id}", *doc.Content[0])
	if err != nil {
		t.Fatalf("cannot create the store: %v", err)
	}
	return u.(*Store)
}

func writeFile(t *testing.T, path, content string) store.Asset {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return store.Asset{Path: path, Rel: filepath.Base(path), Size: int64(len(content))}
}

func TestPutCopiesWholeSet(t *testing.T) {
	src, dstRoot := t.TempDir(), t.TempDir()
	s := newStore(t, dstRoot)
	ctx := context.Background()

	req := store.PutRequest{
		Prefix: "2014/some video [abc]",
		Assets: []store.Asset{
			writeFile(t, filepath.Join(src, "video.mp4"), "video data"),
			writeFile(t, filepath.Join(src, "video.info.json"), "{}"),
			writeFile(t, filepath.Join(src, "video.jpg"), "thumbnail"),
		},
	}

	var last store.Progress
	res, err := s.Put(ctx, req, func(p store.Progress) { last = p })
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if res.Bytes != req.TotalBytes() {
		t.Errorf("wrote %d bytes, want %d", res.Bytes, req.TotalBytes())
	}
	if last.AssetCount != 3 {
		t.Errorf("progress reported %d files, want 3", last.AssetCount)
	}
	for _, name := range []string{"video.mp4", "video.info.json", "video.jpg"} {
		if _, err := os.Stat(filepath.Join(dstRoot, "2014", "some video [abc]", name)); err != nil {
			t.Errorf("sidecar %s did not arrive: %v", name, err)
		}
	}
}

func TestPutSkipsAlreadyCopiedFiles(t *testing.T) {
	src, dstRoot := t.TempDir(), t.TempDir()
	s := newStore(t, dstRoot)
	ctx := context.Background()

	asset := writeFile(t, filepath.Join(src, "video.mp4"), "data")
	req := store.PutRequest{Prefix: "p", Assets: []store.Asset{asset}}

	if _, err := s.Put(ctx, req, nil); err != nil {
		t.Fatalf("first put: %v", err)
	}
	dst := filepath.Join(dstRoot, "p", "video.mp4")
	before, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put(ctx, req, nil); err != nil {
		t.Fatalf("second put: %v", err)
	}
	after, _ := os.Stat(dst)
	if !before.ModTime().Equal(after.ModTime()) {
		t.Error("a file of the same size was copied again; a repeat should be free")
	}
}

func TestPutLeavesNoPartialsOnCancel(t *testing.T) {
	src, dstRoot := t.TempDir(), t.TempDir()
	s := newStore(t, dstRoot)

	big := strings.Repeat("x", 4<<20) // more than one copy chunk
	asset := writeFile(t, filepath.Join(src, "big.mp4"), big)

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel before it starts

	_, err := s.Put(ctx, store.PutRequest{Prefix: "p", Assets: []store.Asset{asset}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want a cancellation, got %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dstRoot, "p"))
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yawn-part") {
			t.Errorf("a temporary file survived the cancel: %s", e.Name())
		}
		if e.Name() == "big.mp4" {
			t.Error("a partially copied file reached the archive under its final name")
		}
	}
}

func TestHasReportsMissingAndPresent(t *testing.T) {
	src, dstRoot := t.TempDir(), t.TempDir()
	s := newStore(t, dstRoot)
	ctx := context.Background()

	ok, _, err := s.Has(ctx, "no/such")
	if err != nil || ok {
		t.Errorf("Has on a missing prefix gave ok=%v err=%v", ok, err)
	}

	asset := writeFile(t, filepath.Join(src, "video.mp4"), "data")
	if _, err := s.Put(ctx, store.PutRequest{Prefix: "p", Assets: []store.Asset{asset}}, nil); err != nil {
		t.Fatal(err)
	}
	ok, size, err := s.Has(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || size != asset.Size {
		t.Errorf("Has = (%v, %d), want (true, %d)", ok, size, asset.Size)
	}
}

func TestNewRequiresPath(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("min_free: 1G"), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := New("test", "{id}", *doc.Content[0]); err == nil {
		t.Error("a store without options.path must be rejected")
	}
}

func TestNewRejectsBadMinFree(t *testing.T) {
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("path: /tmp/x\nmin_free: plenty"), &doc); err != nil {
		t.Fatal(err)
	}
	if _, err := New("test", "{id}", *doc.Content[0]); err == nil {
		t.Error("an unparsable min_free must be rejected at startup, not silently ignored")
	}
}
