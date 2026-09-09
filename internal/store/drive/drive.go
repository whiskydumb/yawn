package drive

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"

	"github.com/whiskydumb/yawn/internal/config"
	"github.com/whiskydumb/yawn/internal/store"
	"golang.org/x/oauth2"
	drivev3 "google.golang.org/api/drive/v3"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"gopkg.in/yaml.v3"
)

func init() {
	store.Register("drive", New)
}

type Options struct {
	// CredentialsFile is the client_secret.json downloaded from Google Cloud
	// Console for an OAuth client of type "Desktop app".
	CredentialsFile string `yaml:"credentials_file"`

	// ClientID and ClientSecret are an alternative to the file and accept a
	// ${VAR} reference.
	ClientID     string `yaml:"client_id"`
	ClientSecret string `yaml:"client_secret"`

	TokenFile string `yaml:"token_file"`

	// Scope is "file" (default, only files this app created) or "full".
	Scope string `yaml:"scope"`

	// FolderID uploads into this exact folder. With the default scope it has
	// to be a folder yawn itself created.
	FolderID string `yaml:"folder_id"`

	// RootFolder is the folder to create in My Drive when FolderID is empty.
	RootFolder string `yaml:"root_folder"`

	SharedDriveID string `yaml:"shared_drive_id"`

	// ChunkSize is the resumable upload chunk, e.g. "16M".
	ChunkSize string `yaml:"chunk_size"`
}

type Store struct {
	name   string
	layout string

	// connectMu guards the lazy connect. Upload workers run in parallel, and
	// without it two of them could each create their own root folder and
	// split the archive in half.
	connectMu sync.Mutex
	svc       *drivev3.Service

	folders       *folderCache
	sharedDriveID string
	chunkSize     int

	rootFolderName string
	folderID       string
	tokenFile      string

	// opts is kept so that building the store never touches the network and
	// never fails on a missing token: authorisation is resolved lazily, on
	// the first call that actually needs it.
	opts Options
}

const (
	defaultRootFolder = "yawn-archive"
	defaultTokenFile  = ".yawn-drive-token.json"
	defaultChunkSize  = 16 << 20
	minChunkSize      = 256 << 10 // the resumable protocol requires a multiple of this
)

func New(name, layout string, options yaml.Node) (store.Uploader, error) {
	var opts Options
	if !options.IsZero() {
		if err := options.Decode(&opts); err != nil {
			return nil, fmt.Errorf("cannot parse options: %w", err)
		}
	}
	if opts.TokenFile == "" {
		opts.TokenFile = defaultTokenFile
	}
	if opts.RootFolder == "" {
		opts.RootFolder = defaultRootFolder
	}

	chunk := int64(defaultChunkSize)
	if opts.ChunkSize != "" {
		v, err := config.ParseSize(opts.ChunkSize)
		if err != nil {
			return nil, fmt.Errorf("options.chunk_size: %w", err)
		}
		chunk = v.Bytes()
	}
	if chunk%minChunkSize != 0 {
		return nil, fmt.Errorf("options.chunk_size must be a multiple of %s", config.Size(minChunkSize))
	}

	return &Store{
		name:           name,
		layout:         layout,
		chunkSize:      int(chunk),
		sharedDriveID:  opts.SharedDriveID,
		rootFolderName: opts.RootFolder,
		folderID:       opts.FolderID,
		tokenFile:      opts.TokenFile,
		opts:           opts,
	}, nil
}

func (s *Store) Name() string   { return s.name }
func (s *Store) Kind() string   { return "drive" }
func (s *Store) Layout() string { return s.layout }
func (s *Store) Close() error   { return nil }

func (s *Store) client(ctx context.Context) (*http.Client, error) {
	scope, err := scopeFor(s.opts.Scope)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrPermanent, err)
	}
	cfg, err := oauthConfig(
		s.opts.CredentialsFile,
		config.ResolveSecret(s.opts.ClientID, "GOOGLE_CLIENT_ID"),
		config.ResolveSecret(s.opts.ClientSecret, "GOOGLE_CLIENT_SECRET"),
		scope,
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrPermanent, err)
	}
	tok, err := loadToken(s.tokenFile)
	if errors.Is(err, ErrNoToken) {
		return nil, fmt.Errorf("%w: %w, run: yawn auth %s", store.ErrPermanent, err, s.name)
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", store.ErrPermanent, err)
	}
	src := &persistingTokenSource{src: cfg.TokenSource(ctx, tok), path: s.tokenFile}
	return oauth2.NewClient(ctx, oauth2.ReuseTokenSource(nil, src)), nil
}

// connect builds the Drive service and resolves the root folder once.
func (s *Store) connect(ctx context.Context) error {
	s.connectMu.Lock()
	defer s.connectMu.Unlock()
	if s.svc != nil {
		return nil
	}
	httpClient, err := s.client(ctx)
	if err != nil {
		return err
	}
	svc, err := drivev3.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return fmt.Errorf("cannot build the drive client: %w", err)
	}

	rootID := s.folderID
	if rootID == "" {
		// ensureRootFolder needs the service, but s.svc is only published on
		// success so that a failed connect leaves the store unconnected
		s.svc = svc
		rootID, err = s.ensureRootFolder(ctx)
		if err != nil {
			s.svc = nil
			return classify(err)
		}
	}
	s.svc = svc
	s.folders = newFolderCache(rootID)
	return nil
}

