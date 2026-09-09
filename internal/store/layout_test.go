package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderLayoutByYear(t *testing.T) {
	got := RenderLayout("{upload_year}/{upload_date} - {title} [{id}]", Vars{
		ID: "r-gLSuEGhRw", Title: "Hunger games #1", UploadDate: "20140403",
	})
	want := "2014/20140403 - Hunger games #1 [r-gLSuEGhRw]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestRenderLayoutSurvivesMissingDate(t *testing.T) {
	got := RenderLayout("{upload_year}/{id}", Vars{ID: "abc"})
	if got != "unknown/abc" {
		t.Errorf("got %q, want unknown/abc", got)
	}
}

func TestRenderLayoutFallsBackToID(t *testing.T) {
	if got := RenderLayout("///", Vars{ID: "abc"}); got != "abc" {
		t.Errorf("an empty template produced %q, want abc", got)
	}
}

func TestSanitizeKeepsTitleReadable(t *testing.T) {
	got := SanitizeSegment("Hunger games #17 <WAT?> :0 */|")
	for _, bad := range []string{"?", ":", "/", "\\", "<", ">", "\"", "|", "*"} {
		if strings.Contains(got, bad) {
			t.Fatalf("forbidden character %q survived in %q", bad, got)
		}
	}
	if !strings.Contains(got, "Hunger games #17") {
		t.Errorf("the title became unreadable: %q", got)
	}
}

func TestSanitizeIsAscii(t *testing.T) {
	got := SanitizeSegment("a<b>c:d\"e/f|g?h*i")
	for _, r := range got {
		if r > 127 {
			t.Errorf("replacement produced a non-ascii rune %q in %q", r, got)
		}
	}
}

func TestSanitizeTrimsTrailingDotsAndSpaces(t *testing.T) {
	if got := SanitizeSegment("some name...  "); got != "some name" {
		t.Errorf("got %q, want 'some name': windows cannot store a trailing dot", got)
	}
}

func TestSanitizeEscapesReservedNames(t *testing.T) {
	for _, name := range []string{"CON", "nul", "COM1", "aux.txt"} {
		got := SanitizeSegment(name)
		if strings.EqualFold(got, name) {
			t.Errorf("reserved name %q was left as is: %q", name, got)
		}
	}
}

func TestSanitizeLimitsLength(t *testing.T) {
	long := strings.Repeat("long-title-", 40)
	got := SanitizeSegment(long)
	if n := utf8.RuneCountInString(got); n > maxSegment {
		t.Errorf("segment is %d runes, the cap is %d", n, maxSegment)
	}
	if !utf8.ValidString(got) {
		t.Error("trimming split a multi-byte rune")
	}
}

func TestSanitizeDropsControlCharacters(t *testing.T) {
	if got := SanitizeSegment("na\x00m\x1fe"); got != "name" {
		t.Errorf("got %q, want 'name'", got)
	}
}

func TestLongTitleDoesNotEatTheID(t *testing.T) {
	long := strings.Repeat("a very long title segment ", 10)
	got := RenderLayout("{upload_year}/{upload_date} - {title} [{id}]", Vars{
		ID: "QMnJ-nrR43s", Title: long, UploadDate: "20190201",
	})

	if !strings.HasSuffix(got, "[QMnJ-nrR43s]") {
		t.Errorf("the id did not survive trimming: %q", got)
	}
	if !strings.Contains(got, "20190201") {
		t.Errorf("the date did not survive trimming: %q", got)
	}
	segment := got[strings.Index(got, "/")+1:]
	if n := utf8.RuneCountInString(segment); n > maxSegment {
		t.Errorf("segment is %d runes, over the cap of %d: %q", n, maxSegment, segment)
	}
}

func TestShortTitleIsLeftAlone(t *testing.T) {
	got := RenderLayout("{upload_date} - {title} [{id}]", Vars{
		ID: "abc", Title: "short one", UploadDate: "20190201",
	})
	if got != "20190201 - short one [abc]" {
		t.Errorf("got %q, want the name unchanged", got)
	}
}

func TestOverlongTemplateStillRespectsSegmentCap(t *testing.T) {
	// a template whose fixed part alone does not fit: the title gets nothing,
	// but the name cap still holds
	got := RenderLayout(strings.Repeat("x", maxSegment+20)+"{title}", Vars{
		ID: "abc", Title: "a title",
	})
	if n := utf8.RuneCountInString(got); n > maxSegment {
		t.Errorf("segment is %d runes, over the cap of %d", n, maxSegment)
	}
}

func TestPutRequestTotalBytes(t *testing.T) {
	req := PutRequest{Assets: []Asset{{Size: 100}, {Size: 250}}}
	if got := req.TotalBytes(); got != 350 {
		t.Errorf("TotalBytes = %d, want 350", got)
	}
}

func TestRenderLayoutSeparatesByChannel(t *testing.T) {
	got := RenderLayout("{uploader}/{upload_year}/{upload_date} - {title} [{id}]", Vars{
		ID: "Bf30Zo-Ues4", Title: "some video", Uploader: "TheNakuName", UploadDate: "20150122",
	})
	want := "TheNakuName/2015/20150122 - some video [Bf30Zo-Ues4]"
	if got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

func TestMissingUploaderKeepsTheTreeShape(t *testing.T) {
	// without the fallback this video would land one level higher than the
	// rest and the archive would end up with two different shapes
	got := RenderLayout("{uploader}/{upload_year}/{id}", Vars{ID: "abc", UploadDate: "20150122"})
	if got != "unknown/2015/abc" {
		t.Errorf("got %q, want unknown/2015/abc", got)
	}
	if n := strings.Count(got, "/"); n != 2 {
		t.Errorf("got %d levels, want the same 3 as a video with an uploader", n+1)
	}
}

func TestChannelNameIsSanitised(t *testing.T) {
	got := RenderLayout("{uploader}/{id}", Vars{ID: "abc", Uploader: "chan/nel: weird?"})
	if strings.Count(got, "/") != 1 {
		t.Errorf("a slash in the channel name created an extra level: %q", got)
	}
	for _, bad := range []string{":", "?"} {
		if strings.Contains(got, bad) {
			t.Errorf("forbidden character %q survived in %q", bad, got)
		}
	}
}
