package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
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
	t.Setenv(defaults.EnvXDGConfigHome.String(), defaultHome)
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

func TestLogoutDoesNotClearConcurrentLogin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	requestStarted := make(chan struct{})
	allowResponse := make(chan struct{})
	village := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		select {
		case <-allowResponse:
			w.WriteHeader(http.StatusOK)
		case <-ctx.Done():
		}
	}))
	defer village.Close()
	home := t.TempDir()
	creds := observationCredentials()
	creds.VillageURL = village.URL
	if err := SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- LogoutFrom(ctx, home) }()
	select {
	case <-requestStarted:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	other := creds
	other.UserID = "22222222-2222-4222-8222-222222222222"
	if err := SaveCredentialsFrom(&other, home); err != nil {
		t.Fatal(err)
	}
	close(allowResponse)
	select {
	case err := <-done:
		if !errors.Is(err, ErrCredentialsChanged) {
			t.Fatalf("old logout lost generation check: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	selected, err := LoadCredentialsFrom(home)
	if err != nil || selected == nil || selected.UserID != other.UserID {
		t.Fatalf("concurrent login erased: %v", err)
	}
}

func TestCancelledLogoutStillClearsSelectedCredentials(t *testing.T) {
	home := t.TempDir()
	creds := observationCredentials()
	if err := SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancellation during remote revocation must not retain local credentials.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { cancel(); <-r.Context().Done() }))
	defer server.Close()
	creds.VillageURL = server.URL
	if err := SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	if err := LogoutFrom(ctx, home); err == nil {
		t.Fatal("remote revocation unexpectedly succeeded")
	}
	selected, err := LoadCredentialsFrom(home)
	if err != nil || selected != nil {
		t.Fatalf("cancelled logout retained credentials: %v", err)
	}
}

func TestAlreadyCancelledLogoutStillClearsSelectedCredentials(t *testing.T) {
	home := t.TempDir()
	creds := observationCredentials()
	if err := SaveCredentialsFrom(&creds, home); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := LogoutFrom(ctx, home); err == nil {
		t.Fatal("cancelled remote revocation unexpectedly succeeded")
	}
	selected, err := LoadCredentialsFrom(home)
	if err != nil || selected != nil {
		t.Fatalf("cancelled logout retained credentials: %v", err)
	}
}
