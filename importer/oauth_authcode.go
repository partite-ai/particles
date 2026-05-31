package importer

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"golang.org/x/oauth2"
)

// runAuthCodeFlow drives the Authorization Code flow, with or
// without PKCE. Spins up a localhost callback server, sends the
// user to the auth URL, and exchanges the returned code for a
// token.
//
// Listens on 127.0.0.1 only (loopback) — the user's auth provider
// has to have a matching `http://127.0.0.1:<port>/callback` URL
// pre-registered. Most providers accept any localhost port; some
// (Google) require exact-match registration. We pick a free port
// dynamically and surface the chosen URL so the user can register
// it if the auth fails.
func runAuthCodeFlow(ctx context.Context, p Prompter, cfg *oauth2.Config, pkce bool) (*oauth2.Token, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen: %w", err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	cfg.RedirectURL = "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"

	state, err := randomState()
	if err != nil {
		return nil, err
	}

	var verifier string
	var authOpts []oauth2.AuthCodeOption
	if pkce {
		verifier = oauth2.GenerateVerifier()
		authOpts = append(authOpts, oauth2.S256ChallengeOption(verifier))
	}
	authURL := cfg.AuthCodeURL(state, authOpts...)

	p.Info("Opening browser to authorize this app...")
	p.Info("If your browser doesn't open, paste this URL into it:")
	p.Info("  " + authURL)
	p.Info(fmt.Sprintf("Callback URL (must be registered with the provider): %s", cfg.RedirectURL))
	if err := openBrowser(authURL); err != nil {
		// Browser launch is best-effort — many headless setups
		// (CI, SSH) won't have a default browser. The user can
		// still paste the URL manually.
		p.Warn(fmt.Sprintf("could not open browser automatically: %v", err))
	}

	type result struct {
		code string
		err  error
	}
	resCh := make(chan result, 1)

	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		// Constant-time compare: the state string is a 32-byte
		// crypto/rand value, but a length-leak via early-exit
		// !=-compare is trivial to remove and the standard
		// library helper covers length-mismatch safely.
		got := q.Get("state")
		if subtle.ConstantTimeCompare([]byte(got), []byte(state)) != 1 {
			http.Error(w, "state mismatch", http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("OAuth callback: state mismatch (csrf protection)")}
			return
		}
		if errCode := q.Get("error"); errCode != "" {
			desc := q.Get("error_description")
			http.Error(w, "auth error: "+errCode, http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("auth provider returned error %q: %s", errCode, desc)}
			return
		}
		code := q.Get("code")
		if code == "" {
			http.Error(w, "missing code", http.StatusBadRequest)
			resCh <- result{err: fmt.Errorf("OAuth callback: provider returned no code")}
			return
		}
		fmt.Fprint(w, callbackOKPage)
		resCh <- result{code: code}
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})

	server := &http.Server{Handler: mux}
	serveErrCh := make(chan error, 1)
	go func() {
		if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
			serveErrCh <- err
		}
	}()
	defer server.Close()

	var got result
	select {
	case got = <-resCh:
	case err := <-serveErrCh:
		return nil, fmt.Errorf("callback server: %w", err)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if got.err != nil {
		return nil, got.err
	}

	var exchOpts []oauth2.AuthCodeOption
	if pkce {
		exchOpts = append(exchOpts, oauth2.VerifierOption(verifier))
	}
	token, err := cfg.Exchange(ctx, got.code, exchOpts...)
	if err != nil {
		return nil, fmt.Errorf("token exchange: %w", err)
	}
	return token, nil
}

// randomState returns 32 bytes of crypto-grade randomness encoded
// as URL-safe base64. Used as the OAuth `state` parameter — its
// only job is to fail any callback whose state doesn't match,
// stopping CSRF.
func randomState() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("randomState: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

const callbackOKPage = `<!doctype html>
<html><head><meta charset="utf-8"><title>Particle authentication complete</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 480px; margin: 4rem auto; padding: 0 1rem; }
  h2 { color: #16a34a; }
  p { color: #4b5563; }
</style></head><body>
<h2>✓ Authentication complete</h2>
<p>You can close this window — return to your terminal to continue.</p>
</body></html>`
