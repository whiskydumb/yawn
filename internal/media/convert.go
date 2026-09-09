package media

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// ErrConvert is a conversion that failed for a reason retrying will not fix:
// a codec the build does not have, an argument ffmpeg rejects.
var ErrConvert = errors.New("conversion failed")

// Mode is what the converter does to a file.
type Mode string

const (
	// ModeRemux changes the container without touching the streams. Fast and
	// lossless, which is what an archive usually wants.
	ModeRemux Mode = "remux"

	// ModeEncode re-encodes the video. Expect hours per hundred videos.
	ModeEncode Mode = "encode"

	// ModeAudio throws the video away and keeps the sound.
	ModeAudio Mode = "audio"
)

// Options describe one conversion.
type Options struct {
	Mode      Mode
	Container string

	VideoCodec string
	AudioCodec string

	// CRF is the quality knob for crf based encoders; zero leaves it out.
	CRF int

	// Preset is the speed and quality trade-off, e.g. slow, p4, veryfast.
	Preset string

	// Bitrate sets a target video bitrate, e.g. "2M". Ignored when CRF is set.
	Bitrate string

	ExtraArgs []string
}

// Validate rejects a configuration before the first video is downloaded
// rather than after the four hundredth.
func (o Options) Validate() error {
	switch o.Mode {
	case ModeRemux:
		if o.Container == "" {
			return errors.New("remux needs a container")
		}
		if o.VideoCodec != "" || o.AudioCodec != "" {
			return errors.New("remux copies the streams, so it cannot take a codec: use mode encode")
		}
	case ModeEncode:
		if o.Container == "" {
			return errors.New("encode needs a container")
		}
		if o.VideoCodec == "" {
			return errors.New("encode needs a video_codec")
		}
	case ModeAudio:
		if o.Container == "" {
			return errors.New("audio needs a container, for example m4a or opus")
		}
	default:
		return fmt.Errorf("unknown mode %q: use remux, encode or audio", o.Mode)
	}
	return nil
}

// Converter runs ffmpeg.
type Converter struct {
	binary string

	once     sync.Once
	encoders map[string]bool
	probeErr error
}

// NewConverter wraps ffmpeg. An empty binary means "look in PATH".
func NewConverter(binary string) *Converter {
	if binary == "" {
		binary = "ffmpeg"
	}
	return &Converter{binary: binary}
}

func (c *Converter) Available(ctx context.Context) error {
	if _, err := exec.CommandContext(ctx, c.binary, "-version").Output(); err != nil {
		return fmt.Errorf("cannot run %s: %w (install ffmpeg)", c.binary, err)
	}
	return nil
}

// SupportsEncoders checks that this ffmpeg build actually has the codecs the
// configuration asks for. A missing hardware encoder is the usual surprise,
// and finding out at startup beats finding out mid-archive.
func (c *Converter) SupportsEncoders(ctx context.Context, names ...string) error {
	c.once.Do(func() { c.encoders, c.probeErr = c.listEncoders(ctx) })
	if c.probeErr != nil {
		// if the list cannot be read, let ffmpeg itself complain later
		return nil
	}
	var missing []string
	for _, n := range names {
		if n == "" || n == "copy" {
			continue
		}
		if !c.encoders[n] {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: this ffmpeg has no encoder %s (see: %s -encoders)",
			ErrConvert, strings.Join(missing, ", "), c.binary)
	}
	return nil
}

func (c *Converter) listEncoders(ctx context.Context) (map[string]bool, error) {
	out, err := exec.CommandContext(ctx, c.binary, "-hide_banner", "-encoders").Output()
	if err != nil {
		return nil, err
	}
	found := make(map[string]bool)
	for _, line := range strings.Split(string(out), "\n") {
		// lines look like " V....D libx264   H.264 encoder"; the name is the
		// second field, after the capability flags
		fields := strings.Fields(line)
		if len(fields) < 2 || len(fields[0]) < 6 {
			continue
		}
		found[fields[1]] = true
	}
	return found, nil
}

// Progress is how far a conversion has got.
type Progress struct {
	// Seconds already written of the output.
	Seconds float64

	// Duration of the source, zero when it is unknown.
	Duration float64

	Bytes int64
	Speed float64 // times realtime
}

func (p Progress) Percent() float64 {
	if p.Duration <= 0 {
		return -1
	}
	v := p.Seconds / p.Duration
	if v > 1 {
		return 1
	}
	return v
}

type ProgressFunc func(Progress)

// Result is the converted file.
type Result struct {
	Path  string
	Bytes int64
}

