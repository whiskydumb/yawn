package s3

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/store"
	"gopkg.in/yaml.v3"
)

func init() {
	store.Register("s3", New)
}

type Options struct {
	// Endpoint is the host, with or without a scheme. Empty means AWS S3.
	Endpoint string `yaml:"endpoint"`
	Region   string `yaml:"region"`
	Bucket   string `yaml:"bucket"`

	// Prefix namespaces everything this store writes inside the bucket.
	Prefix string `yaml:"prefix"`

	// Credentials accept a ${VAR} reference; leaving them empty falls back to
	// the standard AWS environment variables.
	AccessKeyID     string `yaml:"access_key_id"`
	SecretAccessKey string `yaml:"secret_access_key"`
	SessionToken    string `yaml:"session_token"`

	// UseSSL defaults to true; only set it false for a local MinIO.
	UseSSL *bool `yaml:"use_ssl"`

	// PartSize is the multipart chunk size, e.g. "64M". Zero lets the client
	// choose.
	PartSize string `yaml:"part_size"`

	StorageClass string `yaml:"storage_class"`
}

type Store struct {
	name   string
	layout string
	bucket string
	prefix string

	client       *minio.Client
	partSize     uint64
	storageClass string
}

// minPartSize is the smallest multipart chunk S3 accepts.
const minPartSize = 5 << 20

func New(name, layout string, options yaml.Node) (store.Uploader, error) {
	var opts Options
	if !options.IsZero() {
		if err := options.Decode(&opts); err != nil {
			return nil, fmt.Errorf("cannot parse options: %w", err)
		}
	}
	if opts.Bucket == "" {
		return nil, errors.New("options.bucket is not set")
	}

	accessKey := config.ResolveSecret(opts.AccessKeyID, "AWS_ACCESS_KEY_ID", "S3_ACCESS_KEY_ID")
	secretKey := config.ResolveSecret(opts.SecretAccessKey, "AWS_SECRET_ACCESS_KEY", "S3_SECRET_ACCESS_KEY")
	sessionToken := config.ResolveSecret(opts.SessionToken, "AWS_SESSION_TOKEN")
	if accessKey == "" || secretKey == "" {
		return nil, errors.New("no credentials: set options.access_key_id and options.secret_access_key, " +
			"or the AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY environment variables")
	}

	endpoint, secure := splitEndpoint(opts.Endpoint)
	if opts.UseSSL != nil {
		secure = *opts.UseSSL
	}

	partSize, err := config.ParseSize(opts.PartSize)
	if err != nil {
		return nil, fmt.Errorf("options.part_size: %w", err)
	}
	if partSize > 0 && partSize < minPartSize {
		return nil, fmt.Errorf("options.part_size is %s, S3 requires at least %s",
			partSize, config.Size(minPartSize))
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:     credentials.NewStaticV4(accessKey, secretKey, sessionToken),
		Secure:    secure,
		Region:    opts.Region,
		Transport: newTransport(),
	})
	if err != nil {
		return nil, fmt.Errorf("cannot build the client: %w", err)
	}

	return &Store{
		name:         name,
		layout:       layout,
		bucket:       opts.Bucket,
		prefix:       strings.Trim(opts.Prefix, "/"),
		client:       client,
		partSize:     uint64(partSize),
		storageClass: opts.StorageClass,
	}, nil
}

// splitEndpoint accepts a host with or without a scheme and reports whether
// TLS should be used. An empty endpoint means AWS S3 itself.
func splitEndpoint(raw string) (host string, secure bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "s3.amazonaws.com", true
	}
	switch {
	case strings.HasPrefix(raw, "https://"):
		return strings.TrimSuffix(strings.TrimPrefix(raw, "https://"), "/"), true
	case strings.HasPrefix(raw, "http://"):
		return strings.TrimSuffix(strings.TrimPrefix(raw, "http://"), "/"), false
	default:
		return strings.TrimSuffix(raw, "/"), true
	}
}

func newTransport() http.RoundTripper {
	t, err := minio.DefaultTransport(true)
	if err != nil {
		return http.DefaultTransport
	}
	// uploads are long; a short response timeout would kill a large part
	t.ResponseHeaderTimeout = 2 * time.Minute
	return t
}

