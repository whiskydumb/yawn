package media

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestOptionsValidate(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		wantErr bool
	}{
		{"remux to mkv", Options{Mode: ModeRemux, Container: "mkv"}, false},
		{"remux with no container", Options{Mode: ModeRemux}, true},
		{"remux with a codec is a contradiction", Options{Mode: ModeRemux, Container: "mkv", VideoCodec: "libx264"}, true},
		{"encode", Options{Mode: ModeEncode, Container: "mp4", VideoCodec: "libx264"}, false},
		{"encode with no codec", Options{Mode: ModeEncode, Container: "mp4"}, true},
		{"audio", Options{Mode: ModeAudio, Container: "m4a"}, false},
		{"unknown mode", Options{Mode: "transmogrify", Container: "mp4"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.opts.Validate()
			if c.wantErr != (err != nil) {
				t.Errorf("Validate() = %v, wantErr %v", err, c.wantErr)
			}
		})
	}
}

func TestBuildArgs(t *testing.T) {
	cases := []struct {
		name    string
		opts    Options
		want    []string
		notWant []string
	}{
		{
			name: "remux copies the streams",
			opts: Options{Mode: ModeRemux, Container: "mkv"},
			want: []string{"-c", "copy"},
		},
		{
			name:    "audio drops the video",
			opts:    Options{Mode: ModeAudio, Container: "m4a", AudioCodec: "aac"},
			want:    []string{"-vn", "-c:a", "aac"},
			notWant: []string{"-c:v"},
		},
		{
			name: "encode carries quality and preset",
			opts: Options{Mode: ModeEncode, Container: "mp4", VideoCodec: "libx264", CRF: 23, Preset: "slow"},
			want: []string{"-c:v", "libx264", "-crf", "23", "-preset", "slow"},
		},
		{
			name:    "crf wins over bitrate",
			opts:    Options{Mode: ModeEncode, Container: "mp4", VideoCodec: "libx264", CRF: 20, Bitrate: "2M"},
			want:    []string{"-crf", "20"},
			notWant: []string{"-b:v"},
		},
		{
			name: "mp4 gets faststart",
			opts: Options{Mode: ModeRemux, Container: "mp4"},
			want: []string{"-movflags", "+faststart"},
		},
		{
			name:    "mkv does not need faststart",
			opts:    Options{Mode: ModeRemux, Container: "mkv"},
			notWant: []string{"-movflags"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := strings.Join(buildArgs("in.mp4", "out.tmp", c.opts), " ")
			for _, w := range c.want {
				if !strings.Contains(got, w) {
					t.Errorf("args are missing %q:\n%s", w, got)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(got, w) {
					t.Errorf("args should not contain %q:\n%s", w, got)
				}
			}
		})
	}
}

func TestBuildArgsNeverBlocksOnStdin(t *testing.T) {
	// without -nostdin ffmpeg fights the pipeline for the terminal and the
	// whole run appears to hang
	got := strings.Join(buildArgs("in.mp4", "out.tmp", Options{Mode: ModeRemux, Container: "mkv"}), " ")
	if !strings.Contains(got, "-nostdin") {
		t.Error("-nostdin is missing")
	}
}

func TestOutputPath(t *testing.T) {
	cases := []struct{ src, container, want string }{
		{"/a/b/video.mp4", "mkv", "/a/b/video.mkv"},
		{"/a/b/video.mp4", ".mkv", "/a/b/video.mkv"},
		{"/a/b/video.webm", "mp4", "/a/b/video.mp4"},
		{"/a/b/video with [id].mp4", "m4a", "/a/b/video with [id].m4a"},
	}
	for _, c := range cases {
		if got := outputPath(c.src, c.container); got != c.want {
			t.Errorf("outputPath(%q, %q) = %q, want %q", c.src, c.container, got, c.want)
		}
	}
}

func TestReadProgress(t *testing.T) {
	stream := strings.Join([]string{
		"bitrate=1234kbits/s",
		"total_size=1048576",
		"out_time_us=5000000",
		"speed=12.5x",
		"progress=continue",
		"total_size=2097152",
		"out_time_us=10000000",
		"speed=13.0x",
		"progress=end",
	}, "\n")

	var got []Progress
	readProgress(strings.NewReader(stream), 20, func(p Progress) { got = append(got, p) })

	if len(got) != 2 {
		t.Fatalf("got %d updates, want 2: %+v", len(got), got)
	}
	if got[0].Seconds != 5 || got[0].Bytes != 1048576 || got[0].Speed != 12.5 {
		t.Errorf("first update is wrong: %+v", got[0])
	}
	if got[1].Percent() != 0.5 {
		t.Errorf("percent = %v, want 0.5 (10s of 20s)", got[1].Percent())
	}
}

func TestProgressPercentWithoutDuration(t *testing.T) {
	if got := (Progress{Seconds: 5}).Percent(); got != -1 {
		t.Errorf("Percent = %v with an unknown duration, want -1", got)
	}
	if got := (Progress{Seconds: 30, Duration: 20}).Percent(); got != 1 {
		t.Errorf("Percent = %v past the end, want it clamped to 1", got)
	}
}

// --- these need a real ffmpeg -----------------------------------------------

func requireFFmpeg(t *testing.T) *Converter {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg is not installed")
	}
	c := NewConverter("")
	if err := c.Available(context.Background()); err != nil {
		t.Skip("ffmpeg is not runnable")
	}
	return c
}

