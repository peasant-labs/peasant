package transcript

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/omission_placeholder_projection.yaml
var omissionPlaceholderProjectionYAML []byte

//go:embed testdata/omission_placeholder_projection.manifest.yaml
var omissionPlaceholderProjectionManifestYAML []byte

// omissionPlaceholderProjectionCase is one relation between an omission
// placeholder and a tool call, with the place the reader-facing note must
// appear on the served projection for it.
type omissionPlaceholderProjectionCase struct {
	Name                  string `yaml:"name"`
	ToolCallID            string `yaml:"toolCallId"`
	ToolCallPresent       bool   `yaml:"toolCallPresent"`
	NoteOnItsOwnTurn      bool   `yaml:"noteOnItsOwnTurn"`
	NoteOnAToolCallResult bool   `yaml:"noteOnAToolCallResult"`
}

type omissionPlaceholderProjectionFixture struct {
	Cases []omissionPlaceholderProjectionCase `yaml:"cases"`
}

const omissionPlaceholderProjectionLabel = "omission placeholder projection"

func decodeOmissionPlaceholderProjectionFixture(raw []byte) (omissionPlaceholderProjectionFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture omissionPlaceholderProjectionFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, errors.New("omission placeholder projection fixture must contain exactly one YAML document")
	}
	return fixture, nil
}

