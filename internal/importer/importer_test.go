package importer

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeInfo(t *testing.T, path string, info map[string]any) {
	t.Helper()
	data, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	write(t, path, string(data))
}

func TestExtractID(t *testing.T) {
	cases := []struct{ path, want string }{
		{"/a/video [dQw4w9WgXcQ].mp4", "dQw4w9WgXcQ"},
		{"/a/20140403 - title [r-gLSuEGhRw]/title.mp4", "r-gLSuEGhRw"},
		{"/a/20140403 - title [r-gLSuEGhRw]/title [r-gLSuEGhRw].mp4", "r-gLSuEGhRw"},
		{"/a/no id here.mp4", ""},
		{"/a/[tooshort].mp4", ""},
		{"/a/[waytoolongtobeanid].mp4", ""},
	}
	for _, c := range cases {
		if got := extractID(filepath.FromSlash(c.path)); got != c.want {
			t.Errorf("extractID(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

func TestScanFindsVideosAndSidecarMetadata(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "20150122 - some video [Bf30Zo-Ues4]")
	base := filepath.Join(dir, "some video [Bf30Zo-Ues4]")

	write(t, base+".mp4", strings.Repeat("v", 2048))
	write(t, base+".jpg", "thumb")
	writeInfo(t, base+".info.json", map[string]any{
		"id": "Bf30Zo-Ues4", "title": "some video", "uploader": "TheNakuName",
		"upload_date": "20150122", "duration": 49,
	})

	res, err := Scan(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Found) != 1 {
		t.Fatalf("found %d videos, want 1: %+v", len(res.Found), res.Found)
	}
	f := res.Found[0]
	if f.VideoID != "Bf30Zo-Ues4" || f.Title != "some video" || f.Duration != 49 {
		t.Errorf("metadata was not picked up: %+v", f)
	}
	if f.Bytes != 2048 {
		t.Errorf("size = %d, want 2048", f.Bytes)
	}
	if res.Bytes != 2048 {
		t.Errorf("total = %d, want 2048", res.Bytes)
	}
}

func TestScanUsesTheSeparateMetadataTree(t *testing.T) {
	root := t.TempDir()
	meta := t.TempDir()

	// the layout of an archive that keeps metadata apart from the videos
	write(t, filepath.Join(root, "clip [Bf30Zo-Ues4].mp4"), "data")
	writeInfo(t, filepath.Join(meta, "whatever [Bf30Zo-Ues4]", "whatever.info.json"),
		map[string]any{"id": "Bf30Zo-Ues4", "title": "from the meta tree", "duration": 49})

	res, err := Scan(context.Background(), root, meta)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Found) != 1 {
		t.Fatalf("found %d videos, want 1", len(res.Found))
	}
	if res.Found[0].Title != "from the meta tree" {
		t.Errorf("metadata was not taken from the separate tree: %+v", res.Found[0])
	}
}

func TestScanPrefersTheSidecarOverTheIndex(t *testing.T) {
	root := t.TempDir()
	meta := t.TempDir()

	base := filepath.Join(root, "clip [Bf30Zo-Ues4]")
	write(t, base+".mp4", "data")
	writeInfo(t, base+".info.json", map[string]any{"id": "Bf30Zo-Ues4", "title": "next to the file"})
	writeInfo(t, filepath.Join(meta, "x [Bf30Zo-Ues4].info.json"),
		map[string]any{"id": "Bf30Zo-Ues4", "title": "far away"})

	res, _ := Scan(context.Background(), root, meta)
	if res.Found[0].Title != "next to the file" {
		t.Errorf("the sidecar should win: %+v", res.Found[0])
	}
}

func TestScanSkipsWhatItCannotIdentify(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "mystery.mp4"), "data")
	write(t, filepath.Join(root, "notes.txt"), "not a video")
	write(t, filepath.Join(root, "empty [Bf30Zo-Ues4].mp4"), "")

	res, err := Scan(context.Background(), root, "")
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Found) != 0 {
		t.Errorf("found %d videos, want none: %+v", len(res.Found), res.Found)
	}
	if len(res.Problems) != 2 {
		t.Fatalf("reported %d problems, want 2 (no id, empty file): %+v", len(res.Problems), res.Problems)
	}
}

func TestScanIgnoresPartialFiles(t *testing.T) {
	root := t.TempDir()
	// an interrupted download is not an archived video
	write(t, filepath.Join(root, "clip [Bf30Zo-Ues4].f299.mp4.part"), "half")
	write(t, filepath.Join(root, "clip [MjAZaq5ydqc].yawn-convert.mkv"), "half")
	write(t, filepath.Join(root, "good [QMnJ-nrR43s].mp4"), "data")

	res, _ := Scan(context.Background(), root, "")
	if len(res.Found) != 1 || res.Found[0].VideoID != "QMnJ-nrR43s" {
		t.Errorf("partial files were not ignored: %+v", res.Found)
	}
}

func TestScanReportsDuplicateIDs(t *testing.T) {
	root := t.TempDir()
	write(t, filepath.Join(root, "a", "clip [Bf30Zo-Ues4].mp4"), "one")
	write(t, filepath.Join(root, "b", "clip [Bf30Zo-Ues4].mkv"), "two")

	res, _ := Scan(context.Background(), root, "")
	if len(res.Found) != 1 {
		t.Errorf("the same video was imported twice: %+v", res.Found)
	}
	if len(res.Problems) != 1 || !strings.Contains(res.Problems[0].Reason, "same video id") {
		t.Errorf("the duplicate was not reported: %+v", res.Problems)
	}
}

func TestScanRespectsContext(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 20; i++ {
		write(t, filepath.Join(root, string(rune('a'+i))+" [Bf30Zo-Ues"+string(rune('0'+i%10))+"].mp4"), "d")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := Scan(ctx, root, ""); err == nil {
		t.Error("a cancelled scan should report the cancellation")
	}
}

func TestScanHandlesFlatArchives(t *testing.T) {
	// many videos in one directory, which is what makes scoped deletion matter
	root := t.TempDir()
	for _, id := range []string{"Bf30Zo-Ues4", "QMnJ-nrR43s", "MjAZaq5ydqc"} {
		write(t, filepath.Join(root, "clip ["+id+"].mp4"), "data")
	}
	res, err := Scan(context.Background(), root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Found) != 3 {
		t.Errorf("found %d videos in a flat archive, want 3", len(res.Found))
	}
}
