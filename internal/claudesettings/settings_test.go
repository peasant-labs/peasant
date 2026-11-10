package claudesettings_test

import (
	_ "embed"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/claudesettings"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/settings_file.yaml
var settingsFileData []byte

type settingsCase struct {
	Name         string   `yaml:"name"`
	Before       string   `yaml:"before"`
	Days         int      `yaml:"days"`
	WantDays     int      `yaml:"wantDays"`
	WantKeys     []string `yaml:"wantKeys"`
	WantError    []string `yaml:"wantError"`
	Missing      bool     `yaml:"missing"`
	BlockReplace bool     `yaml:"blockReplace"`
}

type settingsDocument struct {
	RequiredNames []string       `yaml:"requiredNames"`
	Cases         []settingsCase `yaml:"cases"`
}

func TestSettingsFile(t *testing.T) {
	t.Parallel()
	var document settingsDocument
	if err := testutil.DecodeFixtureYAML(settingsFileData, &document); err != nil {
		t.Fatalf("decode settings fixture: %v", err)
	}
	present := map[string]bool{}
	for _, row := range document.Cases {
		if present[row.Name] {
			t.Fatalf("duplicate settings case %q", row.Name)
		}
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude settings", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		row := row
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			runSettingsCase(t, row)
		})
	}
}

func runSettingsCase(t *testing.T, row settingsCase) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if !row.Missing {
		if err := os.WriteFile(path, []byte(row.Before), 0o600); err != nil {
			t.Fatalf("write settings fixture: %v", err)
		}
	}
	before, err := os.ReadFile(path)
	if row.Missing {
		if !os.IsNotExist(err) {
			t.Fatalf("missing settings file is present: %v", err)
		}
	} else if err != nil {
		t.Fatalf("read settings fixture: %v", err)
	}

	if row.Missing {
		days, found := claudesettings.ReadCleanupDaysAt(path)
		if days != 0 || found {
			t.Fatalf("missing file read = (%d, %t), want (0, false)", days, found)
		}
		opened, err := claudesettings.OpenAt(path)
		if err != nil {
			t.Fatalf("open missing settings: %v", err)
		}
		if got, ok := opened.CleanupDays(); got != 0 || ok {
			t.Fatalf("missing file cleanup days = (%d, %t), want (0, false)", got, ok)
		}
		return
	}

	if row.BlockReplace {
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("make settings directory read-only: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	}

	err = claudesettings.WriteCleanupDaysAt(path, row.Days)
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read settings after write: %v", readErr)
	}
	if len(row.WantError) > 0 {
		if err == nil {
			t.Fatalf("write succeeded, want an error containing %q", row.WantError)
		}
		for _, phrase := range row.WantError {
			if !strings.Contains(err.Error(), phrase) {
				t.Fatalf("error %q does not contain %q", err.Error(), phrase)
			}
		}
		if string(after) != string(before) {
			t.Fatalf("failed write changed the destination\n before: %s\n after: %s", before, after)
		}
		return
	}
	if err != nil {
		t.Fatalf("write cleanup days: %v", err)
	}
	opened, err := claudesettings.OpenAt(path)
	if err != nil {
		t.Fatalf("reopen settings: %v", err)
	}
	got, ok := opened.CleanupDays()
	if !ok || got != row.WantDays {
		t.Fatalf("cleanupPeriodDays = (%d, %t), want (%d, true)", got, ok, row.WantDays)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(after, &decoded); err != nil {
		t.Fatalf("decode written settings: %v", err)
	}
	for _, key := range row.WantKeys {
		if _, found := decoded[key]; !found {
			t.Fatalf("written settings lost key %q: %s", key, after)
		}
	}
	if theme, found := decoded["theme"]; found && string(theme) != `"dark"` {
		t.Fatalf("theme = %s, want \"dark\"", theme)
	}
}
