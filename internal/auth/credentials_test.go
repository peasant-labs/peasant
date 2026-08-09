package auth

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), tmp)

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
	t.Setenv(defaults.EnvXDGConfigHome.String(), tmp)

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
	t.Setenv(defaults.EnvXDGConfigHome.String(), tmp)

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
	t.Setenv(defaults.EnvXDGConfigHome.String(), tmp)

	if err := ClearCredentials(); err != nil {
		t.Fatalf("ClearCredentials on non-existent file: %v", err)
	}
}

func TestSaveCredentials_Permissions(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), tmp)

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

func TestCredentialsFrom_RoundTripIsolatedFromDefaultStore(t *testing.T) {
	defaultHome := t.TempDir()
	customHome := t.TempDir()
	t.Setenv(defaults.EnvXDGConfigHome.String(), defaultHome)
	defaultCredentials := &Credentials{
		APIKey: "default-key", KeyID: "default-key-id", UserID: "default-user-id", Username: "default-user",
	}
	customCredentials := &Credentials{
		APIKey: "custom-key", KeyID: "custom-key-id", UserID: "custom-user-id", Username: "custom-user",
	}
	if err := SaveCredentials(defaultCredentials); err != nil {
		t.Fatalf("seed default credentials: %v", err)
	}
	defaultPath := credentialsPathWith("")
	defaultBefore, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatalf("read default credentials before custom save: %v", err)
	}

	if err := SaveCredentialsFrom(customCredentials, customHome); err != nil {
		t.Fatalf("save custom credentials: %v", err)
	}
	customPath := credentialsPathWith(customHome)
	customLoaded, err := LoadCredentialsFrom(customHome)
	if err != nil || customLoaded == nil || customLoaded.Username != customCredentials.Username {
		t.Fatalf("custom credential round trip = %#v, %v", customLoaded, err)
	}
	defaultLoaded, err := LoadCredentials()
	if err != nil || defaultLoaded == nil || defaultLoaded.Username != defaultCredentials.Username {
		t.Fatalf("default credentials changed after custom save = %#v, %v", defaultLoaded, err)
	}
	defaultAfter, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatalf("read default credentials after custom save: %v", err)
	}
	if !bytes.Equal(defaultBefore, defaultAfter) {
		t.Fatal("custom credential save rewrote the default-profile file")
	}
	info, err := os.Stat(customPath)
	if err != nil {
		t.Fatalf("stat custom credentials: %v", err)
	}
	if info.Mode().Perm() != defaults.PrivateFilePerm {
		t.Errorf("custom credential permissions = %o, want %o", info.Mode().Perm(), defaults.PrivateFilePerm)
	}

	if err := ClearCredentialsFrom(customHome); err != nil {
		t.Fatalf("clear custom credentials: %v", err)
	}
	if customLoaded, err = LoadCredentialsFrom(customHome); err != nil || customLoaded != nil {
		t.Fatalf("custom credentials after clear = %#v, %v, want absent", customLoaded, err)
	}
	defaultLoaded, err = LoadCredentials()
	if err != nil || defaultLoaded == nil || defaultLoaded.Username != defaultCredentials.Username {
		t.Fatalf("clearing custom credentials changed default store = %#v, %v", defaultLoaded, err)
	}
}
