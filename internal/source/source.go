package source

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"strings"

	"github.com/whiskydumb/yawn/internal/queue"
)

const idLength = 11

type Input struct {
	Kind queue.SourceKind
	URL  string

	// FromList marks entries that came from a link list file.
	FromList string
}

// Parse reads one argument: a URL, a bare video id, or a path to a list file.
func Parse(arg string) ([]Input, error) {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return nil, fmt.Errorf("empty argument")
	}
	if isFile(arg) {
		urls, err := ReadList(arg)
		if err != nil {
			return nil, err
		}
		out := make([]Input, 0, len(urls))
		for _, u := range urls {
			out = append(out, Input{Kind: queue.SourceVideo, URL: u, FromList: arg})
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("found no links in %s", arg)
		}
		return out, nil
	}
	return []Input{{Kind: Kind(arg), URL: Normalize(arg)}}, nil
}

func isFile(s string) bool {
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		return false
	}
	st, err := os.Stat(s)
	return err == nil && !st.IsDir()
}

func Kind(raw string) queue.SourceKind {
	low := strings.ToLower(raw)
	switch {
	case strings.Contains(low, "/playlist?") || strings.Contains(low, "list="):
		return queue.SourcePlaylist
	case strings.Contains(low, "/channel/"), strings.Contains(low, "/c/"),
		strings.Contains(low, "/user/"), strings.Contains(low, "/@"),
		strings.HasPrefix(low, "@"):
		return queue.SourceChannel
	default:
		return queue.SourceVideo
	}
}

// Normalize turns an argument into a URL the extractor understands.
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "@") {
		return "https://www.youtube.com/" + raw
	}
	if isBareID(raw) {
		return "https://www.youtube.com/watch?v=" + raw
	}
	if !strings.Contains(raw, "://") {
		return "https://" + raw
	}
	return raw
}

func isBareID(s string) bool {
	if len(s) != idLength {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
		default:
			return false
		}
	}
	return true
}

// ExtractID pulls the video id out of a link, or returns "" if the link does
// not point at a single video.
func ExtractID(raw string) string {
	raw = strings.TrimSpace(raw)
	if isBareID(raw) {
		return raw
	}
	u, err := url.Parse(Normalize(raw))
	if err != nil {
		return ""
	}
	if v := u.Query().Get("v"); isBareID(v) {
		return v
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	for i, p := range parts {
		switch p {
		case "shorts", "embed", "live", "v":
			if i+1 < len(parts) && isBareID(parts[i+1]) {
				return parts[i+1]
			}
		}
	}
	if strings.HasSuffix(u.Host, "youtu.be") && len(parts) > 0 && isBareID(parts[0]) {
		return parts[0]
	}
	return ""
}

// ReadList reads a link list: one link per line, everything after a hash is
// a comment, blank lines are skipped, duplicates are dropped in place.
func ReadList(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot open list %s: %w", path, err)
	}
	defer f.Close()

	var out []string
	seen := make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if i := strings.Index(line, "#"); i > 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		normalized := Normalize(line)
		key := normalized
		if id := ExtractID(line); id != "" {
			key = id
			normalized = "https://www.youtube.com/watch?v=" + id
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, normalized)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("cannot finish reading list %s: %w", path, err)
	}
	return out, nil
}
