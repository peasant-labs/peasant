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

// resolveConfigDirWith resolves the config directory, preferring an explicit
// XDG_CONFIG_HOME override (e.g. from the --config-dir flag) over the process
// environment. An empty override falls back to the environment. This is the
// parallel-safe path: callers thread the override instead of mutating env. The
// canonical defaults resolver owns the XDG/home precedence for every caller.
func resolveConfigDirWith(xdgConfigHomeOverride string) string {
	return defaults.ResolveConfigDirPathWith(xdgConfigHomeOverride).String()
}

func credentialsPathWith(xdgConfigHomeOverride string) string {
	return filepath.Join(resolveConfigDirWith(xdgConfigHomeOverride), defaults.CredentialsFile.String())
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
	data, err := os.ReadFile(credentialsPathWith(xdgConfigHomeOverride))
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
	return SaveCredentialsFrom(creds, "")
}

// SaveCredentialsFrom atomically writes credentials under the config directory
// selected by xdgConfigHomeOverride. An empty override preserves the environment
// based behavior of SaveCredentials.
func SaveCredentialsFrom(creds *Credentials, xdgConfigHomeOverride string) error {
	dir := resolveConfigDirWith(xdgConfigHomeOverride)
	if err := os.MkdirAll(dir, defaults.PrivateDirPerm); err != nil {
		return fmt.Errorf("save credentials: create private config directory %q: %w; no credential file was changed; fix directory ownership or permissions and retry", dir, err)
	}

	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return fmt.Errorf("save credentials: marshal credentials for %q: %w; no credential file was changed; correct the unsupported credential value and retry", credentialsPathWith(xdgConfigHomeOverride), err)
	}
	path := credentialsPathWith(xdgConfigHomeOverride)
	tmp, err := os.CreateTemp(dir, ".credentials-*.json.tmp")
	if err != nil {
		return fmt.Errorf("save credentials: create temporary file beside %q: %w; no credential file was changed; fix directory permissions or free space and retry", path, err)
	}
	tmpPath := tmp.Name()
	committed := false
	defer func() {
		_ = tmp.Close()
		if !committed {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(defaults.PrivateFilePerm); err != nil {
		return fmt.Errorf("save credentials: set private permissions on temporary file %q for %q: %w; the destination was not changed; fix filesystem permission support and retry", tmpPath, path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("save credentials: write temporary file %q for %q: %w; the destination was not changed; free disk space or repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("save credentials: sync temporary file %q for %q: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save credentials: close temporary file %q for %q: %w; the destination was not changed; repair the filesystem and retry", tmpPath, path, err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("save credentials: atomically replace %q from %q: %w; the previous destination remains available; fix destination permissions and retry", path, tmpPath, err)
	}
	committed = true
	if parent, err := os.Open(dir); err == nil {
		if syncErr := parent.Sync(); syncErr != nil {
			_ = parent.Close()
			return fmt.Errorf("save credentials: sync directory %q after replacing %q: %w; the new file is present but crash durability is not confirmed; verify it and retry", dir, path, syncErr)
		}
		if closeErr := parent.Close(); closeErr != nil {
			return fmt.Errorf("save credentials: close directory %q after replacing %q: %w; the new file is present; verify it before continuing", dir, path, closeErr)
		}
	}
	return nil
}

// ClearCredentials removes the credentials file from disk.
// Returns nil if the file does not exist.
func ClearCredentials() error {
	return ClearCredentialsFrom("")
}

// ClearCredentialsFrom removes credentials only from the config directory
// selected by xdgConfigHomeOverride. Returns nil if the file does not exist.
func ClearCredentialsFrom(xdgConfigHomeOverride string) error {
	path := credentialsPathWith(xdgConfigHomeOverride)
	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear credentials at %q: %w; the selected credential file remains in place; fix file permissions and retry", path, err)
	}
	return nil
}
