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
	if !forceReauth {
		existing, err := LoadCredentials()
		if err != nil {
			return nil, fmt.Errorf("check existing credentials: %w", err)
		}
		if existing != nil && existing.IsValid() {
			return nil, fmt.Errorf("already logged in as %s (use 'peasant logout' first)", existing.Username)
		}
	}

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
		result.Credentials.VillageURL = villageURL
		result.Credentials.LinkedAt = time.Now()
		if err := SaveCredentials(result.Credentials); err != nil {
			return nil, fmt.Errorf("save credentials: %w", err)
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
