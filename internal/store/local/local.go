package local

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/diskspace"
	"github.com/whiskydumb/yawn/internal/store"
	"gopkg.in/yaml.v3"
)

func init() {
	store.Register("local", New)
}

type Options struct {
	Path string `yaml:"path"`

	// MinFree refuses uploads once the destination runs low, e.g. "5G".
	MinFree string `yaml:"min_free"`
}

type Store struct {
	name    string
	layout  string
	root    string
	minFree int64
}

func New(name, layout string, options yaml.Node) (store.Uploader, error) {
	var opts Options
	if !options.IsZero() {
		if err := options.Decode(&opts); err != nil {
			return nil, fmt.Errorf("cannot parse options: %w", err)
		}
	}
	if opts.Path == "" {
		return nil, errors.New("options.path is not set: nowhere to put the archive")
	}
	root, err := filepath.Abs(opts.Path)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve path %q: %w", opts.Path, err)
	}
	minFree, err := config.ParseSize(opts.MinFree)
	if err != nil {
		return nil, fmt.Errorf("options.min_free: %w", err)
	}
	return &Store{name: name, layout: layout, root: root, minFree: minFree.Bytes()}, nil
}

func (s *Store) Name() string   { return s.name }
func (s *Store) Kind() string   { return "local" }
func (s *Store) Close() error   { return nil }
func (s *Store) Layout() string { return s.layout }
func (s *Store) Root() string   { return s.root }

// checkFree refuses to write once the destination volume runs low. The error
// is deliberately permanent: filling an archive disk is not a failure worth
// retrying, since retries would only eat the remainder.
func (s *Store) checkFree() error {
	if s.minFree <= 0 {
		return nil
	}
	avail, err := diskspace.Free(s.root)
	if err != nil {
		// if the system will not answer, that is no reason to refuse the upload
		return nil
	}
	if avail < s.minFree {
		return fmt.Errorf("%w: %s has %d bytes free, below the %d threshold",
			store.ErrPermanent, s.root, avail, s.minFree)
	}
	return nil
}

func (s *Store) Available(ctx context.Context) error {
	if err := os.MkdirAll(s.root, 0o755); err != nil {
		return fmt.Errorf("%w: cannot create directory %s: %v", store.ErrPermanent, s.root, err)
	}
	probe := filepath.Join(s.root, ".yawn-write-test")
	if err := os.WriteFile(probe, []byte("ok"), 0o644); err != nil {
		return fmt.Errorf("%w: directory %s is not writable: %v", store.ErrPermanent, s.root, err)
	}
	return os.Remove(probe)
}

func (s *Store) Has(ctx context.Context, prefix string) (bool, int64, error) {
	dir := filepath.Join(s.root, filepath.FromSlash(prefix))
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return false, 0, nil
		}
		return false, 0, err
	}
	var total int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		total += info.Size()
	}
	return len(entries) > 0, total, nil
}

// Put copies the set into the destination. Every file is written to a
// temporary name next to its target and renamed on success, so an interrupt
// never leaves a half file that the next run would mistake for a finished one.
func (s *Store) Put(ctx context.Context, req store.PutRequest, onProgress store.ProgressFunc) (*store.PutResult, error) {
	if err := s.checkFree(); err != nil {
		return nil, err
	}
	dir := filepath.Join(s.root, filepath.FromSlash(req.Prefix))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("%w: cannot create %s: %v", store.ErrPermanent, dir, err)
	}

	total := req.TotalBytes()
	var written int64

	for i, asset := range req.Assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		dst := filepath.Join(dir, filepath.FromSlash(asset.Rel))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, fmt.Errorf("cannot create %s: %w", filepath.Dir(dst), err)
		}

		// already there at the same size, no need to copy again
		if st, err := os.Stat(dst); err == nil && st.Size() == asset.Size && asset.Size > 0 {
			written += asset.Size
			report(onProgress, asset, written, total, i, len(req.Assets))
			continue
		}

		if _, err := copyFile(ctx, asset.Path, dst, req.Read, func(delta int64) {
			written += delta
			report(onProgress, asset, written, total, i, len(req.Assets))
		}); err != nil {
			return nil, fmt.Errorf("cannot copy %s: %w", asset.Rel, err)
		}
	}

	return &store.PutResult{Key: dir, Bytes: written}, nil
}

func report(f store.ProgressFunc, a store.Asset, done, total int64, idx, count int) {
	if f == nil {
		return
	}
	f(store.Progress{
		Asset: a.Rel, Uploaded: done, Total: total,
		AssetIndex: idx + 1, AssetCount: count,
	})
}

const copyChunk = 1 << 20

func copyFile(ctx context.Context, src, dst string,
	throttle func(context.Context, io.Reader) io.Reader, onDelta func(int64)) (int64, error) {

	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	in := throttle(ctx, f)

	tmp := dst + ".yawn-part"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	// on any exit but success the temporary file goes away, otherwise the
	// destination collects debris after every interrupt
	success := false
	defer func() {
		out.Close()
		if !success {
			os.Remove(tmp)
		}
	}()

	buf := make([]byte, copyChunk)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := in.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return total, werr
			}
			total += int64(n)
			if onDelta != nil {
				onDelta(int64(n))
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return total, readErr
		}
	}
	if err := out.Sync(); err != nil {
		return total, err
	}
	if err := out.Close(); err != nil {
		return total, err
	}
	if err := os.Rename(tmp, dst); err != nil {
		return total, err
	}
	success = true
	return total, nil
}
