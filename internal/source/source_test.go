package source

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/whiskydumb/yawn/internal/queue"
)

func TestKind(t *testing.T) {
	cases := []struct {
		in   string
		want queue.SourceKind
	}{
		{"https://www.youtube.com/@The_Naku_Names", queue.SourceChannel},
		{"@The_Naku_Names", queue.SourceChannel},
		{"https://www.youtube.com/channel/UCabc", queue.SourceChannel},
		{"https://www.youtube.com/playlist?list=PLabc", queue.SourcePlaylist},
		{"https://youtu.be/r-gLSuEGhRw", queue.SourceVideo},
		{"r-gLSuEGhRw", queue.SourceVideo},
	}
	for _, c := range cases {
		if got := Kind(c.in); got != c.want {
			t.Errorf("Kind(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestExtractID(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://youtu.be/r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"https://www.youtube.com/watch?v=r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"https://www.youtube.com/watch?v=r-gLSuEGhRw&list=PLabc&index=2", "r-gLSuEGhRw"},
		{"https://www.youtube.com/shorts/r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"https://www.youtube.com/embed/r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"https://www.youtube.com/live/r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"r-gLSuEGhRw", "r-gLSuEGhRw"},
		{"https://www.youtube.com/@The_Naku_Names", ""},
		{"https://www.youtube.com/playlist?list=PLabc", ""},
		{"tooshort", ""},
	}
	for _, c := range cases {
		if got := ExtractID(c.in); got != c.want {
			t.Errorf("ExtractID(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNormalize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"r-gLSuEGhRw", "https://www.youtube.com/watch?v=r-gLSuEGhRw"},
		{"@channel", "https://www.youtube.com/@channel"},
		{"youtube.com/@channel", "https://youtube.com/@channel"},
		{"https://youtu.be/abc", "https://youtu.be/abc"},
	}
	for _, c := range cases {
		if got := Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestReadListMatchesExistingArchiveFormat(t *testing.T) {
	content := "" +
		"https://youtu.be/r-gLSuEGhRw # Hunger games #1\n" +
		"\n" +
		"# a fully commented line\n" +
		"https://youtu.be/IDp6dbWcCa8 # Hunger games #2\n" +
		"https://www.youtube.com/watch?v=r-gLSuEGhRw # same video, other link shape\n"

	path := filepath.Join(t.TempDir(), "queue.txt")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := ReadList(path)
	if err != nil {
		t.Fatalf("read list: %v", err)
	}
	want := []string{
		"https://www.youtube.com/watch?v=r-gLSuEGhRw",
		"https://www.youtube.com/watch?v=IDp6dbWcCa8",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d links %v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("link %d: %q, want %q", i, got[i], want[i])
		}
	}
}

func TestParseExpandsListFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "list.txt")
	if err := os.WriteFile(path, []byte("https://youtu.be/r-gLSuEGhRw\nIDp6dbWcCa8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := Parse(path)
	if err != nil {
		t.Fatalf("parse file: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].FromList != path {
		t.Errorf("entry is not marked as coming from a list: %+v", got[0])
	}
}

func TestParseRejectsEmpty(t *testing.T) {
	if _, err := Parse("   "); err == nil {
		t.Error("an empty argument must be rejected")
	}
}
