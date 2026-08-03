package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	creds := &Credentials{
		APIKey:     "peasant_test1234",
		KeyID:      "key-uuid-1",
		UserID:     "user-uuid-1",
		Username:   "testuser",
		VillageURL: defaults.DefaultVillageURL.String(),
		LinkedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}

	if err := SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if loaded == nil {
		t.Fatal("LoadCredentials returned nil")
	}

	if loaded.APIKey != creds.APIKey {
		t.Errorf("APIKey = %q, want %q", loaded.APIKey, creds.APIKey)
	}
	if loaded.KeyID != creds.KeyID {
		t.Errorf("KeyID = %q, want %q", loaded.KeyID, creds.KeyID)
	}
	if loaded.UserID != creds.UserID {
		t.Errorf("UserID = %q, want %q", loaded.UserID, creds.UserID)
	}
	if loaded.Username != creds.Username {
		t.Errorf("Username = %q, want %q", loaded.Username, creds.Username)
	}
	if loaded.VillageURL != creds.VillageURL {
		t.Errorf("VillageURL = %q, want %q", loaded.VillageURL, creds.VillageURL)
	}
}

func TestLoadCredentials_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	creds, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	if creds != nil {
		t.Fatalf("expected nil credentials, got %+v", creds)
	}
}

func TestClearCredentials(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	creds := &Credentials{
		APIKey:   "peasant_test1234",
		KeyID:    "key-uuid-1",
		UserID:   "user-uuid-1",
		Username: "testuser",
	}
	if err := SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	if err := ClearCredentials(); err != nil {
		t.Fatalf("ClearCredentials: %v", err)
	}

	loaded, err := LoadCredentials()
	if err != nil {
		t.Fatalf("LoadCredentials after clear: %v", err)
	}
	if loaded != nil {
		t.Fatalf("expected nil after clear, got %+v", loaded)
	}
}

func TestClearCredentials_NotFound(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	if err := ClearCredentials(); err != nil {
		t.Fatalf("ClearCredentials on non-existent file: %v", err)
	}
}

func TestSaveCredentials_Permissions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	creds := &Credentials{
		APIKey:   "peasant_test1234",
		KeyID:    "key-uuid-1",
		UserID:   "user-uuid-1",
		Username: "testuser",
	}
	if err := SaveCredentials(creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}

	path := filepath.Join(tmp, "peasant", defaults.CredentialsFile.String())
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat credentials file: %v", err)
	}

	perm := info.Mode().Perm()
	if perm != defaults.PrivateFilePerm {
		t.Errorf("file perm = %o, want %o", perm, defaults.PrivateFilePerm)
	}
}

func TestCredentials_IsValid(t *testing.T) {
	tests := []struct {
		name  string
		creds Credentials
		want  bool
	}{
		{
			name:  "all fields",
			creds: Credentials{APIKey: "k", KeyID: "k", UserID: "u", Username: "n"},
			want:  true,
		},
		{
			name:  "missing api key",
			creds: Credentials{KeyID: "k", UserID: "u", Username: "n"},
			want:  false,
		},
		{
			name:  "missing username",
			creds: Credentials{APIKey: "k", KeyID: "k", UserID: "u"},
			want:  false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.creds.IsValid(); got != tt.want {
				t.Errorf("IsValid() = %v, want %v", got, tt.want)
			}
		})
	}
}
