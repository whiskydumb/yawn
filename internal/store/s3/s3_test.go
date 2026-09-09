package s3

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/whiskydumb/yawn/internal/store"
	"gopkg.in/yaml.v3"
)

func TestSplitEndpoint(t *testing.T) {
	cases := []struct {
		in         string
		wantHost   string
		wantSecure bool
	}{
		{"", "s3.amazonaws.com", true},
		{"https://abc.r2.cloudflarestorage.com", "abc.r2.cloudflarestorage.com", true},
		{"http://localhost:9000", "localhost:9000", false},
		{"s3.eu-central-1.wasabisys.com", "s3.eu-central-1.wasabisys.com", true},
		{"https://example.com/", "example.com", true},
	}
	for _, c := range cases {
		host, secure := splitEndpoint(c.in)
		if host != c.wantHost || secure != c.wantSecure {
			t.Errorf("splitEndpoint(%q) = (%q, %v), want (%q, %v)",
				c.in, host, secure, c.wantHost, c.wantSecure)
		}
	}
}

func TestKeyJoinsPrefix(t *testing.T) {
	s := &Store{prefix: "archive"}
	if got := s.key("2014/video [abc]", "video.mp4"); got != "archive/2014/video [abc]/video.mp4" {
		t.Errorf("got %q", got)
	}

	bare := &Store{}
	if got := bare.key("2014/video", "video.mp4"); got != "2014/video/video.mp4" {
		t.Errorf("got %q with no prefix", got)
	}
	if got := bare.key("", "video.mp4"); got != "video.mp4" {
		t.Errorf("got %q with an empty segment", got)
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"a.mp4":         "video/mp4",
		"a.info.json":   "application/json",
		"a.jpg":         "image/jpeg",
		"a.ru.vtt":      "text/vtt",
		"a.description": "text/plain; charset=utf-8",
		"a.unknown":     "application/octet-stream",
	}
	for name, want := range cases {
		if got := contentType(name); got != want {
			t.Errorf("contentType(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestNewRequiresBucket(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	if _, err := New("test", "{id}", node(t, "endpoint: localhost:9000")); err == nil {
		t.Error("a store without options.bucket must be rejected")
	}
}

func TestNewRequiresCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "")
	t.Setenv("S3_ACCESS_KEY_ID", "")
	t.Setenv("S3_SECRET_ACCESS_KEY", "")
	_, err := New("test", "{id}", node(t, "bucket: b\nendpoint: localhost:9000"))
	if err == nil {
		t.Fatal("a store without credentials must be rejected")
	}
	if !strings.Contains(err.Error(), "credentials") {
		t.Errorf("the error should name the missing credentials, got %v", err)
	}
}

func TestNewReadsCredentialsFromEnv(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "from-env")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")
	if _, err := New("test", "{id}", node(t, "bucket: b\nendpoint: localhost:9000")); err != nil {
		t.Errorf("credentials from the environment were not picked up: %v", err)
	}
}

func TestNewRejectsTooSmallPartSize(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "k")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "s")
	_, err := New("test", "{id}", node(t, "bucket: b\npart_size: 1M"))
	if err == nil {
		t.Fatal("a part size below the S3 minimum must be rejected at startup")
	}
}

func node(t *testing.T, body string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("cannot build options: %v", err)
	}
	return *doc.Content[0]
}

// --- integration, needs a running S3 endpoint ------------------------------

