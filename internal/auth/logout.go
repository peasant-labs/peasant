package auth

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// Logout revokes the API key on the village and clears local credentials.
func Logout(ctx context.Context) error {
	creds, err := LoadCredentials()
	if err != nil {
		return fmt.Errorf("load credentials: %w", err)
	}
	if creds == nil {
		return fmt.Errorf("not logged in")
	}

	revokeErr := revokeRemoteKey(ctx, creds)

	// Always clear local credentials, even if remote revocation failed.
	if err := ClearCredentials(); err != nil {
		return fmt.Errorf("clear credentials: %w", err)
	}

	if revokeErr != nil {
		return fmt.Errorf("revoke remote key (credentials cleared locally): %w", revokeErr)
	}
	return nil
}

// revokeRemoteKey sends a DELETE request to revoke the API key on the village.
func revokeRemoteKey(ctx context.Context, creds *Credentials) error {
	villageURL := fmt.Sprintf("%s/api/v1/auth/api-keys/%s", creds.VillageURL, creds.KeyID)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, villageURL, nil)
	if err != nil {
		return fmt.Errorf("create revocation request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+creds.APIKey)

	client := &http.Client{Timeout: defaults.LoginHTTPTimeout}

	// For local development, allow insecure TLS if the village is on localhost.
	if u, err := url.Parse(creds.VillageURL); err == nil {
		if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
			client.Transport = &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
			}
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("send revocation request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("revocation failed with status %d", resp.StatusCode)
	}
	return nil
}