// Convert rewrites a file according to opts.
//
// The output is written next to the source under a temporary name and only
// then put in place, so an interrupted conversion never leaves a half file
// that the next run would treat as the finished video.
func (c *Converter) Convert(ctx context.Context, src string, opts Options, duration float64, onProgress ProgressFunc) (*Result, error) {
	if err := opts.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConvert, err)
	}
	dst := outputPath(src, opts.Container)
	tmp := tempPath(dst)

	// converting onto the source would truncate the input mid-read
	if tmp == src {
		return nil, fmt.Errorf("%w: the temporary output collides with the source %s", ErrConvert, src)
	}
	defer os.Remove(tmp)

	args := buildArgs(src, tmp, opts)
	cmd := exec.CommandContext(ctx, c.binary, args...)
	configureProcess(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start ffmpeg: %w", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		readProgress(stdout, duration, onProgress)
	}()
	wg.Wait()

	if err := cmd.Wait(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: ffmpeg on %s: %v (%s)",
			ErrConvert, filepath.Base(src), err, lastLine(stderr.String()))
	}

	info, err := os.Stat(tmp)
	if err != nil {
		return nil, fmt.Errorf("%w: ffmpeg produced no output for %s", ErrConvert, src)
	}
	if info.Size() == 0 {
		return nil, fmt.Errorf("%w: ffmpeg produced an empty file for %s", ErrConvert, src)
	}

	// the source goes only after the output is safely in place
	if err := os.Rename(tmp, dst); err != nil {
		return nil, fmt.Errorf("cannot put the converted file in place: %w", err)
	}
	if dst != src {
		if err := os.Remove(src); err != nil {
			return nil, fmt.Errorf("cannot remove the source after conversion: %w", err)
		}
	}
	return &Result{Path: dst, Bytes: info.Size()}, nil
}

// tempPath keeps the target extension: ffmpeg picks its muxer from the
// output file name, and a name ending in .yawn-convert leaves it with no idea
// what container to write.
func tempPath(dst string) string {
	ext := filepath.Ext(dst)
	return strings.TrimSuffix(dst, ext) + ".yawn-convert" + ext
}

// outputPath swaps the extension for the target container.
func outputPath(src, container string) string {
	ext := strings.TrimPrefix(container, ".")
	return strings.TrimSuffix(src, filepath.Ext(src)) + "." + ext
}

func buildArgs(src, dst string, opts Options) []string {
	args := []string{
		"-hide_banner",
		"-nostdin", // without it ffmpeg fights the parent for the terminal
		"-loglevel", "error",
		"-y",
		"-i", src,
		"-progress", "pipe:1",
	}

	switch opts.Mode {
	case ModeRemux:
		args = append(args, "-c", "copy")
	case ModeAudio:
		args = append(args, "-vn")
		if opts.AudioCodec != "" {
			args = append(args, "-c:a", opts.AudioCodec)
		} else {
			args = append(args, "-c:a", "copy")
		}
	case ModeEncode:
		args = append(args, "-c:v", opts.VideoCodec)
		if opts.CRF > 0 {
			args = append(args, "-crf", strconv.Itoa(opts.CRF))
		} else if opts.Bitrate != "" {
			args = append(args, "-b:v", opts.Bitrate)
		}
		if opts.Preset != "" {
			args = append(args, "-preset", opts.Preset)
		}
		if opts.AudioCodec != "" {
			args = append(args, "-c:a", opts.AudioCodec)
		} else {
			args = append(args, "-c:a", "copy")
		}
	}

	// mp4 keeps its index at the end of the file by default, which makes a
	// half uploaded file unplayable; moving it to the front costs nothing
	if strings.TrimPrefix(opts.Container, ".") == "mp4" {
		args = append(args, "-movflags", "+faststart")
	}

	args = append(args, opts.ExtraArgs...)
	return append(args, dst)
}

// readProgress parses the key=value stream ffmpeg writes with -progress.
func readProgress(r io.Reader, duration float64, onProgress ProgressFunc) {
	sc := bufio.NewScanner(r)
	var p Progress
	p.Duration = duration

	for sc.Scan() {
		key, value, ok := strings.Cut(strings.TrimSpace(sc.Text()), "=")
		if !ok {
			continue
		}
		switch key {
		case "out_time_us", "out_time_ms":
			// despite the name out_time_ms is microseconds in every ffmpeg
			// build that writes it, which is why both keys are read the same way
			if v, err := strconv.ParseFloat(value, 64); err == nil {
				p.Seconds = v / 1_000_000
			}
		case "total_size":
			if v, err := strconv.ParseInt(value, 10, 64); err == nil {
				p.Bytes = v
			}
		case "speed":
			if v, err := strconv.ParseFloat(strings.TrimSuffix(value, "x"), 64); err == nil {
				p.Speed = v
			}
		case "progress":
			if onProgress != nil {
				onProgress(p)
			}
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
