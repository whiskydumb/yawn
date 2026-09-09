package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const DefaultFileName = "yawn.yaml"

// DefaultLayout is used when a store does not define its own.
const DefaultLayout = "{uploader}/{upload_year}/{upload_date} - {title} [{id}]"

type Config struct {
	WorkDir  string `yaml:"work_dir"`
	Database string `yaml:"database"`

	Extractor Extractor     `yaml:"extractor"`
	Download  Download      `yaml:"download"`
	Convert   Convert       `yaml:"convert"`
	Upload    Upload        `yaml:"upload"`
	Identity  Identity      `yaml:"identity"`
	Stores    []StoreConfig `yaml:"stores"`
}

// Extractor locates external tools. Empty values mean "look in PATH".
type Extractor struct {
	Binary  string `yaml:"binary"`
	FFmpeg  string `yaml:"ffmpeg"`
	FFprobe string `yaml:"ffprobe"`

	// ExtraArgs is passed to every extractor call: custom JS runtime,
	// player client selection, PO tokens.
	ExtraArgs []string `yaml:"extra_args"`
}

type Download struct {
	Workers int `yaml:"workers"`

	// Quality is a ceiling, not a requirement: "1080", "720", "best", "audio".
	Quality string `yaml:"quality"`

	// Format overrides Quality with a raw format selector.
	Format string `yaml:"format"`

	// PreferDirect favours plain https streams over HLS: smaller files that
	// resume with a Range request.
	PreferDirect bool `yaml:"prefer_direct"`

	Container string `yaml:"container"`
	Fragments int    `yaml:"fragments"`

	// RateLimit caps the whole tool, not each worker.
	RateLimit Size `yaml:"rate_limit"`

	// MaxDisk caps the total weight of files in flight. This is what lets
	// yawn mirror an archive larger than the disk.
	MaxDisk Size `yaml:"max_disk"`

	// MinFree stops new downloads when the volume runs low.
	MinFree Size `yaml:"min_free"`

	Retries       int     `yaml:"retries"`
	SleepRequests float64 `yaml:"sleep_requests"`

	Subtitles Subtitles `yaml:"subtitles"`
	Metadata  Metadata  `yaml:"metadata"`
}

// Convert is an optional ffmpeg pass between download and upload.
//
// Remuxing is cheap and lossless. Re-encoding is not: a few hundred hours of
// video is days of CPU, so it is off by default and the config says so.
type Convert struct {
	Enabled bool `yaml:"enabled"`

	// Mode is remux, encode or audio.
	Mode string `yaml:"mode"`

	Container  string `yaml:"container"`
	VideoCodec string `yaml:"video_codec"`
	AudioCodec string `yaml:"audio_codec"`

	CRF     int    `yaml:"crf"`
	Preset  string `yaml:"preset"`
	Bitrate string `yaml:"bitrate"`

	ExtraArgs []string `yaml:"extra_args"`
}

type Subtitles struct {
	Enabled bool     `yaml:"enabled"`
	Auto    bool     `yaml:"auto"`
	Langs   []string `yaml:"langs"`
}

type Metadata struct {
	InfoJSON    bool `yaml:"info_json"`
	Description bool `yaml:"description"`
	Thumbnail   bool `yaml:"thumbnail"`
	Embed       bool `yaml:"embed"`
}

type Upload struct {
	Workers   int  `yaml:"workers"`
	RateLimit Size `yaml:"rate_limit"`

	// Verify probes the file before upload. Without it a truncated download
	// reaches the archive looking valid.
	Verify bool `yaml:"verify"`

	// DeleteAfter removes the local copy once every store has it.
	DeleteAfter bool `yaml:"delete_after"`

	Retries int `yaml:"retries"`
}

// Identity is how we present ourselves to the site.
//
// The flat fields describe one identity and cover the common case. Pool
// replaces them with several that are rotated, rested when refused, and
// retired when they stop working.
type Identity struct {
	CookiesFile        string `yaml:"cookies_file"`
	CookiesFromBrowser string `yaml:"cookies_from_browser"`
	Proxy              string `yaml:"proxy"`
	UserAgent          string `yaml:"user_agent"`

	Pool []PoolIdentity `yaml:"pool"`

	// Backoff tunes how long a refused identity rests.
	BaseBackoff Duration `yaml:"base_backoff"`
	MaxBackoff  Duration `yaml:"max_backoff"`

	// MaxStrikes is how many refusals in a row retire an identity for the run.
	MaxStrikes int `yaml:"max_strikes"`
}

// PoolIdentity is one entry of the rotation.
type PoolIdentity struct {
	Name               string `yaml:"name"`
	CookiesFile        string `yaml:"cookies_file"`
	CookiesFromBrowser string `yaml:"cookies_from_browser"`
	Proxy              string `yaml:"proxy"`
	UserAgent          string `yaml:"user_agent"`
}

