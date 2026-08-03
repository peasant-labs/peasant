package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const staleE2ETTL = 24 * time.Hour

// E2E test sandbox layout.
//
// The harness must NOT touch the user's real peasant data. It computes a throwaway
// sandbox rooted at the RESOLVED XDG_STATE_HOME (OS-compliant default, never a
// hardcoded ~/.local/state), then points every peasant subprocess's
// XDG_{DATA,CONFIG,STATE}_HOME under it:
//
//	realStateDir = defaults.ResolveStateDirPath()      // <XDG_STATE_HOME|~/.local/state>/peasant — from the AMBIENT env, BEFORE any override
//	SANDBOX      = realStateDir/test/e2e/<ts-token>/    // timestamp ⇒ parallel-safe, identifiable on crash-leak
//	  ├── data/    → XDG_DATA_HOME   (test DB at data/peasant/peasant.db)
//	  ├── config/  → XDG_CONFIG_HOME (creds)
//	  └── state/   → XDG_STATE_HOME
//
// SANDBOX lands under the real STATE dir's test/e2e area — deliberately DISTINCT
// from the real DATA dir (~/.local/share/peasant), which stays untouched and is
// asserted unchanged setup→teardown.

// sandboxBase returns the parent dir that holds the per-run timestamped sandboxes.
func sandboxBase(realStateDir string) string {
	return filepath.Join(realStateDir, "test", "e2e")
}

// computeSandbox returns the sandbox root for a run: realStateDir/test/e2e/<ts-token>.
func computeSandbox(realStateDir string, tsToken int64) string {
	return filepath.Join(sandboxBase(realStateDir), strconv.FormatInt(tsToken, 10))
}

// sandboxXDG returns the three XDG dirs (data, config, state) under a sandbox root.
func sandboxXDG(sandbox string) (dataHome, configHome, stateHome string) {
	return filepath.Join(sandbox, "data"),
		filepath.Join(sandbox, "config"),
		filepath.Join(sandbox, "state")
}

// pruneStaleSandboxes performs mandatory start-time cleanup of leftover
// timestamped sandbox dirs from previously hard-crashed runs (which couldn't run
// their t.Cleanup). It removes only children older than the conservative TTL, so
// concurrent recent sibling sandboxes survive. It is a no-op when the base does
// not yet exist.
func pruneStaleSandboxes(realStateDir string) error {
	return pruneStaleSandboxesBefore(realStateDir, time.Now(), staleE2ETTL)
}

func pruneStaleSandboxesBefore(realStateDir string, now time.Time, ttl time.Duration) error {
	base := sandboxBase(realStateDir)
	entries, err := os.ReadDir(base)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	cutoff := now.Add(-ttl)
	for _, e := range entries {
		info, err := e.Info()
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
	}
	return nil
}
