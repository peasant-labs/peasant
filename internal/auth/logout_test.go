package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLogoutFromClearsOnlySelectedCredentialStore(t *testing.T) {
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/auth/api-keys/custom-key-id" {
			t.Errorf("revocation request = %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer village.Close()

	defaultHome := t.TempDir()
	customHome := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", defaultHome)
	if err := SaveCredentials(&Credentials{
		APIKey: "default-key", KeyID: "default-key-id", UserID: "default-user-id", Username: "default-user",
	}); err != nil {
		t.Fatalf("seed default credentials: %v", err)
	}
	if err := SaveCredentialsFrom(&Credentials{
		APIKey: "custom-key", KeyID: "custom-key-id", UserID: "custom-user-id", Username: "custom-user", VillageURL: village.URL,
	}, customHome); err != nil {
		t.Fatalf("seed custom credentials: %v", err)
	}

	if err := LogoutFrom(context.Background(), customHome); err != nil {
		t.Fatalf("LogoutFrom custom store: %v", err)
	}
	custom, err := LoadCredentialsFrom(customHome)
	if err != nil || custom != nil {
		t.Fatalf("custom credentials after logout = %#v, %v, want absent", custom, err)
	}
	defaultCredentials, err := LoadCredentials()
	if err != nil || defaultCredentials == nil || defaultCredentials.Username != "default-user" {
		t.Fatalf("custom logout changed default credentials = %#v, %v", defaultCredentials, err)
	}
}
