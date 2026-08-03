package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// Credentials holds the authentication data received from the village.
type Credentials struct {
	APIKey     string    `json:"api_key"`
	KeyID      string    `json:"key_id"`
	UserID     string    `json:"user_id"`
	Username   string    `json:"username"`
	VillageURL string    `json:"village_url"`
	LinkedAt   time.Time `json:"linked_at"`
}

// IsValid returns true if the required credential fields are populated.
func (c *Credentials) IsValid() bool {
	return c.APIKey != "" && c.KeyID != "" && c.UserID != "" && c.Username != ""
}

// credentialsPath returns the full path to the credentials file.
// It resolves the config directory at call time (not package init)
// to support t.Setenv in tests.
func credentialsPath() string {
	return filepath.Join(resolveConfigDir(), defaults.CredentialsFile.String())
}

// resolveConfigDir returns the config directory, resolved from the environment
// at call time. This mirrors the lazy resolution pattern used by
// defaults.ResolveDataDirPath.
func resolveConfigDir() string {
	return resolveConfigDirWith("")
}

// resolveConfigDirWith resolves the config directory, preferring an explicit
// XDG_CONFIG_HOME override (e.g. from the --config-dir flag) over the process
// environment. An empty override falls back to the environment. This is the
// parallel-safe path: callers thread the override instead of mutating env.
func resolveConfigDirWith(xdgConfigHomeOverride string) string {
	if xdgConfigHomeOverride != "" {
		return filepath.Join(xdgConfigHomeOverride, defaults.AppName.String())
	}
	if configHome := os.Getenv("XDG_CONFIG_HOME"); configHome != "" {
		return filepath.Join(configHome, defaults.AppName.String())
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", defaults.AppName.String())
	}
	return filepath.Join(os.Getenv("HOME"), ".config", defaults.AppName.String())
}

// LoadCredentials reads stored credentials from disk (config dir from env).
// Returns (nil, nil) if the file does not exist.
func LoadCredentials() (*Credentials, error) {
	return LoadCredentialsFrom("")
}

// LoadCredentialsFrom reads stored credentials, preferring an explicit
// XDG_CONFIG_HOME override over the environment. Returns (nil, nil) if the file
// does not exist.
func LoadCredentialsFrom(xdgConfigHomeOverride string) (*Credentials, error) {
	data, err := os.ReadFile(filepath.Join(resolveConfigDirWith(xdgConfigHomeOverride), defaults.CredentialsFile.String()))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("load credentials: %w", err)
	}

	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return nil, fmt.Errorf("parse credentials: %w", err)
	}
	return &creds, nil
}

// SaveCredentials writes credentials to disk with restricted permissions.
func SaveCredentials(creds *Credentials) error {
	dir := resolveConfigDir()
	if err := os.MkdirAll(dir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal credentials: %w", err)
	}

	if err := os.WriteFile(credentialsPath(), data, defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}

// ClearCredentials removes the credentials file from disk.
// Returns nil if the file does not exist.
func ClearCredentials() error {
	err := os.Remove(credentialsPath())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear credentials: %w", err)
	}
	return nil
}
