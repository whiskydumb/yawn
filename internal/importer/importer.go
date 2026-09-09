package importer

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// idPattern matches the "[VIDEOID]" that yt-dlp puts in a filename.
var idPattern = regexp.MustCompile(`\[([A-Za-z0-9_-]{11})\]`)

// mediaExtensions are the files treated as the video itself.
var mediaExtensions = map[string]bool{
	".mp4": true, ".mkv": true, ".webm": true, ".m4a": true,
	".mp3": true, ".opus": true, ".flv": true, ".mov": true, ".avi": true,
}

// Found is one video discovered on disk.
type Found struct {
	VideoID string
	Path    string
	Bytes   int64

	Title      string
	Uploader   string
	UploadDate string
	Duration   int64

	// InfoJSON is where the metadata came from, empty when none was found.
	InfoJSON string
}

// Problem is a file that could not be imported and why.
type Problem struct {
	Path   string
	Reason string
}

// Result is what a scan turned up.
type Result struct {
	Found    []Found
	Problems []Problem
	Bytes    int64
}

// Scan walks root and collects every video it can identify.
//
// @param root - directory to walk
// @param metaDir - optional tree searched for metadata when a video has no
//
//	sidecar next to it, which is how an archive that keeps its .info.json
//	files apart still gets titles and durations
func Scan(ctx context.Context, root, metaDir string) (*Result, error) {
	var meta map[string]string
	if metaDir != "" {
		var err error
		meta, err = indexMetadata(ctx, metaDir)
		if err != nil {
			return nil, err
		}
	}

	res := &Result{}
	seen := make(map[string]string)

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			res.Problems = append(res.Problems, Problem{Path: path, Reason: err.Error()})
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if !mediaExtensions[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		// a leftover from an interrupted download is not an archived video
		if strings.Contains(name, ".part") || strings.Contains(name, ".yawn-") {
			return nil
		}

		id := extractID(path)
		if id == "" {
			res.Problems = append(res.Problems, Problem{
				Path:   path,
				Reason: "no video id in the name; expected something like [dQw4w9WgXcQ]",
			})
			return nil
		}
		if first, dup := seen[id]; dup {
			res.Problems = append(res.Problems, Problem{
				Path:   path,
				Reason: "same video id as " + first,
			})
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			res.Problems = append(res.Problems, Problem{Path: path, Reason: statErr.Error()})
			return nil
		}
		if info.Size() == 0 {
			res.Problems = append(res.Problems, Problem{Path: path, Reason: "empty file"})
			return nil
		}

		seen[id] = path
		found := Found{VideoID: id, Path: path, Bytes: info.Size()}
		applyMetadata(&found, meta)
		res.Found = append(res.Found, found)
		res.Bytes += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// extractID looks for the id in the file name and then in its directory, which
// covers both "title [id].mp4" and "date - title [id]/title.mp4".
func extractID(path string) string {
	if m := idPattern.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	if m := idPattern.FindStringSubmatch(filepath.Base(filepath.Dir(path))); m != nil {
		return m[1]
	}
	return ""
}

// applyMetadata fills in what a sidecar knows, preferring one next to the
// video and falling back to the separate index.
func applyMetadata(f *Found, meta map[string]string) {
	sidecar := strings.TrimSuffix(f.Path, filepath.Ext(f.Path)) + ".info.json"
	if _, err := os.Stat(sidecar); err == nil {
		if readInfo(sidecar, f) {
			f.InfoJSON = sidecar
			return
		}
	}
	if path, ok := meta[f.VideoID]; ok && readInfo(path, f) {
		f.InfoJSON = path
	}
}

type infoJSON struct {
	ID         string  `json:"id"`
	Title      string  `json:"title"`
	Uploader   string  `json:"uploader"`
	UploadDate string  `json:"upload_date"`
	Duration   float64 `json:"duration"`
}

func readInfo(path string, f *Found) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var raw infoJSON
	if err := json.Unmarshal(data, &raw); err != nil {
		return false
	}
	f.Title = raw.Title
	f.Uploader = raw.Uploader
	f.UploadDate = raw.UploadDate
	f.Duration = int64(raw.Duration)
	return true
}

// indexMetadata maps video ids to the .info.json files under dir.
func indexMetadata(ctx context.Context, dir string) (map[string]string, error) {
	index := make(map[string]string)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // an unreadable corner of the metadata tree is not fatal
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".info.json") {
			return nil
		}
		if id := extractID(path); id != "" {
			index[id] = path
			return nil
		}
		// the name may carry no id, so fall back to reading the file
		var f Found
		if readInfo(path, &f) {
			var raw infoJSON
			if data, readErr := os.ReadFile(path); readErr == nil {
				if json.Unmarshal(data, &raw) == nil && raw.ID != "" {
					index[raw.ID] = path
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cannot index metadata in %s: %w", dir, err)
	}
	return index, nil
}
