package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/extract"
	"github.com/whiskydumb/yawn/internal/media"
	"github.com/whiskydumb/yawn/internal/queue"
)

// stubExtractor satisfies the interface without touching the network.
type stubExtractor struct{}

func (stubExtractor) Name() string                    { return "stub" }
func (stubExtractor) Available(context.Context) error { return nil }
func (stubExtractor) Resolve(context.Context, string) (*extract.Listing, error) {
	return nil, nil
}
func (stubExtractor) Info(context.Context, string) (*extract.Info, error) { return nil, nil }
func (stubExtractor) Download(context.Context, extract.Request, extract.ProgressFunc) (*extract.Result, error) {
	return nil, nil
}

func newPipeline(t *testing.T, cfg config.Config, converter *media.Converter) (*Pipeline, error) {
	t.Helper()
	q, err := queue.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { q.Close() })

	if cfg.Download.Quality == "" {
		cfg.Download.Quality = "1080"
	}
	return New(Options{
		Config:    cfg,
		Queue:     q,
		Extractor: stubExtractor{},
		Converter: converter,
	})
}

func TestConversionIsOnWhenTheConfigSaysSo(t *testing.T) {
	cfg := config.Default()
	cfg.Convert = config.Convert{Enabled: true, Mode: "remux", Container: "mkv"}

	p, err := newPipeline(t, cfg, media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// a feature that is configured but silently skipped is the worst kind of
	// bug: the run looks successful and does the wrong thing
	if !p.converting() {
		t.Fatal("convert is enabled in the config but the pipeline would skip it")
	}
	if p.convert.Container != "mkv" {
		t.Errorf("container = %q, want mkv", p.convert.Container)
	}
}

func TestConversionIsOffByDefault(t *testing.T) {
	p, err := newPipeline(t, config.Default(), media.NewConverter(""))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.converting() {
		t.Error("conversion must stay off unless the config asks for it")
	}
}

func TestConversionNeedsAConverter(t *testing.T) {
	cfg := config.Default()
	cfg.Convert = config.Convert{Enabled: true, Mode: "remux", Container: "mkv"}

	p, err := newPipeline(t, cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if p.converting() {
		t.Error("without a converter the pipeline must not claim it converts")
	}
}

func TestBadConversionConfigFailsAtStartup(t *testing.T) {
	cfg := config.Default()
	// remux copies the streams, so naming a codec is a contradiction
	cfg.Convert = config.Convert{Enabled: true, Mode: "remux", Container: "mkv", VideoCodec: "libx264"}

	_, err := newPipeline(t, cfg, media.NewConverter(""))
	if err == nil {
		t.Fatal("a contradictory convert config must be rejected before any download")
	}
	if !strings.Contains(err.Error(), "convert") {
		t.Errorf("the error should point at the convert section, got %v", err)
	}
}
