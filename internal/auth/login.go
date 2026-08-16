package auth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/peasant-labs/peasant/internal/browser"
	"github.com/peasant-labs/peasant/internal/defaults"
)

// Login performs the full OAuth browser login flow.
// It starts a local callback server, opens the browser to the village's
// CLI login endpoint, and waits for the OAuth redirect with credentials.
// When forceReauth is true, the village is asked to force GitHub account
// selection (useful when switching accounts).
func Login(ctx context.Context, villageURL string, forceReauth bool) (*Credentials, error) {
	return LoginFrom(ctx, villageURL, forceReauth, "", nil)
}

// LoginFrom performs Login with every credential-store operation pinned to the
// same XDG config-home override. The pre-check and successful callback save can
// therefore never collide with a different default profile.
//
// onURL, when non-nil, receives the exact login URL the runner is about to open
// (or ask the user to open) BEFORE it blocks waiting for the OAuth callback.
// This is the progress boundary a caller uses to surface the URL somewhere the
// browser-open failure message on stderr would not reach (e.g. a TUI alt-screen).
func LoginFrom(ctx context.Context, villageURL string, forceReauth bool, xdgConfigHomeOverride string, onURL func(string)) (*Credentials, error) {
	return loginFromWith(ctx, villageURL, forceReauth, xdgConfigHomeOverride, onURL, browserLogin, time.Now)
}

type browserLoginFunc func(context.Context, string, func(string)) (*Credentials, error)

func loginFromWith(
	ctx context.Context,
	villageURL string,
	forceReauth bool,
	xdgConfigHomeOverride string,
	onURL func(string),
	login browserLoginFunc,
	now func() time.Time,
) (*Credentials, error) {
	if !forceReauth {
		existing, err := LoadCredentialsFrom(xdgConfigHomeOverride)
		if err != nil {
			return nil, fmt.Errorf("check existing credentials: %w", err)
		}
		if existing != nil && existing.IsValid() {
			return nil, fmt.Errorf("already logged in as %s (use 'peasant logout' first)", existing.Username)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("login cancelled: %w", err)
	}
	if login == nil {
		return nil, fmt.Errorf("perform browser login: callback boundary is nil")
	}
	if now == nil {
		return nil, fmt.Errorf("perform browser login: clock boundary is nil")
	}
	credentials, err := login(ctx, villageURL, onURL)
	if err != nil {
		return nil, err
	}
	if credentials == nil {
		return nil, fmt.Errorf("perform browser login: callback returned no credentials; no local credential file was changed; retry the village login")
	}
	credentials.VillageURL = villageURL
	credentials.LinkedAt = now()
	if err := SaveCredentialsFrom(credentials, xdgConfigHomeOverride); err != nil {
		return nil, fmt.Errorf("save credentials: %w", err)
	}
	return credentials, nil
}

// browserLogin performs the browser/callback exchange and returns the received
// credentials without choosing a local store. loginFromWith exclusively owns
// the path-aware pre-check and save on either side of this network boundary.
func browserLogin(ctx context.Context, villageURL string, onURL func(string)) (*Credentials, error) {
	state, err := generateRandomState()
	if err != nil {
		return nil, fmt.Errorf("generate state: %w", err)
	}

	listener, err := startListener()
	if err != nil {
		return nil, err
	}

	port := listener.Addr().(*net.TCPAddr).Port

	resultCh := make(chan LoginResult, 1)
	srv := newCallbackServer(listener, state, villageURL, resultCh)
	defer srv.shutdown()

	go func() {
		_ = srv.serve()
	}()

	loginURL := fmt.Sprintf("%s/api/v1/auth/cli/login?port=%d&state=%s", villageURL, port, state)
	// Report the URL to the caller BEFORE opening the browser or blocking on the
	// callback below, so a caller that cannot rely on stderr (e.g. a TUI
	// alt-screen) can still surface it up front.
	if onURL != nil {
		onURL(loginURL)
	}
	// Opening the browser is best-effort, but a failure MUST be surfaced — the
	// login blocks on the OAuth callback, so the user has to know to open the
	// URL themselves if no browser appeared.
	if err := browser.Open(loginURL); err != nil {
		fmt.Fprintf(os.Stderr, "Warning: could not open browser automatically: %v\n", err)
		fmt.Fprintf(os.Stderr, "Open this URL to continue login: %s\n", loginURL)
	}

	select {
	case result := <-resultCh:
		if result.Err != nil {
			return nil, fmt.Errorf("login callback: %w", result.Err)
		}
		return result.Credentials, nil

	case <-time.After(defaults.LoginTimeout):
		return nil, fmt.Errorf("login timed out (waited %s)", defaults.LoginTimeout)

	case <-ctx.Done():
		return nil, fmt.Errorf("login cancelled: %w", ctx.Err())
	}
}

func generateRandomState() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate random bytes: %w", err)
	}
	return hex.EncodeToString(b), nil
}