func (s *Store) Name() string   { return s.name }
func (s *Store) Kind() string   { return "s3" }
func (s *Store) Close() error   { return nil }
func (s *Store) Layout() string { return s.layout }

// key builds the object key for a path inside the store.
func (s *Store) key(parts ...string) string {
	segments := make([]string, 0, len(parts)+1)
	if s.prefix != "" {
		segments = append(segments, s.prefix)
	}
	for _, p := range parts {
		if p = strings.Trim(p, "/"); p != "" {
			segments = append(segments, p)
		}
	}
	return path.Join(segments...)
}

func (s *Store) Available(ctx context.Context) error {
	ok, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("%w: cannot reach bucket %s: %v", store.ErrPermanent, s.bucket, err)
	}
	if !ok {
		return fmt.Errorf("%w: bucket %s does not exist or is not visible to these credentials",
			store.ErrPermanent, s.bucket)
	}
	return nil
}

func (s *Store) Has(ctx context.Context, prefix string) (bool, int64, error) {
	listPrefix := s.key(prefix) + "/"
	var total int64
	found := false

	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{
		Prefix:    listPrefix,
		Recursive: true,
	}) {
		if obj.Err != nil {
			return false, 0, obj.Err
		}
		found = true
		total += obj.Size
	}
	return found, total, nil
}

func (s *Store) Put(ctx context.Context, req store.PutRequest, onProgress store.ProgressFunc) (*store.PutResult, error) {
	total := req.TotalBytes()
	var written int64

	for i, asset := range req.Assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := s.key(req.Prefix, asset.Rel)

		// already there at the same size: an interrupted run must not resend
		if stat, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
			if stat.Size == asset.Size && asset.Size > 0 {
				written += asset.Size
				report(onProgress, asset, written, total, i, len(req.Assets))
				continue
			}
		}

		n, err := s.putOne(ctx, key, asset, req.Read, func(delta int64) {
			written += delta
			report(onProgress, asset, written, total, i, len(req.Assets))
		})
		if err != nil {
			return nil, classify(fmt.Errorf("cannot upload %s: %w", asset.Rel, err))
		}
		_ = n
	}

	return &store.PutResult{
		Key:   fmt.Sprintf("s3://%s/%s", s.bucket, s.key(req.Prefix)),
		Bytes: written,
	}, nil
}

func (s *Store) putOne(ctx context.Context, key string, asset store.Asset,
	throttle func(context.Context, io.Reader) io.Reader, onDelta func(int64)) (int64, error) {

	f, err := os.Open(asset.Path)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	opts := minio.PutObjectOptions{
		ContentType:  contentType(asset.Rel),
		StorageClass: s.storageClass,
	}
	if s.partSize > 0 {
		opts.PartSize = s.partSize
	}

	body := throttle(ctx, &countingReader{r: f, onDelta: onDelta})
	info, err := s.client.PutObject(ctx, s.bucket, key, body, asset.Size, opts)
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

// countingReader reports progress as the upload consumes the file.
type countingReader struct {
	r       io.Reader
	onDelta func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.onDelta != nil {
		c.onDelta(int64(n))
	}
	return n, err
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

func contentType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".mp4":
		return "video/mp4"
	case ".mkv":
		return "video/x-matroska"
	case ".webm":
		return "video/webm"
	case ".m4a":
		return "audio/mp4"
	case ".opus":
		return "audio/opus"
	case ".mp3":
		return "audio/mpeg"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".png":
		return "image/png"
	case ".webp":
		return "image/webp"
	case ".json":
		return "application/json"
	case ".vtt":
		return "text/vtt"
	case ".srt":
		return "application/x-subrip"
	case ".description", ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// permanentCodes are S3 errors that retrying cannot fix.
var permanentCodes = map[string]struct{}{
	"AccessDenied":          {},
	"NoSuchBucket":          {},
	"InvalidAccessKeyId":    {},
	"SignatureDoesNotMatch": {},
	"EntityTooLarge":        {},
	"InvalidBucketName":     {},
	"MethodNotAllowed":      {},
}

func classify(err error) error {
	var resp minio.ErrorResponse
	if errors.As(err, &resp) {
		if _, permanent := permanentCodes[resp.Code]; permanent {
			return fmt.Errorf("%w: %v", store.ErrPermanent, err)
		}
	}
	return err
}