// ensureRootFolder finds or creates the archive folder in My Drive.
func (s *Store) ensureRootFolder(ctx context.Context) (string, error) {
	parent := "root"
	if s.sharedDriveID != "" {
		parent = s.sharedDriveID
	}
	id, err := s.findFolder(ctx, s.rootFolderName, parent)
	if err != nil {
		return "", err
	}
	if id != "" {
		return id, nil
	}
	return s.createFolder(ctx, s.rootFolderName, parent)
}

func (s *Store) Available(ctx context.Context) error {
	if err := s.connect(ctx); err != nil {
		return err
	}
	if _, err := s.svc.About.Get().Fields("user(emailAddress)").Context(ctx).Do(); err != nil {
		return classify(fmt.Errorf("cannot reach google drive: %w", err))
	}
	return nil
}

func (s *Store) Has(ctx context.Context, prefix string) (bool, int64, error) {
	if err := s.connect(ctx); err != nil {
		return false, 0, err
	}
	folderID, err := s.lookupPath(ctx, prefix)
	if err != nil {
		return false, 0, err
	}
	if folderID == "" {
		return false, 0, nil
	}
	count, bytes, err := s.listFolder(ctx, folderID)
	if err != nil {
		return false, 0, err
	}
	return count > 0, bytes, nil
}

func (s *Store) Put(ctx context.Context, req store.PutRequest, onProgress store.ProgressFunc) (*store.PutResult, error) {
	if err := s.connect(ctx); err != nil {
		return nil, err
	}
	folderID, err := s.ensurePath(ctx, req.Prefix)
	if err != nil {
		return nil, classify(err)
	}

	total := req.TotalBytes()
	var written int64

	for i, asset := range req.Assets {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		// already there at the same size: an interrupted run must not resend
		existingID, existingSize, err := s.findFile(ctx, asset.Rel, folderID)
		if err != nil {
			return nil, classify(err)
		}
		if existingID != "" && existingSize == asset.Size && asset.Size > 0 {
			written += asset.Size
			report(onProgress, asset, written, total, i, len(req.Assets))
			continue
		}
		if existingID != "" {
			// a leftover of a different size is a partial upload, not a file
			if err := s.svc.Files.Delete(existingID).SupportsAllDrives(true).Context(ctx).Do(); err != nil {
				return nil, classify(fmt.Errorf("cannot remove the partial %s: %w", asset.Rel, err))
			}
		}

		base := written
		if err := s.putOne(ctx, folderID, asset, req.Read, func(done int64) {
			written = base + done
			report(onProgress, asset, written, total, i, len(req.Assets))
		}); err != nil {
			return nil, classify(fmt.Errorf("cannot upload %s: %w", asset.Rel, err))
		}
		written = base + asset.Size
	}

	return &store.PutResult{
		Key:   "drive://" + strings.Trim(req.Prefix, "/") + " (" + folderID + ")",
		Bytes: written,
	}, nil
}

func (s *Store) putOne(ctx context.Context, folderID string, asset store.Asset,
	throttle func(context.Context, io.Reader) io.Reader, onDone func(int64)) error {

	f, err := os.Open(asset.Path)
	if err != nil {
		return err
	}
	defer f.Close()

	meta := &drivev3.File{
		Name:    asset.Rel,
		Parents: []string{folderID},
	}
	call := s.svc.Files.Create(meta).
		Fields("id, size").
		SupportsAllDrives(true).
		Media(throttle(ctx, f), googleapi.ChunkSize(s.chunkSize)).
		ProgressUpdater(func(current, _ int64) { onDone(current) })

	_, err = call.Context(ctx).Do()
	return err
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

// classify marks Drive failures that retrying cannot fix.
func classify(err error) error {
	var api *googleapi.Error
	if errors.As(err, &api) {
		switch api.Code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound:
			// 403 also covers the daily upload quota, which is worth retrying
			// tomorrow but not in this run
			return fmt.Errorf("%w: %v", store.ErrPermanent, err)
		}
	}
	return err
}

// Login runs the interactive authorisation for this store and saves the
// token where the store expects to find it.
func (s *Store) Login(ctx context.Context, port int) error {
	return Login(ctx,
		s.opts.CredentialsFile,
		config.ResolveSecret(s.opts.ClientID, "GOOGLE_CLIENT_ID"),
		config.ResolveSecret(s.opts.ClientSecret, "GOOGLE_CLIENT_SECRET"),
		s.opts.Scope,
		s.tokenFile,
		port,
	)
}

// TokenPath reports where this store keeps its token.
func (s *Store) TokenPath() string { return s.tokenFile }