func loadOmissionPlaceholderProjectionFixture(t *testing.T) omissionPlaceholderProjectionFixture {
	t.Helper()
	fixture, err := decodeOmissionPlaceholderProjectionFixture(omissionPlaceholderProjectionYAML)
	if err != nil {
		t.Fatalf("decode %s fixture: %v", omissionPlaceholderProjectionLabel, err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(omissionPlaceholderProjectionManifestYAML, omissionPlaceholderProjectionLabel)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.NoteOnItsOwnTurn == fixtureCase.NoteOnAToolCallResult {
			t.Fatalf(
				"%s case %q declares noteOnItsOwnTurn=%v and noteOnAToolCallResult=%v; the note is shown in exactly one place, so exactly one of the two is true",
				omissionPlaceholderProjectionLabel, fixtureCase.Name, fixtureCase.NoteOnItsOwnTurn, fixtureCase.NoteOnAToolCallResult,
			)
		}
		if fixtureCase.ToolCallPresent && fixtureCase.ToolCallID == "" {
			t.Fatalf("%s case %q holds a tool call with no id, which no transcript can contain", omissionPlaceholderProjectionLabel, fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, omissionPlaceholderProjectionLabel); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestOmissionPlaceholderProjectionFixtureGuards(t *testing.T) {
	t.Parallel()
	loadOmissionPlaceholderProjectionFixture(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(omissionPlaceholderProjectionManifestYAML, omissionPlaceholderProjectionLabel)
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(omissionPlaceholderProjectionYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeOmissionPlaceholderProjectionFixture(mutated)
		if err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateRequiredNames(manifest, names, omissionPlaceholderProjectionLabel); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

// omissionPlaceholderProjectionTrailing is the record after the omitted one. It
// must survive on the projection in every case.
const omissionPlaceholderProjectionTrailing = "the tool finished"

// TestOmissionPlaceholderReachesTheServedProjection drives the production
// projection every served surface reads through: the detail socket, the
// kickstart and wizard previews, the export and the publication body all call
// EntriesToTurns, over the stored entries a real harvest leaves behind.
func TestOmissionPlaceholderReachesTheServedProjection(t *testing.T) {
	t.Parallel()
	fixture := loadOmissionPlaceholderProjectionFixture(t)
	for _, fixtureCase := range fixture.Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			entries, note := omissionPlaceholderEntries(t, fixtureCase)
			turns := EntriesToTurns(entries)

			ownTurns := 0
			resultsWithNote := 0
			trailingSurvived := false
			for _, turn := range turns {
				if strings.Contains(turn.Content, note) {
					ownTurns++
				}
				if strings.Contains(turn.Content, omissionPlaceholderProjectionTrailing) {
					trailingSurvived = true
				}
				for _, call := range turn.ToolCalls {
					if strings.Contains(call.Result, note) {
						resultsWithNote++
					}
				}
			}

			if (ownTurns > 0) != fixtureCase.NoteOnItsOwnTurn {
				t.Errorf(
					"the omission note is a turn of its own in %d of %d served turns, want noteOnItsOwnTurn=%v; a reader who is served neither the turn nor a tool result is shown a conversation that silently jumps over the missing record",
					ownTurns, len(turns), fixtureCase.NoteOnItsOwnTurn,
				)
			}
			if (resultsWithNote > 0) != fixtureCase.NoteOnAToolCallResult {
				t.Errorf(
					"the omission note is the result of %d served tool call(s), want noteOnAToolCallResult=%v",
					resultsWithNote, fixtureCase.NoteOnAToolCallResult,
				)
			}
			if ownTurns+resultsWithNote != 1 {
				t.Errorf(
					"the omission note is served %d times (%d own turns, %d tool call results), want exactly once; one omitted record must read as one missing record",
					ownTurns+resultsWithNote, ownTurns, resultsWithNote,
				)
			}
			if !trailingSurvived {
				t.Errorf("the record after the omitted one is missing from the %d served turns", len(turns))
			}
		})
	}
}

// omissionPlaceholderEntries builds the stored entries a harvest writes for the
// case: the conversation around the omitted record, and the placeholder itself
// built from the production typed omission record so the projection reads the
// same Extra payload ingest writes.
func omissionPlaceholderEntries(t *testing.T, fixtureCase omissionPlaceholderProjectionCase) ([]schema.SessionEntry, string) {
	t.Helper()
	const sessionID = schema.SessionID("0f1e2d3c-4b5a-4968-8776-65544332211f")
	record, err := ingest.NewOmittedRecord(ingest.OmittedRecordTooLarge, 3, 8193, 8192)
	if err != nil {
		t.Fatal(err)
	}
	extra, err := record.Extra()
	if err != nil {
		t.Fatal(err)
	}
	note := "tool output omitted: 8 KiB record at line 3 is over the 8 KiB limit"

	text := func(index int, role schema.Role, entryType schema.EntryType, content string) schema.SessionEntry {
		preview := content
		return schema.SessionEntry{
			SessionID:      sessionID,
			EntryIndex:     index,
			Harness:        schema.Harness(ingest.HarnessClaudeCode),
			EntryType:      entryType,
			Role:           role,
			Depth:          0,
			ContentPreview: &preview,
		}
	}

	entries := []schema.SessionEntry{text(0, schema.RoleUser, schema.EntryTypeText, "run the tool")}
	next := 1
	if fixtureCase.ToolCallPresent {
		parent := next
		entries = append(entries, text(parent, schema.RoleAssistant, schema.EntryTypeText, "running the tool"))
		next++
		id := fixtureCase.ToolCallID
		name := "Bash"
		parentIndex := parent
		entries = append(entries, schema.SessionEntry{
			SessionID:    sessionID,
			EntryIndex:   next,
			Harness:      schema.Harness(ingest.HarnessClaudeCode),
			EntryType:    schema.EntryTypeToolUse,
			Role:         schema.RoleAssistant,
			Depth:        1,
			ParentIndex:  &parentIndex,
			ToolCallID:   &id,
			ToolNamesCSV: &name,
		})
		next++
	}

	placeholderNote := note
	placeholder := schema.SessionEntry{
		SessionID:      sessionID,
		EntryIndex:     next,
		Harness:        schema.Harness(ingest.HarnessClaudeCode),
		EntryType:      schema.EntryTypeToolResult,
		Role:           schema.RoleTool,
		Depth:          0,
		ContentPreview: &placeholderNote,
		Extra:          &extra,
	}
	if fixtureCase.ToolCallID != "" {
		id := fixtureCase.ToolCallID
		placeholder.ToolCallID = &id
	}
	entries = append(entries, placeholder)
	next++
	entries = append(entries, text(next, schema.RoleAssistant, schema.EntryTypeText, omissionPlaceholderProjectionTrailing))
	return entries, note
}