// liveStore builds a store against the endpoint in YAWN_TEST_S3_ENDPOINT and
// skips the test when it is not set.
func liveStore(t *testing.T) (*Store, string) {
	t.Helper()
	endpoint := os.Getenv("YAWN_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set YAWN_TEST_S3_ENDPOINT to run the S3 integration tests")
	}
	access := os.Getenv("YAWN_TEST_S3_ACCESS_KEY")
	secret := os.Getenv("YAWN_TEST_S3_SECRET_KEY")
	bucket := fmt.Sprintf("yawn-test-%d", os.Getpid())

	host, secure := splitEndpoint(endpoint)
	admin, err := minio.New(host, &minio.Options{
		Creds:  credentials.NewStaticV4(access, secret, ""),
		Secure: secure,
	})
	if err != nil {
		t.Fatalf("cannot build the admin client: %v", err)
	}
	ctx := context.Background()
	if err := admin.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("cannot create bucket %s: %v", bucket, err)
	}
	t.Cleanup(func() {
		for obj := range admin.ListObjects(ctx, bucket, minio.ListObjectsOptions{Recursive: true}) {
			admin.RemoveObject(ctx, bucket, obj.Key, minio.RemoveObjectOptions{})
		}
		admin.RemoveBucket(ctx, bucket)
	})

	body := fmt.Sprintf("endpoint: %s\nbucket: %s\naccess_key_id: %s\nsecret_access_key: %s\nprefix: archive",
		endpoint, bucket, access, secret)
	u, err := New("live", "{id}", node(t, body))
	if err != nil {
		t.Fatalf("cannot create the store: %v", err)
	}
	return u.(*Store), bucket
}

func writeFile(t *testing.T, path, content string) store.Asset {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return store.Asset{Path: path, Rel: filepath.Base(path), Size: int64(len(content))}
}

func TestLiveAvailableAndPut(t *testing.T) {
	s, _ := liveStore(t)
	ctx := context.Background()
	src := t.TempDir()

	if err := s.Available(ctx); err != nil {
		t.Fatalf("Available: %v", err)
	}

	req := store.PutRequest{
		Prefix: "2014/some video [abc]",
		Assets: []store.Asset{
			writeFile(t, filepath.Join(src, "video.mp4"), strings.Repeat("v", 2048)),
			writeFile(t, filepath.Join(src, "video.info.json"), "{}"),
			writeFile(t, filepath.Join(src, "video.jpg"), "thumb"),
		},
	}

	var last store.Progress
	res, err := s.Put(ctx, req, func(p store.Progress) { last = p })
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if res.Bytes != req.TotalBytes() {
		t.Errorf("uploaded %d bytes, want %d", res.Bytes, req.TotalBytes())
	}
	if last.AssetCount != 3 {
		t.Errorf("progress reported %d files, want 3", last.AssetCount)
	}

	ok, size, err := s.Has(ctx, req.Prefix)
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if !ok || size != req.TotalBytes() {
		t.Errorf("Has = (%v, %d), want (true, %d)", ok, size, req.TotalBytes())
	}
}

func TestLiveHasIsFalseForMissingPrefix(t *testing.T) {
	s, _ := liveStore(t)
	ok, size, err := s.Has(context.Background(), "nothing/here")
	if err != nil {
		t.Fatalf("Has: %v", err)
	}
	if ok || size != 0 {
		t.Errorf("Has = (%v, %d), want (false, 0)", ok, size)
	}
}

func TestLiveRepeatedPutSkipsExistingObjects(t *testing.T) {
	s, _ := liveStore(t)
	ctx := context.Background()
	src := t.TempDir()

	req := store.PutRequest{
		Prefix: "repeat",
		Assets: []store.Asset{writeFile(t, filepath.Join(src, "video.mp4"), "payload")},
	}
	if _, err := s.Put(ctx, req, nil); err != nil {
		t.Fatalf("first put: %v", err)
	}

	// a second put must find the object in place and send nothing
	var uploaded int64
	if _, err := s.Put(ctx, req, func(p store.Progress) { uploaded = p.Uploaded }); err != nil {
		t.Fatalf("second put: %v", err)
	}
	if uploaded != req.TotalBytes() {
		t.Errorf("progress reported %d, want the full size to be counted as done", uploaded)
	}
}

func TestLiveAvailableFailsOnMissingBucket(t *testing.T) {
	s, _ := liveStore(t)
	s.bucket = "yawn-definitely-missing-bucket"
	err := s.Available(context.Background())
	if err == nil {
		t.Fatal("a missing bucket must be reported")
	}
	if !strings.Contains(err.Error(), "permanent") {
		t.Errorf("a missing bucket should be a permanent failure, got %v", err)
	}
}
