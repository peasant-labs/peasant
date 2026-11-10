package transcript

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/image_turns.yaml
var imageTurnsYAML []byte

//go:embed testdata/image_turns.manifest.yaml
var imageTurnsManifestYAML []byte

type imageTurnEntry struct {
	Index         int    `yaml:"index"`
	Depth         int    `yaml:"depth"`
	Parent        *int   `yaml:"parent"`
	Role          string `yaml:"role"`
	EntryType     string `yaml:"entry_type"`
	PartType      string `yaml:"part_type"`
	Text          string `yaml:"text"`
	Ref           string `yaml:"ref"`
	MediaType     string `yaml:"media_type"`
	ByteSize      int    `yaml:"byte_size"`
	OmittedReason string `yaml:"omitted_reason"`
}

type imageTurnPart struct {
	Kind          string `yaml:"kind"`
	Text          string `yaml:"text"`
	Ref           string `yaml:"ref"`
	OmittedReason string `yaml:"omitted_reason"`
}

type imageTurnCase struct {
	Name        string           `yaml:"name"`
	Entries     []imageTurnEntry `yaml:"entries"`
	WantTurns   int              `yaml:"want_turns"`
	WantParts   []imageTurnPart  `yaml:"want_parts"`
	WantContent string           `yaml:"want_content"`
	Counts      int              `yaml:"counts"`
}

func TestImageTurns(t *testing.T) {
	t.Parallel()
	var document struct {
		Cases []imageTurnCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(imageTurnsYAML, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(imageTurnsManifestYAML, "image turns")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, fixture := range document.Cases {
		names[i] = fixture.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "image turns"); err != nil {
		t.Fatal(err)
	}
	for _, fixture := range document.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			t.Parallel()
			entries := make([]schema.SessionEntry, len(fixture.Entries))
			for i, row := range fixture.Entries {
				entry := schema.SessionEntry{
					SessionID: schema.SessionID(testutil.TestSessionUUID), Harness: schema.HarnessClaudeCode,
					EntryIndex: row.Index, Depth: row.Depth, Role: schema.Role(row.Role), EntryType: schema.EntryType(row.EntryType),
				}
				if row.Parent != nil {
					parent := *row.Parent
					entry.ParentIndex = &parent
				}
				if row.PartType != "" {
					part := row.PartType
					entry.PartType = &part
				}
				if row.Text != "" || row.EntryType == "text" {
					text := row.Text
					entry.ContentPreview = &text
				}
				if row.EntryType == "image" {
					desc := map[string]any{}
					if row.Ref != "" {
						desc["ref"] = row.Ref
						desc["mediaType"] = row.MediaType
						desc["byteSize"] = row.ByteSize
					}
					if row.OmittedReason != "" {
						desc["omittedReason"] = row.OmittedReason
					}
					encoded, err := json.Marshal(desc)
					if err != nil {
						t.Fatal(err)
					}
					text := string(encoded)
					entry.Extra = &text
				}
				entries[i] = entry
			}
			turns := ingest.EntriesToTurns(entries)
			if len(turns) != fixture.WantTurns {
				t.Fatalf("turns = %d, want %d", len(turns), fixture.WantTurns)
			}
			if ingest.ConversationTurnCount(turns) != fixture.Counts {
				t.Fatalf("conversation count = %d, want %d", ingest.ConversationTurnCount(turns), fixture.Counts)
			}
			if turns[0].Content != fixture.WantContent && fixture.WantContent != "" {
				t.Fatalf("content = %q, want %q", turns[0].Content, fixture.WantContent)
			}
			if len(turns[0].Parts) != len(fixture.WantParts) {
				t.Fatalf("parts = %d, want %d", len(turns[0].Parts), len(fixture.WantParts))
			}
			for i, want := range fixture.WantParts {
				got := turns[0].Parts[i]
				if string(got.Kind) != want.Kind {
					t.Fatalf("part %d kind = %s, want %s", i, got.Kind, want.Kind)
				}
				if want.Text != "" && got.Text != want.Text {
					t.Fatalf("part %d text = %q, want %q", i, got.Text, want.Text)
				}
				if want.Ref != "" && (got.Image == nil || got.Image.Ref != want.Ref) {
					t.Fatalf("part %d ref = %+v, want %s", i, got.Image, want.Ref)
				}
				if want.OmittedReason != "" && (got.Image == nil || string(got.Image.OmittedReason) != want.OmittedReason) {
					t.Fatalf("part %d reason = %+v, want %s", i, got.Image, want.OmittedReason)
				}
			}
			detail := SessionToDetail(&ingest.Session{ID: ingest.SessionID(testutil.TestSessionUUID), Harness: schema.HarnessClaudeCode, Turns: turns})
			if len(detail.Turns) != 1 || len(detail.Turns[0].ContentParts) != len(fixture.WantParts) {
				t.Fatalf("detail parts = %d, want %d", len(detail.Turns[0].ContentParts), len(fixture.WantParts))
			}
		})
	}
}
