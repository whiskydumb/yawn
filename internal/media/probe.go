package media

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// ErrCorrupt means the file does not read back or is noticeably short.
var ErrCorrupt = errors.New("file is corrupt or truncated")

type Prober struct {
	binary string
}

// NewProber wraps ffprobe. An empty binary means "look in PATH".
func NewProber(binary string) *Prober {
	if binary == "" {
		binary = "ffprobe"
	}
	return &Prober{binary: binary}
}

func (p *Prober) Available(ctx context.Context) error {
	if _, err := exec.CommandContext(ctx, p.binary, "-version").Output(); err != nil {
		return fmt.Errorf("cannot run %s: %w (install ffmpeg)", p.binary, err)
	}
	return nil
}

type Probe struct {
	Duration float64
	Bytes    int64
	Video    bool
	Audio    bool
	VCodec   string
	ACodec   string
	Width    int
	Height   int
}

func (p *Prober) Probe(ctx context.Context, path string) (*Probe, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("cannot find file %s: %w", path, err)
	}
	out, err := exec.CommandContext(ctx, p.binary,
		"-v", "error",
		"-show_entries", "format=duration",
		"-show_entries", "stream=codec_type,codec_name,width,height",
		"-of", "json",
		path,
	).Output()
	if err != nil {
		return nil, fmt.Errorf("%w: ffprobe could not read %s: %v", ErrCorrupt, path, err)
	}

	var raw struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("cannot parse the ffprobe output for %s: %w", path, err)
	}

	res := &Probe{Bytes: st.Size()}
	res.Duration, _ = strconv.ParseFloat(strings.TrimSpace(raw.Format.Duration), 64)
	for _, s := range raw.Streams {
		switch s.CodecType {
		case "video":
			res.Video = true
			res.VCodec = s.CodecName
			if s.Width > res.Width {
				res.Width, res.Height = s.Width, s.Height
			}
		case "audio":
			res.Audio = true
			res.ACodec = s.CodecName
		}
	}
	return res, nil
}

// DurationTolerance is the allowed relative drift in duration.
const DurationTolerance = 0.05

// DurationFloor is the allowed drift in seconds regardless of length.
//
// A relative tolerance alone is not enough. YouTube reports whole seconds and
// muxing adds fractions, so a seven second clip measuring 7.4s drifts by 5.7%
// and would be wrongly called truncated. The threshold is the larger of the two.
const DurationFloor = 2.0

// Verify checks that a downloaded file is intact. A zero expectedDuration
// skips the duration comparison.
func (p *Prober) Verify(ctx context.Context, path string, expectedDuration int64) (*Probe, error) {
	probe, err := p.Probe(ctx, path)
	if err != nil {
		return nil, err
	}
	if probe.Bytes == 0 {
		return probe, fmt.Errorf("%w: %s is empty", ErrCorrupt, path)
	}
	if !probe.Video && !probe.Audio {
		return probe, fmt.Errorf("%w: %s has neither video nor audio", ErrCorrupt, path)
	}
	if expectedDuration <= 0 || probe.Duration <= 0 {
		return probe, nil
	}
	want := float64(expectedDuration)
	drift := math.Abs(probe.Duration - want)
	allowed := math.Max(want*DurationTolerance, DurationFloor)
	if drift > allowed {
		return probe, fmt.Errorf("%w: %s runs %.1fs instead of %.1fs (drift %.1fs, allowed %.1f)",
			ErrCorrupt, path, probe.Duration, want, drift, allowed)
	}
	return probe, nil
}
