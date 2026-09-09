package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"

	"gopkg.in/yaml.v3"
)

// ErrPermanent wraps failures that retrying cannot fix: no permission, no
// bucket, a file over the size limit.
var ErrPermanent = errors.New("permanent store failure")

// Asset is one file of a set. The unit of work is a set, not a file: a video
// always travels together with its info.json, thumbnail, description and
// subtitles, otherwise the archive drifts apart.
type Asset struct {
	Path string
	Rel  string // name inside the destination, relative to Prefix
	Size int64
}

// Throttle caps how fast an upload reads its source. It is shared by every
// worker, so the ceiling applies to the tool rather than to each upload.
type Throttle interface {
	Reader(ctx context.Context, r io.Reader) io.Reader
}

type PutRequest struct {
	Prefix string
	Assets []Asset

	// Throttle is optional; nil means the source is read at full speed and
	// no wrapping happens at all.
	Throttle Throttle
}

// Read wraps a reader with the request's throttle, if it has one.
func (r PutRequest) Read(ctx context.Context, src io.Reader) io.Reader {
	if r.Throttle == nil {
		return src
	}
	return r.Throttle.Reader(ctx, src)
}

func (r PutRequest) TotalBytes() int64 {
	var n int64
	for _, a := range r.Assets {
		n += a.Size
	}
	return n
}

type PutResult struct {
	Key   string
	Bytes int64
}

type Progress struct {
	Asset      string
	Uploaded   int64
	Total      int64
	AssetIndex int
	AssetCount int
}

type ProgressFunc func(Progress)

type Uploader interface {
	// Name is the config name, which doubles as the key in the targets table.
	Name() string

	// Kind is the implementation type: local, s3, drive, youtube.
	Kind() string

	Available(ctx context.Context) error

	// Has reports whether the set is already there and how much it weighs.
	// Needed for idempotency: a run interrupted mid-upload must not start over.
	Has(ctx context.Context, prefix string) (bool, int64, error)

	Put(ctx context.Context, req PutRequest, onProgress ProgressFunc) (*PutResult, error)

	Close() error
}

// Factory builds a store from its settings. Options are still undecoded:
// each implementation decodes its own.
type Factory func(name, layout string, options yaml.Node) (Uploader, error)

var registry = map[string]Factory{}

// Register declares a store implementation, called from its init.
func Register(kind string, f Factory) {
	if _, dup := registry[kind]; dup {
		panic("store " + kind + " registered twice")
	}
	registry[kind] = f
}

func Kinds() []string {
	out := make([]string, 0, len(registry))
	for k := range registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func Build(kind, name, layout string, options yaml.Node) (Uploader, error) {
	f, ok := registry[kind]
	if !ok {
		return nil, fmt.Errorf("unknown store type %q for %q: known types are %v", kind, name, Kinds())
	}
	u, err := f(name, layout, options)
	if err != nil {
		return nil, fmt.Errorf("store %q: %w", name, err)
	}
	return u, nil
}
