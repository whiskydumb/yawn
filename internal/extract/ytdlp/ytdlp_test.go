package ytdlp

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/extract"
)

func TestParseProgressHandlesUnknownValues(t *testing.T) {
	cases := []struct {
		name string
		line string
		want extract.Progress
	}{
		{
			name: "complete line",
			line: "downloading|1048576|10485760|NA|524288.5|18|3|120",
			want: extract.Progress{
				Stage: "download", Status: "downloading",
				Downloaded: 1048576, Total: 10485760, Speed: 524288.5, ETA: 18,
				FragmentIndex: 3, FragmentCount: 120,
			},
		},
		{
			name: "exact size unknown, estimate is used",
			line: "downloading|500|NA|9000|NA|NA|NA|NA",
			want: extract.Progress{
				Stage: "download", Status: "downloading", Downloaded: 500, Total: 9000,
			},
		},
		{
			name: "finished",
			line: "finished|9000|9000|9000|0|0|NA|NA",
			want: extract.Progress{
				Stage: "download", Status: "finished", Downloaded: 9000, Total: 9000,
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := parseProgress(c.line)
			if !ok {
				t.Fatalf("line did not parse: %q", c.line)
			}
			if got != c.want {
				t.Errorf("got  %+v\nwant %+v", got, c.want)
			}
		})
	}
}

func TestParseProgressRejectsGarbage(t *testing.T) {
	if _, ok := parseProgress("two|columns"); ok {
		t.Error("a short line must not parse")
	}
}

func TestProgressPercent(t *testing.T) {
	if p := (extract.Progress{Downloaded: 50, Total: 200}).Percent(); p != 0.25 {
		t.Errorf("Percent = %v, want 0.25", p)
	}
	if p := (extract.Progress{Downloaded: 50}).Percent(); p != -1 {
		t.Errorf("Percent = %v with an unknown size, want -1", p)
	}
}

func TestReadProgressIgnoresUnmarkedOutput(t *testing.T) {
	input := strings.Join([]string{
		"[youtube] Extracting URL: https://youtu.be/abc",
		progressMark + "downloading|100|1000|NA|50|10|NA|NA",
		"[download] Destination: video.mp4",
		ppMark + "started",
		progressMark + "finished|1000|1000|1000|0|0|NA|NA",
	}, "\n")

	var got []extract.Progress
	readProgress(strings.NewReader(input), func(p extract.Progress) { got = append(got, p) })

	if len(got) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(got), got)
	}
	if got[1].Stage != "postprocess" || got[1].Status != "started" {
		t.Errorf("second event %+v, want a postprocess one", got[1])
	}
	if got[2].Status != "finished" {
		t.Errorf("last event %+v, want finished", got[2])
	}
}

func TestClassifyRecognisesPermanentFailures(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   error
	}{
		{"removed video", "ERROR: [youtube] abc: Video unavailable", extract.ErrUnavailable},
		{"private", "ERROR: [youtube] abc: Private video. Sign in if you've been granted access", extract.ErrUnavailable},
		{"members only", "ERROR: Join this channel to get access to members-only content", extract.ErrUnavailable},
		{"bot check", "ERROR: Sign in to confirm you're not a bot. Use --cookies", extract.ErrBlocked},
		{"rate limited", "ERROR: HTTP Error 429: Too Many Requests", extract.ErrBlocked},
		{"missing token", "ERROR: The following content is not available: PO Token required", extract.ErrBlocked},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := classify(errors.New("exit status 1"), c.stderr)
			if !errors.Is(err, c.want) {
				t.Errorf("classify(%q) = %v, want it to wrap %v", c.stderr, err, c.want)
			}
		})
	}
}

