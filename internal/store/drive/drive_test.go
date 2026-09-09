package drive

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/whiskydumb/yawn/internal/store"
	"golang.org/x/oauth2"
	drivev3 "google.golang.org/api/drive/v3"
	"gopkg.in/yaml.v3"
)

func node(t *testing.T, body string) yaml.Node {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("cannot build options: %v", err)
	}
	return *doc.Content[0]
}

func TestScopeFor(t *testing.T) {
	cases := map[string]string{
		"":      drivev3.DriveFileScope,
		"file":  drivev3.DriveFileScope,
		"full":  drivev3.DriveScope,
		"drive": drivev3.DriveScope,
	}
	for in, want := range cases {
		got, err := scopeFor(in)
		if err != nil {
			t.Errorf("scopeFor(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("scopeFor(%q) = %q, want %q", in, got, want)
		}
	}
	if _, err := scopeFor("everything"); err == nil {
		t.Error("an unknown scope must be rejected")
	}
}

func TestEscapeQuery(t *testing.T) {
	cases := []struct{ in, want string }{
		{"plain", "plain"},
		{"it's here", `it\'s here`},
		{`back\slash`, `back\\slash`},
		{`both'\ways`, `both\'\\ways`},
	}
	for _, c := range cases {
		if got := escapeQuery(c.in); got != c.want {
			t.Errorf("escapeQuery(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestEscapeQueryProtectsTheQuery(t *testing.T) {
	// a title carrying a quote must not be able to end the query string
	name := "video' or name = 'anything"
	q := "name = '" + escapeQuery(name) + "'"
	if strings.Count(q, "'")-strings.Count(q, `\'`) != 2 {
		t.Errorf("unescaped quotes leaked into the query: %s", q)
	}
}

func TestSplitPath(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"2014/video [abc]", []string{"2014", "video [abc]"}},
		{"/leading/and/trailing/", []string{"leading", "and", "trailing"}},
		{"a//b", []string{"a", "b"}},
		{"", nil},
		{"///", nil},
	}
	for _, c := range cases {
		got := splitPath(c.in)
		if len(got) == 0 && len(c.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitPath(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestNewAppliesDefaults(t *testing.T) {
	u, err := New("gdrive", "{id}", node(t, "credentials_file: creds.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s := u.(*Store)
	if s.tokenFile != defaultTokenFile {
		t.Errorf("token file %q, want the default %q", s.tokenFile, defaultTokenFile)
	}
	if s.rootFolderName != defaultRootFolder {
		t.Errorf("root folder %q, want the default %q", s.rootFolderName, defaultRootFolder)
	}
	if s.chunkSize != defaultChunkSize {
		t.Errorf("chunk size %d, want the default %d", s.chunkSize, defaultChunkSize)
	}
}

func TestNewRejectsBadChunkSize(t *testing.T) {
	// the resumable protocol only accepts a multiple of 256 KiB. Note that
	// every whole MiB already is one, so a bad value has to be sub-MiB.
	if _, err := New("gdrive", "{id}", node(t, "chunk_size: 300K")); err == nil {
		t.Error("a chunk size that is not a multiple of 256 KiB must be rejected")
	}
	if _, err := New("gdrive", "{id}", node(t, "chunk_size: 16M")); err != nil {
		t.Errorf("16M is a valid chunk size: %v", err)
	}
}

func TestNewDoesNotNeedATokenYet(t *testing.T) {
	// building a store must never touch the disk or the network, otherwise
	// `yawn auth` could not build the very store it is about to authorise
	if _, err := New("gdrive", "{id}", node(t, "token_file: /definitely/missing/token.json")); err != nil {
		t.Errorf("New must not require an existing token: %v", err)
	}
}

func TestAvailableReportsMissingToken(t *testing.T) {
	u, err := New("gdrive", "{id}", node(t,
		"client_id: id\nclient_secret: secret\ntoken_file: "+
			filepath.ToSlash(filepath.Join(t.TempDir(), "absent.json"))))
	if err != nil {
		t.Fatal(err)
	}
	err = u.Available(context.Background())
	if err == nil {
		t.Fatal("a store with no token must report itself unavailable")
	}
	if !errors.Is(err, store.ErrPermanent) {
		t.Errorf("a missing token is permanent until the user logs in, got %v", err)
	}
	// the hint has to name this store, not the store type: `yawn auth` takes
	// the name from the config, and "drive" would be wrong for a store called
	// anything else
	if !strings.Contains(err.Error(), "yawn auth gdrive") {
		t.Errorf("the error should name the store to authorise, got %v", err)
	}
	if !errors.Is(err, ErrNoToken) {
		t.Errorf("ErrNoToken must stay in the chain, got %v", err)
	}
}

func TestOauthConfigNeedsCredentials(t *testing.T) {
	if _, err := oauthConfig("", "", "", drivev3.DriveFileScope); err == nil {
		t.Error("a config with neither a file nor a client id must be rejected")
	}
	cfg, err := oauthConfig("", "id", "secret", drivev3.DriveFileScope)
	if err != nil {
		t.Fatalf("inline credentials: %v", err)
	}
	if cfg.ClientID != "id" || len(cfg.Scopes) != 1 {
		t.Errorf("built an unexpected config: %+v", cfg)
	}
}

func TestTokenRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	if _, err := loadToken(path); !errors.Is(err, ErrNoToken) {
		t.Fatalf("a missing token file should give ErrNoToken, got %v", err)
	}

	want := &oauth2.Token{AccessToken: "at", RefreshToken: "rt", TokenType: "Bearer"}
	if err := saveToken(path, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err := loadToken(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.AccessToken != want.AccessToken || got.RefreshToken != want.RefreshToken {
		t.Errorf("round trip lost data: %+v", got)
	}

	if runtime.GOOS == "windows" {
		// windows ignores unix modes, the token is protected by NTFS ACLs there
		return
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// a refresh token is a long lived credential and must not be world readable
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("token file mode is %v, want no access for group or others", perm)
	}
}