// makeSample renders a couple of seconds of test pattern with sound.
func makeSample(t *testing.T, dir string, seconds int) string {
	t.Helper()
	path := filepath.Join(dir, "sample.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:rate=15",
		"-f", "lavfi", "-i", "sine=frequency=440",
		"-t", string(rune('0'+seconds)),
		"-c:v", "libx264", "-preset", "ultrafast", "-c:a", "aac",
		path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("cannot render a sample: %v: %s", err, out)
	}
	return path
}

func TestConvertRemuxesAndReplacesTheSource(t *testing.T) {
	c := requireFFmpeg(t)
	dir := t.TempDir()
	src := makeSample(t, dir, 2)

	res, err := c.Convert(context.Background(), src,
		Options{Mode: ModeRemux, Container: "mkv"}, 2, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if filepath.Ext(res.Path) != ".mkv" {
		t.Errorf("output is %q, want an mkv", res.Path)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("the source survived the conversion")
	}
	if res.Bytes == 0 {
		t.Error("the output is empty")
	}

	// nothing temporary may be left behind
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "yawn-convert") {
			t.Errorf("a temporary file survived: %s", e.Name())
		}
	}
}

func TestConvertReportsProgress(t *testing.T) {
	c := requireFFmpeg(t)
	dir := t.TempDir()
	src := makeSample(t, dir, 3)

	var updates int
	var last Progress
	_, err := c.Convert(context.Background(), src,
		Options{Mode: ModeRemux, Container: "mkv"}, 3,
		func(p Progress) { updates++; last = p })
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	if updates == 0 {
		t.Fatal("no progress was reported")
	}
	if last.Seconds <= 0 {
		t.Errorf("the last update carries no position: %+v", last)
	}
}

func TestConvertToAudioDropsTheVideo(t *testing.T) {
	c := requireFFmpeg(t)
	dir := t.TempDir()
	src := makeSample(t, dir, 2)

	res, err := c.Convert(context.Background(), src,
		Options{Mode: ModeAudio, Container: "m4a", AudioCodec: "aac"}, 2, nil)
	if err != nil {
		t.Fatalf("Convert: %v", err)
	}
	probe, err := NewProber("").Probe(context.Background(), res.Path)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if probe.Video {
		t.Error("the output still carries video")
	}
	if !probe.Audio {
		t.Error("the output lost its audio")
	}
}

func TestConvertLeavesTheSourceOnFailure(t *testing.T) {
	c := requireFFmpeg(t)
	dir := t.TempDir()
	src := makeSample(t, dir, 1)

	_, err := c.Convert(context.Background(), src,
		Options{Mode: ModeEncode, Container: "mp4", VideoCodec: "definitely_not_a_codec"}, 1, nil)
	if err == nil {
		t.Fatal("an unknown codec must fail")
	}
	if !errors.Is(err, ErrConvert) {
		t.Errorf("want ErrConvert, got %v", err)
	}
	if _, statErr := os.Stat(src); statErr != nil {
		t.Error("a failed conversion must leave the source alone")
	}
}

func TestSupportsEncodersCatchesAMissingCodec(t *testing.T) {
	c := requireFFmpeg(t)
	ctx := context.Background()

	if err := c.SupportsEncoders(ctx, "definitely_not_a_codec"); err == nil {
		t.Error("a missing encoder must be reported at startup")
	}
	// copy is not an encoder and must not be looked up
	if err := c.SupportsEncoders(ctx, "copy", ""); err != nil {
		t.Errorf("copy should always pass: %v", err)
	}
	if err := c.SupportsEncoders(ctx, "aac"); err != nil {
		t.Errorf("every ffmpeg build has aac: %v", err)
	}
}

func TestTempPathKeepsTheExtension(t *testing.T) {
	// ffmpeg picks its muxer from the output name, so the temporary file has
	// to end in the target container or it cannot write anything at all
	got := tempPath("/a/b/video.mkv")
	if filepath.Ext(got) != ".mkv" {
		t.Errorf("tempPath(%q) = %q, want it to keep the .mkv extension", "/a/b/video.mkv", got)
	}
	if !strings.Contains(got, "yawn-convert") {
		t.Errorf("tempPath(%q) = %q, want it marked as temporary", "/a/b/video.mkv", got)
	}
	if got == "/a/b/video.mkv" {
		t.Error("the temporary path must differ from the destination")
	}
}
