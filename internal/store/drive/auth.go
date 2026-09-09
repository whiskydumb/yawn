package drive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	drivev3 "google.golang.org/api/drive/v3"
)

// ErrNoToken means the store has never been authorised on this machine.
// The fix names the store, so it is added by the caller that knows it.
var ErrNoToken = errors.New("google drive is not authorised")

// scopeFor maps the config value onto an OAuth scope.
//
// drive.file is the default and the least privilege: it grants access only to
// files this app created, which is all an archiver needs. The full scope is
// there for the one case drive.file cannot serve, writing into a folder that
// somebody else created.
func scopeFor(name string) (string, error) {
	switch name {
	case "", "file":
		return drivev3.DriveFileScope, nil
	case "full", "drive":
		return drivev3.DriveScope, nil
	default:
		return "", fmt.Errorf("unknown scope %q: use file or full", name)
	}
}

// oauthConfig builds the OAuth client from a downloaded credentials file or
// from an inline client id and secret.
func oauthConfig(credentialsFile, clientID, clientSecret, scope string) (*oauth2.Config, error) {
	if credentialsFile != "" {
		data, err := os.ReadFile(credentialsFile)
		if err != nil {
			return nil, fmt.Errorf("cannot read credentials %s: %w", credentialsFile, err)
		}
		cfg, err := google.ConfigFromJSON(data, scope)
		if err != nil {
			return nil, fmt.Errorf("cannot parse credentials %s: %w", credentialsFile, err)
		}
		return cfg, nil
	}
	if clientID == "" || clientSecret == "" {
		return nil, errors.New("set options.credentials_file, or options.client_id and options.client_secret")
	}
	return &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     google.Endpoint,
		Scopes:       []string{scope},
	}, nil
}

func loadToken(path string) (*oauth2.Token, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNoToken
		}
		return nil, err
	}
	var tok oauth2.Token
	if err := json.Unmarshal(data, &tok); err != nil {
		return nil, fmt.Errorf("cannot parse the token %s: %w", path, err)
	}
	return &tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(tok, "", "  ")
	if err != nil {
		return err
	}
	// 0600: the refresh token is a long lived credential
	return os.WriteFile(path, data, 0o600)
}

// Login runs the interactive authorisation and writes the token to disk.
//
// It uses the loopback flow, which is why the OAuth client in Google Cloud
// Console has to be of type "Desktop app": that type accepts a redirect to
// 127.0.0.1 on any port, and a Web client does not.
//
// @param port - fixed redirect port, or 0 to pick a free one
func Login(ctx context.Context, credentialsFile, clientID, clientSecret, scopeName, tokenFile string, port int) error {
	scope, err := scopeFor(scopeName)
	if err != nil {
		return err
	}
	cfg, err := oauthConfig(credentialsFile, clientID, clientSecret, scope)
	if err != nil {
		return err
	}

	// port 0 picks a free one, which is what a desktop wants. A fixed port is
	// what makes this work from a container: the redirect has to land on a
	// port that was published with -p, and a random one cannot be.
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("cannot open port %d for the redirect: %w", port, err)
	}
	defer listener.Close()
	cfg.RedirectURL = fmt.Sprintf("http://127.0.0.1:%d/callback", listener.Addr().(*net.TCPAddr).Port)

	state := oauth2.GenerateVerifier()
	verifier := oauth2.GenerateVerifier()

	type result struct {
		code string
		err  error
	}
	results := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if got := q.Get("state"); got != state {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			results <- result{err: errors.New("state mismatch, the response did not belong to this request")}
			return
		}
		if e := q.Get("error"); e != "" {
			http.Error(w, e, http.StatusBadRequest)
			results <- result{err: fmt.Errorf("authorisation refused: %s", e)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "no code", http.StatusBadRequest)
			results <- result{err: errors.New("no authorisation code in the response")}
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, "<html><body><h3>yawn is authorised.</h3><p>You can close this tab.</p></body></html>")
		results <- result{code: code}
	})

	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(listener)
	defer srv.Close()

	authURL := cfg.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.S256ChallengeOption(verifier),
		oauth2.ApprovalForce,
	)
	fmt.Println("open this link and grant access:")
	fmt.Println()
	fmt.Println("  " + authURL)
	fmt.Println()
	openBrowser(authURL)

	timeout, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	var res result
	select {
	case res = <-results:
	case <-timeout.Done():
		return errors.New("timed out waiting for authorisation")
	}
	if res.err != nil {
		return res.err
	}

	tok, err := cfg.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("cannot exchange the code for a token: %w", err)
	}
	if tok.RefreshToken == "" {
		return errors.New("google returned no refresh token; revoke the app's access and try again")
	}
	if err := saveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("cannot save the token: %w", err)
	}
	fmt.Printf("token saved to %s\n", tokenFile)
	return nil
}

// persistingTokenSource writes the token back whenever it is refreshed, so a
// rotated refresh token is not lost on the next run.
type persistingTokenSource struct {
	src  oauth2.TokenSource
	path string
	last string
}

func (p *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := p.src.Token()
	if err != nil {
		return nil, err
	}
	if tok.AccessToken != p.last {
		p.last = tok.AccessToken
		if err := saveToken(p.path, tok); err != nil {
			// not fatal: the token works right now, it just was not persisted
			fmt.Fprintf(os.Stderr, "warning: cannot save the refreshed drive token: %v\n", err)
		}
	}
	return tok, nil
}

func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// best effort: the link is printed above either way
	_ = cmd.Start()
}
