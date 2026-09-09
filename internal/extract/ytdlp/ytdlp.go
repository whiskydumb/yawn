package ytdlp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/whiskydumb/yawn/internal/extract"
)

// markers that separate progress lines from the rest of the output
const (
	progressMark = "@YAWN@"
	ppMark       = "@YAWNPP@"
	fieldSep     = "|"
)

type Extractor struct {
	binary string

	// ExtraArgs is mixed into every call: custom JS runtime, player client
	// selection, PO tokens.
	ExtraArgs []string

	once   sync.Once
	verErr error
	ver    string

	jsOnce sync.Once
	jsArgs []string
}

// New creates the extractor. An empty binary means "look in PATH".
func New(binary string, extraArgs []string) *Extractor {
	if binary == "" {
		binary = "yt-dlp"
	}
	return &Extractor{binary: binary, ExtraArgs: extraArgs}
}

func (e *Extractor) Name() string {
	if e.ver != "" {
		return "yt-dlp " + e.ver
	}
	return "yt-dlp"
}

func (e *Extractor) Available(ctx context.Context) error {
	e.once.Do(func() {
		out, err := exec.CommandContext(ctx, e.binary, "--version").Output()
		if err != nil {
			e.verErr = fmt.Errorf("cannot run %s: %w (install yt-dlp or set its path in the config)", e.binary, err)
			return
		}
		e.ver = strings.TrimSpace(string(out))
	})
	return e.verErr
}

// baseArgs are shared by every call.
//
// --ignore-config is mandatory: without it the user's global yt-dlp.conf
// silently changes behaviour and runs stop being reproducible.
func (e *Extractor) baseArgs() []string {
	args := []string{"--ignore-config", "--no-colors"}
	args = append(args, e.ExtraArgs...)
	return append(args, e.jsRuntimeArgs()...)
}

// jsRuntimeArgs enables a JavaScript runtime that is present on this machine.
//
// youtube hands out challenges that need one, and yt-dlp only enables deno by
// default. Anything else has to be named explicitly, which would otherwise be
// a line of config every user has to discover the hard way.
func (e *Extractor) jsRuntimeArgs() []string {
	e.jsOnce.Do(func() {
		for _, arg := range e.ExtraArgs {
			// the user has an opinion, so it wins
			if strings.HasPrefix(arg, "--js-runtimes") || arg == "--no-js-runtimes" {
				return
			}
		}
		// deno is on by default, so finding it means there is nothing to add
		if _, err := exec.LookPath("deno"); err == nil {
			return
		}
		for _, name := range []string{"node", "bun", "qjs"} {
			if _, err := exec.LookPath(name); err == nil {
				runtime := name
				if name == "qjs" {
					runtime = "quickjs"
				}
				e.jsArgs = []string{"--js-runtimes", runtime}
				return
			}
		}
	})
	return e.jsArgs
}

type flatEntry struct {
	ID         string      `json:"id"`
	URL        string      `json:"url"`
	WebpageURL string      `json:"webpage_url"`
	Title      string      `json:"title"`
	Uploader   string      `json:"uploader"`
	UploadDate string      `json:"upload_date"`
	Duration   float64     `json:"duration"`
	Type       string      `json:"_type"`
	Entries    []flatEntry `json:"entries"`
}

func (e *Extractor) Resolve(ctx context.Context, url string) (*extract.Listing, error) {
	if err := e.Available(ctx); err != nil {
		return nil, err
	}
	args := append(e.baseArgs(),
		"--dump-single-json",
		"--flat-playlist",
		"--no-warnings",
		"--ignore-errors",
		url,
	)
	cmd := exec.CommandContext(ctx, e.binary, args...)
	configureProcess(cmd)

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, classify(fmt.Errorf("cannot expand %s: %w", url, err), stderr.String())
	}

	var root flatEntry
	if err := json.Unmarshal(out, &root); err != nil {
		return nil, fmt.Errorf("cannot parse the extractor response for %s: %w", url, err)
	}

	listing := &extract.Listing{ID: root.ID, Title: root.Title, Uploader: root.Uploader}
	collect(&root, listing)
	if len(listing.Entries) == 0 {
		return nil, fmt.Errorf("found no videos in %s", url)
	}
	return listing, nil
}

