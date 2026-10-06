package transcript

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/thinking_level_entry_validation.yaml
var thinkingEntryValidationYAML []byte

//go:embed testdata/thinking_level_entry_validation.manifest.yaml
var thinkingEntryValidationManifestYAML []byte

type thinkingEntryValidationFixture struct {
	ErrorFragments []string `yaml:"errorFragments"`
	Cases          []struct {
		Name        string      `yaml:"name"`
		Role        schema.Role `yaml:"role"`
		Extra       string      `yaml:"extra"`
		Dropped     bool        `yaml:"dropped"`
		ServedLevel string      `yaml:"servedLevel"`
		ServedRaw   string      `yaml:"servedRaw"`
	} `yaml:"cases"`
}

func loadThinkingEntryValidationFixture(t *testing.T) thinkingEntryValidationFixture {
	t.Helper()
	var fixture thinkingEntryValidationFixture
	decoder := yaml.NewDecoder(bytes.NewReader(thinkingEntryValidationYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must contain one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(thinkingEntryValidationManifestYAML, "thinking entry validation")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "thinking entry validation"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestThinkingLevelStoredEvidenceDropsWithWarning proves malformed stored
// per-turn thinking-level evidence is DROPPED with an actionable warning and
// served absent, not refused at the fold. Each case drives both the pure
// warning collector and EntriesToProjectionValidated, the boundary the
// WebSocket viewer and the export path share, and observes the slog warning the
// boundary emits. The test is serial because it installs a process-global slog
// handler.
func TestThinkingLevelStoredEvidenceDropsWithWarning(t *testing.T) {
	fixture := loadThinkingEntryValidationFixture(t)

	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for i, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			entry := schema.SessionEntry{EntryIndex: i + 1, Role: c.Role, EntryType: schema.EntryTypeText}
			content := "recorded output"
			entry.ContentPreview = &content
			if c.Extra != "" {
				value := c.Extra
				entry.Extra = &value
			}

			warnings := ThinkingLevelEntryWarnings([]schema.SessionEntry{entry})
			if got := len(warnings) > 0; got != c.Dropped {
				t.Fatalf("dropped=%t, warnings=%d: %+v", c.Dropped, len(warnings), warnings)
			}
			if c.Dropped {
				if len(warnings) != 1 {
					t.Fatalf("want exactly one warning per occurrence, got %d", len(warnings))
				}
				w := warnings[0]
				if w.EntryIndex != entry.EntryIndex {
					t.Errorf("warning entry index = %d, want %d", w.EntryIndex, entry.EntryIndex)
				}
				if w.Role != c.Role {
					t.Errorf("warning role = %q, want %q", w.Role, c.Role)
				}
				if w.Fix == "" {
					t.Error("warning carries no fix")
				}
				message := w.Message()
				for _, fragment := range fixture.ErrorFragments {
					if !strings.Contains(message, fragment) {
						t.Errorf("warning missing %q: %s", fragment, message)
					}
				}
			}

			buf.Reset()
			projection, err := EntriesToProjectionValidated([]schema.SessionEntry{entry}, ProjectionOptions{})
			if err != nil {
				t.Fatalf("malformed stored evidence must be dropped, not refused: %v", err)
			}
			level, raw := servedThinkingLevel(projection.Turns)
			if c.Dropped {
				if level != "" || raw != "" {
					t.Fatalf("dropped evidence served as level=%q raw=%q, want absent", level, raw)
				}
			} else if level != schema.ThinkingLevel(c.ServedLevel) || raw != schema.ThinkingLevelRaw(c.ServedRaw) {
				t.Fatalf("served level=%q raw=%q, want level=%q raw=%q", level, raw, c.ServedLevel, c.ServedRaw)
			}

			logged := buf.String()
			if c.Dropped {
				if !strings.Contains(logged, "thinking-level stored evidence dropped") {
					t.Fatalf("no dropped-evidence warning was logged: %q", logged)
				}
				if !strings.Contains(logged, fmt.Sprintf("\"entry_index\":%d", entry.EntryIndex)) {
					t.Fatalf("the warning did not name the entry index: %q", logged)
				}
				if !strings.Contains(logged, "\"fix\":") {
					t.Fatalf("the warning carried no fix field: %q", logged)
				}
			} else if strings.Contains(logged, "thinking-level stored evidence dropped") {
				t.Fatalf("well-formed evidence was warned as dropped: %q", logged)
			}
		})
	}
}

// servedThinkingLevel returns the first non-absent thinking-level observation
// across the folded turns.
func servedThinkingLevel(turns []ingest.Turn) (schema.ThinkingLevel, schema.ThinkingLevelRaw) {
	for _, turn := range turns {
		if turn.ThinkingLevel != "" || turn.ThinkingLevelRaw != "" {
			return turn.ThinkingLevel, turn.ThinkingLevelRaw
		}
	}
	return "", ""
}
