package extract

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// ErrUnavailable means the video is gone for good: deleted, private, blocked.
var ErrUnavailable = errors.New("video unavailable")

// ErrBlocked means we were turned away: captcha, sign-in wall, 429. Worth
// retrying, but only after a pause or with a different identity.
var ErrBlocked = errors.New("blocked by bot protection")

// Entry is the short record produced by expanding a listing.
type Entry struct {
	ID         string
	URL        string
	Title      string
	Uploader   string
	UploadDate string
	Duration   int64
}

// Listing is an expanded channel, playlist or single video.
type Listing struct {
	ID       string
	Title    string
	Uploader string
	Entries  []Entry
}

type Info struct {
	ID             string
	URL            string
	Title          string
	Uploader       string
	UploadDate     string
	Description    string
	Duration       int64
	FilesizeApprox int64

	// Raw is the untouched metadata snapshot to store next to the file. It is
	// the only place where extractor specific data is allowed; the rest of the
	// codebase only sees the fields above.
	Raw json.RawMessage
}

type Request struct {
	URL       string
	OutputDir string

	// Template is the output filename template in extractor syntax.
	Template string

	Format    string
	Container string

	Fragments    int
	Retries      int
	SleepBetween time.Duration

	// RateLimit caps this download in bytes per second; zero means no limit.
	RateLimit int64

	Subtitles     bool
	SubtitleAuto  bool
	SubtitleLangs []string

	WriteInfoJSON    bool
	WriteDescription bool
	WriteThumbnail   bool
	EmbedMetadata    bool

	CookiesFile        string
	CookiesFromBrowser string
	Proxy              string
	UserAgent          string
}

type Progress struct {
	Stage         string // download or postprocess
	Status        string // downloading, finished, error
	Downloaded    int64
	Total         int64
	Speed         float64 // bytes per second
	ETA           int64   // seconds
	FragmentIndex int
	FragmentCount int
}

// Percent returns completion from 0 to 1, or -1 when the size is unknown.
func (p Progress) Percent() float64 {
	if p.Total <= 0 {
		return -1
	}
	return float64(p.Downloaded) / float64(p.Total)
}

// Result carries what the download produced. The metadata is not decorative:
// a video added from a link list knows neither its title nor its duration
// beforehand, and without them the layout falls back to "unknown" and the
// duration check silently turns itself off.
type Result struct {
	Path     string
	Bytes    int64
	FormatID string

	Title      string
	Uploader   string
	UploadDate string
	Duration   int64
}

// ProgressFunc must be fast and must not block the download.
type ProgressFunc func(Progress)

type Extractor interface {
	Name() string
	Available(ctx context.Context) error

	// Resolve expands a channel, playlist or video URL into entries.
	Resolve(ctx context.Context, url string) (*Listing, error)

	// Info returns details for a single video without downloading it.
	Info(ctx context.Context, url string) (*Info, error)

	// Download fetches a video. Cancelling the context stops it and leaves a
	// partial file that can be resumed.
	Download(ctx context.Context, req Request, onProgress ProgressFunc) (*Result, error)
}