// collect walks nested playlists: a channel has tabs at the top level
// (videos, shorts, streams) and the videos one level down.
func collect(node *flatEntry, out *extract.Listing) {
	if len(node.Entries) == 0 {
		if node.ID == "" || node.Type == "playlist" {
			return
		}
		out.Entries = append(out.Entries, extract.Entry{
			ID:         node.ID,
			URL:        pickURL(node),
			Title:      node.Title,
			Uploader:   node.Uploader,
			UploadDate: node.UploadDate,
			Duration:   int64(node.Duration),
		})
		return
	}
	for i := range node.Entries {
		collect(&node.Entries[i], out)
	}
}

func pickURL(n *flatEntry) string {
	if n.WebpageURL != "" {
		return n.WebpageURL
	}
	if n.URL != "" {
		return n.URL
	}
	return "https://www.youtube.com/watch?v=" + n.ID
}

func (e *Extractor) Info(ctx context.Context, url string) (*extract.Info, error) {
	if err := e.Available(ctx); err != nil {
		return nil, err
	}
	args := append(e.baseArgs(), "--dump-single-json", "--no-warnings", url)
	cmd := exec.CommandContext(ctx, e.binary, args...)
	configureProcess(cmd)

	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, classify(fmt.Errorf("cannot fetch metadata for %s: %w", url, err), stderr.String())
	}

	var raw struct {
		ID             string  `json:"id"`
		WebpageURL     string  `json:"webpage_url"`
		Title          string  `json:"title"`
		Uploader       string  `json:"uploader"`
		UploadDate     string  `json:"upload_date"`
		Description    string  `json:"description"`
		Duration       float64 `json:"duration"`
		FilesizeApprox int64   `json:"filesize_approx"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("cannot parse metadata for %s: %w", url, err)
	}
	return &extract.Info{
		ID:             raw.ID,
		URL:            raw.WebpageURL,
		Title:          raw.Title,
		Uploader:       raw.Uploader,
		UploadDate:     raw.UploadDate,
		Description:    raw.Description,
		Duration:       int64(raw.Duration),
		FilesizeApprox: raw.FilesizeApprox,
		Raw:            json.RawMessage(out),
	}, nil
}

// Download fetches one video. Cancelling the context stops the process and
// leaves a .part file, which the next run resumes with a Range request
// instead of starting over.
func (e *Extractor) Download(ctx context.Context, req extract.Request, onProgress extract.ProgressFunc) (*extract.Result, error) {
	if err := e.Available(ctx); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(req.OutputDir, 0o755); err != nil {
		return nil, fmt.Errorf("cannot create directory %s: %w", req.OutputDir, err)
	}

	resultFile, err := os.CreateTemp("", "yawn-result-*.txt")
	if err != nil {
		return nil, fmt.Errorf("cannot create a temporary result file: %w", err)
	}
	resultPath := resultFile.Name()
	resultFile.Close()
	defer os.Remove(resultPath)

	args := append(e.baseArgs(), e.downloadArgs(req, resultPath)...)
	cmd := exec.CommandContext(ctx, e.binary, args...)
	cmd.Dir = req.OutputDir
	configureProcess(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("cannot start the download of %s: %w", req.URL, err)
	}

	var wg sync.WaitGroup
	var stderrTail tail
	wg.Add(2)
	go func() { defer wg.Done(); readProgress(stdout, onProgress) }()
	go func() { defer wg.Done(); stderrTail.consume(stderrPipe) }()
	wg.Wait()

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if waitErr != nil {
		return nil, classify(fmt.Errorf("download of %s failed: %w", req.URL, waitErr), stderrTail.String())
	}

	res, err := readResult(resultPath)
	if err != nil {
		return nil, fmt.Errorf("download of %s finished but the result was unreadable: %w", req.URL, err)
	}
	return res, nil
}

func (e *Extractor) downloadArgs(req extract.Request, resultPath string) []string {
	template := req.Template
	if template == "" {
		template = "%(upload_date)s - %(title)s [%(id)s].%(ext)s"
	}
	args := []string{
		"-f", req.Format,
		"-o", template,
		"-P", req.OutputDir,
		"--no-playlist",
		"--newline",
		"--no-simulate",
		"--progress",
		"--progress-template", "download:" + progressMark + strings.Join([]string{
			"%(progress.status)s",
			"%(progress.downloaded_bytes)s",
			"%(progress.total_bytes)s",
			"%(progress.total_bytes_estimate)s",
			"%(progress.speed)s",
			"%(progress.eta)s",
			"%(progress.fragment_index)s",
			"%(progress.fragment_count)s",
		}, fieldSep),
		"--progress-template", "postprocess:" + ppMark + "%(progress.status)s",
		// JSON rather than a delimited string: titles contain every character
		// one might pick as a separator.
		"--print-to-file",
		"after_move:%(.{filepath,format_id,filesize,filesize_approx,title,uploader,upload_date,duration})j",
		resultPath,
		"--windows-filenames",
		"--trim-filenames", "180",
		"--no-overwrites",
		"--continue",
	}
	if req.Container != "" && req.Container != "original" {
		args = append(args, "--merge-output-format", req.Container)
	}
	if req.Fragments > 0 {
		args = append(args, "--concurrent-fragments", strconv.Itoa(req.Fragments))
	}
	if req.Retries > 0 {
		args = append(args, "--retries", strconv.Itoa(req.Retries),
			"--fragment-retries", strconv.Itoa(req.Retries*2))
	}
	if req.SleepBetween > 0 {
		args = append(args, "--sleep-requests", formatSeconds(req.SleepBetween))
	}
	if req.RateLimit > 0 {
		args = append(args, "--limit-rate", strconv.FormatInt(req.RateLimit, 10))
	}
	if req.Subtitles && len(req.SubtitleLangs) > 0 {
		args = append(args, "--write-subs", "--sub-langs", strings.Join(req.SubtitleLangs, ","))
		if req.SubtitleAuto {
			args = append(args, "--write-auto-subs")
		}
	}
	if req.WriteInfoJSON {
		args = append(args, "--write-info-json")
	}
	if req.WriteDescription {
		args = append(args, "--write-description")
	}
	if req.WriteThumbnail {
		args = append(args, "--write-thumbnail", "--convert-thumbnails", "jpg")
	}
	if req.EmbedMetadata {
		args = append(args, "--embed-metadata")
	}
	if req.CookiesFile != "" {
		args = append(args, "--cookies", req.CookiesFile)
	}
	if req.CookiesFromBrowser != "" {
		args = append(args, "--cookies-from-browser", req.CookiesFromBrowser)
	}
	if req.Proxy != "" {
		args = append(args, "--proxy", req.Proxy)
	}
	if req.UserAgent != "" {
		args = append(args, "--user-agent", req.UserAgent)
	}
	return append(args, req.URL)
}

func formatSeconds(d time.Duration) string {
	return strconv.FormatFloat(d.Seconds(), 'f', 2, 64)
}

func readProgress(r io.Reader, onProgress extract.ProgressFunc) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, progressMark):
			if onProgress == nil {
				continue
			}
			if p, ok := parseProgress(strings.TrimPrefix(line, progressMark)); ok {
				onProgress(p)
			}
		case strings.HasPrefix(line, ppMark):
			if onProgress == nil {
				continue
			}
			onProgress(extract.Progress{
				Stage:  "postprocess",
				Status: strings.TrimPrefix(line, ppMark),
			})
		}
	}
}

func parseProgress(s string) (extract.Progress, bool) {
	f := strings.Split(s, fieldSep)
	if len(f) < 6 {
		return extract.Progress{}, false
	}
	p := extract.Progress{Stage: "download", Status: f[0]}
	p.Downloaded = parseInt(f[1])
	p.Total = parseInt(f[2])
	if p.Total == 0 {
		p.Total = parseInt(f[3])
	}
	p.Speed = parseFloat(f[4])
	p.ETA = parseInt(f[5])
	if len(f) >= 8 {
		p.FragmentIndex = int(parseInt(f[6]))
		p.FragmentCount = int(parseInt(f[7]))
	}
	return p, true
}

// parseInt is lenient: yt-dlp prints "NA" where a value is unknown.
func parseInt(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return 0
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil {
		return int64(v)
	}
	return 0
}

func parseFloat(s string) float64 {
	s = strings.TrimSpace(s)
	if s == "" || s == "NA" || s == "None" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}

func readResult(path string) (*extract.Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			line = l
		}
	}
	if line == "" {
		return nil, errors.New("the extractor reported no file path")
	}

	var raw struct {
		Filepath       string  `json:"filepath"`
		FormatID       string  `json:"format_id"`
		Filesize       int64   `json:"filesize"`
		FilesizeApprox int64   `json:"filesize_approx"`
		Title          string  `json:"title"`
		Uploader       string  `json:"uploader"`
		UploadDate     string  `json:"upload_date"`
		Duration       float64 `json:"duration"`
	}
	if err := json.Unmarshal([]byte(line), &raw); err != nil {
		return nil, fmt.Errorf("cannot parse the result %q: %w", line, err)
	}
	if raw.Filepath == "" {
		return nil, errors.New("the extractor reported no file path")
	}

	res := &extract.Result{
		Path:       raw.Filepath,
		Bytes:      raw.Filesize,
		FormatID:   raw.FormatID,
		Title:      raw.Title,
		Uploader:   raw.Uploader,
		UploadDate: raw.UploadDate,
		Duration:   int64(raw.Duration),
	}
	if res.Bytes == 0 {
		res.Bytes = raw.FilesizeApprox
	}
	if !filepath.IsAbs(res.Path) {
		if abs, err := filepath.Abs(res.Path); err == nil {
			res.Path = abs
		}
	}
	// the size on disk wins: a merged file does not weigh what the formats promised
	if st, err := os.Stat(res.Path); err == nil {
		res.Bytes = st.Size()
	}
	return res, nil
}

// tail keeps the last stderr lines, enough both for the error message and
// for classification, without growing without bound.
type tail struct {
	mu    sync.Mutex
	lines []string
}

const tailLimit = 40

func (t *tail) consume(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		t.mu.Lock()
		t.lines = append(t.lines, line)
		if len(t.lines) > tailLimit {
			t.lines = t.lines[len(t.lines)-tailLimit:]
		}
		t.mu.Unlock()
	}
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.Join(t.lines, "\n")
}

// unavailableMarks are failures that retrying cannot fix.
var unavailableMarks = []string{
	"video unavailable",
	"private video",
	"has been removed",
	"account associated with this video has been terminated",
	"members-only",
	"join this channel to get access",
	"video is not available",
	"removed by the uploader",
	"this live event has ended",
	"unsupported url",
}

// blockedMarks are bot protection failures: retryable, but only after a
// pause or with a different identity.
var blockedMarks = []string{
	"sign in to confirm you're not a bot",
	"sign in to confirm your age",
	"http error 429",
	"too many requests",
	"confirm you are not a bot",
	"po token",
	"failed to extract any player response",
}

// classify turns a raw process failure into a domain error.
func classify(err error, stderr string) error {
	low := strings.ToLower(stderr)
	for _, m := range unavailableMarks {
		if strings.Contains(low, m) {
			return fmt.Errorf("%w: %v (%s)", extract.ErrUnavailable, err, firstLine(stderr))
		}
	}
	for _, m := range blockedMarks {
		if strings.Contains(low, m) {
			return fmt.Errorf("%w: %v (%s)", extract.ErrBlocked, err, firstLine(stderr))
		}
	}
	if stderr == "" {
		return err
	}
	return fmt.Errorf("%v: %s", err, firstLine(stderr))
}

func firstLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "WARNING:") {
			return l
		}
	}
	return strings.TrimSpace(s)
}