func TestClassifyLeavesUnknownErrorsRetryable(t *testing.T) {
	err := classify(errors.New("exit status 1"), "ERROR: unable to download video data: <urlopen error timed out>")
	if errors.Is(err, extract.ErrUnavailable) || errors.Is(err, extract.ErrBlocked) {
		t.Errorf("a network timeout was marked permanent: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("the error text was lost: %v", err)
	}
}

func TestCollectFlattensChannelTabs(t *testing.T) {
	root := flatEntry{
		ID: "UCxxx", Title: "channel", Type: "playlist",
		Entries: []flatEntry{
			{ID: "tab1", Type: "playlist", Entries: []flatEntry{
				{ID: "vid1", Title: "first", Duration: 60},
				{ID: "vid2", Title: "second", WebpageURL: "https://youtu.be/vid2"},
			}},
			{ID: "tab2", Type: "playlist", Entries: []flatEntry{
				{ID: "vid3", Title: "third"},
			}},
		},
	}
	var listing extract.Listing
	collect(&root, &listing)

	if len(listing.Entries) != 3 {
		t.Fatalf("collected %d entries, want 3: %+v", len(listing.Entries), listing.Entries)
	}
	if listing.Entries[0].URL != "https://www.youtube.com/watch?v=vid1" {
		t.Errorf("built %q for an entry with no URL", listing.Entries[0].URL)
	}
	if listing.Entries[1].URL != "https://youtu.be/vid2" {
		t.Errorf("an existing URL was overwritten: %q", listing.Entries[1].URL)
	}
}

func TestReadResultParsesMetadata(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "video.mp4")
	content := []byte("some bytes")
	if err := os.WriteFile(media, content, 0o644); err != nil {
		t.Fatal(err)
	}
	// exactly what yt-dlp prints through %(...)j
	payload, err := json.Marshal(map[string]any{
		"filepath":        media,
		"format_id":       "299+140",
		"filesize":        nil,
		"filesize_approx": 4096,
		"title":           "Hunger games #17 [WAT? :0]",
		"uploader":        "TheNakuName",
		"upload_date":     "20190201",
		"duration":        7,
	})
	if err != nil {
		t.Fatal(err)
	}

	resultFile := filepath.Join(dir, "result.txt")
	if err := os.WriteFile(resultFile, append(payload, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := readResult(resultFile)
	if err != nil {
		t.Fatalf("parse result: %v", err)
	}
	if res.Title != "Hunger games #17 [WAT? :0]" {
		t.Errorf("title %q: separator characters in a title must not break parsing", res.Title)
	}
	if res.UploadDate != "20190201" || res.Duration != 7 || res.FormatID != "299+140" {
		t.Errorf("metadata parsed wrong: %+v", res)
	}
	// the size comes from disk, not from what the extractor promised
	if res.Bytes != int64(len(content)) {
		t.Errorf("size %d, want the real file size %d", res.Bytes, len(content))
	}
}

func TestReadResultRejectsEmptyOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(path, []byte("\n\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readResult(path); err == nil {
		t.Error("an empty result must be an error, not a silent success")
	}
}

func TestJSRuntimeIsNotAddedWhenTheUserPickedOne(t *testing.T) {
	cases := [][]string{
		{"--js-runtimes", "deno:/opt/deno"},
		{"--js-runtimes=node"},
		{"--no-js-runtimes"},
	}
	for _, extra := range cases {
		e := New("", extra)
		if got := e.jsRuntimeArgs(); got != nil {
			t.Errorf("with %v the extractor added %v, want nothing: the user decided already", extra, got)
		}
	}
}

func TestJSRuntimeDetectionIsCachedAndSafe(t *testing.T) {
	e := New("", nil)
	first := e.jsRuntimeArgs()
	second := e.jsRuntimeArgs()

	if len(first) != len(second) {
		t.Fatalf("detection is not stable: %v then %v", first, second)
	}
	// whatever this machine has, the result must be a valid pair or empty
	if len(first) != 0 && len(first) != 2 {
		t.Errorf("got %v, want either nothing or a flag with its value", first)
	}
	if len(first) == 2 && first[0] != "--js-runtimes" {
		t.Errorf("got %v, want the js-runtimes flag", first)
	}
}

func TestBaseArgsAlwaysIgnoresTheUserConfig(t *testing.T) {
	// a global yt-dlp.conf would silently change behaviour and make runs
	// unreproducible, which is why this is not optional
	args := New("", nil).baseArgs()
	found := false
	for _, a := range args {
		if a == "--ignore-config" {
			found = true
		}
	}
	if !found {
		t.Errorf("baseArgs = %v, want --ignore-config among them", args)
	}
}