// Identities returns the pool, falling back to the flat fields so that a
// simple config keeps working untouched.
func (i Identity) Identities() []PoolIdentity {
	if len(i.Pool) > 0 {
		return i.Pool
	}
	if i.CookiesFile == "" && i.CookiesFromBrowser == "" && i.Proxy == "" && i.UserAgent == "" {
		return nil
	}
	return []PoolIdentity{{
		Name:               "default",
		CookiesFile:        i.CookiesFile,
		CookiesFromBrowser: i.CookiesFromBrowser,
		Proxy:              i.Proxy,
		UserAgent:          i.UserAgent,
	}}
}

// StoreConfig describes one store. Options are decoded by the implementation
// they belong to.
type StoreConfig struct {
	Name    string    `yaml:"name"`
	Type    string    `yaml:"type"`
	Layout  string    `yaml:"layout"`
	Enabled *bool     `yaml:"enabled"`
	Options yaml.Node `yaml:"options"`
}

// IsEnabled treats a missing field as enabled.
func (s StoreConfig) IsEnabled() bool { return s.Enabled == nil || *s.Enabled }

// Default returns a config that works without any edits.
func Default() Config {
	return Config{
		WorkDir:  "downloads",
		Database: "yawn.db",
		Download: Download{
			Workers:       2,
			Quality:       "1080",
			PreferDirect:  true,
			Container:     "mp4",
			Fragments:     4,
			MaxDisk:       Size(20) << 30,
			MinFree:       Size(10) << 30,
			Retries:       5,
			SleepRequests: 1.5,
			Subtitles:     Subtitles{Enabled: true, Auto: true, Langs: []string{"en"}},
			Metadata:      Metadata{InfoJSON: true, Description: true, Thumbnail: true, Embed: true},
		},
		Upload: Upload{
			Workers:     4,
			Verify:      true,
			DeleteAfter: true,
			Retries:     5,
		},
	}
}

// Load reads a config file on top of the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	if path == "" {
		path = DefaultFileName
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, fmt.Errorf("no config at %s: create one with `yawn init`", path)
		}
		return cfg, fmt.Errorf("cannot read config %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return cfg, fmt.Errorf("cannot parse config %s: %w", path, err)
	}
	if err := cfg.normalize(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c *Config) normalize() error {
	if c.WorkDir == "" {
		c.WorkDir = "downloads"
	}
	abs, err := filepath.Abs(c.WorkDir)
	if err != nil {
		return fmt.Errorf("cannot resolve work_dir %q: %w", c.WorkDir, err)
	}
	c.WorkDir = abs

	if c.Database == "" {
		c.Database = "yawn.db"
	}
	if c.Download.Workers < 1 {
		c.Download.Workers = 1
	}
	if c.Upload.Workers < 1 {
		c.Upload.Workers = 1
	}
	if c.Download.Fragments < 1 {
		c.Download.Fragments = 1
	}
	if c.Download.Quality == "" {
		c.Download.Quality = "1080"
	}

	seen := make(map[string]struct{}, len(c.Stores))
	for i, s := range c.Stores {
		if s.Name == "" {
			return fmt.Errorf("stores[%d]: empty name", i)
		}
		if s.Type == "" {
			return fmt.Errorf("store %q: no type given", s.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return fmt.Errorf("store %q is declared twice", s.Name)
		}
		seen[s.Name] = struct{}{}
		if s.Layout == "" {
			c.Stores[i].Layout = DefaultLayout
		}
	}
	if _, err := c.Download.FormatSelector(); err != nil {
		return err
	}
	if c.Convert.Enabled && c.Convert.Mode == "" {
		c.Convert.Mode = "remux"
	}
	return nil
}

// FormatSelector builds a yt-dlp format selector from the quality settings.
func (d Download) FormatSelector() (string, error) {
	if d.Format != "" {
		return d.Format, nil
	}
	q := strings.ToLower(strings.TrimSpace(d.Quality))
	switch q {
	case "audio":
		return "ba/b", nil
	case "best", "max":
		if d.PreferDirect {
			return "bv*[protocol=https]+ba[protocol=https]/bv*+ba/b", nil
		}
		return "bv*+ba/b", nil
	}
	q = strings.TrimSuffix(q, "p")
	height, err := strconv.Atoi(q)
	if err != nil || height <= 0 {
		return "", fmt.Errorf("quality %q: expected a number (360, 720, 1080), best, or audio", d.Quality)
	}
	if d.PreferDirect {
		return fmt.Sprintf(
			"bv*[height<=%[1]d][protocol=https]+ba[protocol=https]/bv*[height<=%[1]d]+ba/b[height<=%[1]d]/b",
			height), nil
	}
	return fmt.Sprintf("bv*[height<=%[1]d]+ba/b[height<=%[1]d]/b", height), nil
}
