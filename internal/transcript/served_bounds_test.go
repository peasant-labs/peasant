package transcript

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/served_bounds.yaml
var servedBoundsFixtureYAML []byte

//go:embed testdata/served_bounds.manifest.yaml
var servedBoundsManifestYAML []byte

type servedBoundsExpectation struct {
	Bounded      bool     `yaml:"bounded"`
	ShownBytes   int      `yaml:"shownBytes,omitempty"`
	NoteContains []string `yaml:"noteContains,omitempty"`
}

type servedBoundsCase struct {
	Name                          string                    `yaml:"name"`
	Kind                          string                    `yaml:"kind"`
	Fill                          string                    `yaml:"fill"`
	FieldBytes                    []int                     `yaml:"fieldBytes"`
	Expected                      []servedBoundsExpectation `yaml:"expected,omitempty"`
	EqualBoundAcrossBoundedFields bool                      `yaml:"equalBoundAcrossBoundedFields,omitempty"`
	AllBounded                    bool                      `yaml:"allBounded,omitempty"`
	// EveryBoundedFieldShowsBytes requires that a bounded field still shows the
	// reader some of the record. A shared bound of zero bytes satisfies
	// allBounded and equalBoundAcrossBoundedFields while serving every field as
	// the note alone, so those two alone cannot tell a lowered bound from a
	// collapsed one.
	EveryBoundedFieldShowsBytes bool `yaml:"everyBoundedFieldShowsBytes,omitempty"`
	// BoundIsLargestThatFits requires that the shared bound cannot be raised: a
	// bound one byte larger must push the encoded document over the budget.
	BoundIsLargestThatFits bool `yaml:"boundIsLargestThatFits,omitempty"`
}

type servedBoundsFixture struct {
	Cases []servedBoundsCase `yaml:"cases"`
}

func servedBoundsKind(name string) (ServedTextKind, error) {
	switch name {
	case "tool_result":
		return ServedTextToolResult, nil
	case "tool_arguments":
		return ServedTextToolArguments, nil
	case "turn_content":
		return ServedTextTurnContent, nil
	}
	return 0, fmt.Errorf("served bounds fixture names field kind %q; use tool_result, tool_arguments or turn_content", name)
}

func decodeServedBoundsFixture(raw []byte) (servedBoundsFixture, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	var fixture servedBoundsFixture
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, fmt.Errorf("decode served bounds fixture: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fixture, fmt.Errorf("served bounds fixture must contain exactly one YAML document: %v", err)
	}
	return fixture, nil
}

func loadServedBoundsFixture(t *testing.T) servedBoundsFixture {
	t.Helper()
	fixture, err := decodeServedBoundsFixture(servedBoundsFixtureYAML)
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(servedBoundsManifestYAML, "served bounds")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" || fixtureCase.Fill == "" || len(fixtureCase.FieldBytes) == 0 {
			t.Fatalf("served bounds fixture case %q is incomplete", fixtureCase.Name)
		}
		if _, err := servedBoundsKind(fixtureCase.Kind); err != nil {
			t.Fatalf("served bounds fixture case %q: %v", fixtureCase.Name, err)
		}
		if len(fixtureCase.Expected) != 0 && len(fixtureCase.Expected) != len(fixtureCase.FieldBytes) {
			t.Fatalf("served bounds fixture case %q states %d expectations for %d fields", fixtureCase.Name, len(fixtureCase.Expected), len(fixtureCase.FieldBytes))
		}
		if fixtureCase.BoundIsLargestThatFits && !fixtureCase.EqualBoundAcrossBoundedFields {
			t.Fatalf("served bounds fixture case %q asks for the largest fitting bound without asking for one shared bound", fixtureCase.Name)
		}
		if len(fixtureCase.Expected) == 0 && !fixtureCase.AllBounded {
			t.Fatalf("served bounds fixture case %q asserts nothing about its fields", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "served bounds"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func TestServedBoundsFixtureGuards(t *testing.T) {
	loadServedBoundsFixture(t)
	manifest, err := testutil.DecodeRequiredNamesManifest(servedBoundsManifestYAML, "served bounds")
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range manifest.RequiredNames {
		mutated := bytes.Replace(servedBoundsFixtureYAML, []byte("name: "+required), []byte("name: replacement_case"), 1)
		fixture, err := decodeServedBoundsFixture(mutated)
		if err != nil {
			t.Fatalf("decode renamed fixture: %v", err)
		}
		names := make([]string, len(fixture.Cases))
		for index, fixtureCase := range fixture.Cases {
			names[index] = fixtureCase.Name
		}
		if err := testutil.ValidateRequiredNames(manifest, names, "served bounds"); err == nil {
			t.Fatalf("required case %q replacement unexpectedly validated", required)
		}
	}
}

// servedBoundsText repeats fill until it is exactly size bytes long. The fixture
// picks fills whose byte length divides the requested size.
func servedBoundsText(fill string, size int) (string, error) {
	if size%len(fill) != 0 {
		return "", fmt.Errorf("served bounds fixture asks for %d bytes of %q, whose rune is %d bytes wide", size, fill, len(fill))
	}
	return strings.Repeat(fill, size/len(fill)), nil
}

// servedBoundsSession builds the session the production projection converts. The
// fields are placed exactly where the case's kind says they live.
func servedBoundsSession(t *testing.T, fixtureCase servedBoundsCase) (*ingest.Session, []string) {
	t.Helper()
	kind, err := servedBoundsKind(fixtureCase.Kind)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	session := &ingest.Session{
		ID:        ingest.SessionID("served-bounds-session"),
		Harness:   schema.HarnessClaudeCode,
		Model:     "anthropic/claude-opus-4-8",
		StartTime: at,
		EndTime:   at.Add(time.Minute),
	}
	originals := make([]string, len(fixtureCase.FieldBytes))
	for index, size := range fixtureCase.FieldBytes {
		text, err := servedBoundsText(fixtureCase.Fill, size)
		if err != nil {
			t.Fatal(err)
		}
		originals[index] = text
		turn := ingest.Turn{Index: index, Role: schema.RoleAssistant, Timestamp: at, EntryType: schema.EntryTypeText}
		switch kind {
		case ServedTextTurnContent:
			turn.Content = text
		case ServedTextToolArguments:
			turn.ToolCalls = []ingest.ToolCall{{ID: fmt.Sprintf("call-%d", index), Name: "Bash", Arguments: text}}
		case ServedTextToolResult:
			turn.ToolCalls = []ingest.ToolCall{{ID: fmt.Sprintf("call-%d", index), Name: "Bash", Result: text}}
		}
		session.Turns = append(session.Turns, turn)
	}
	return session, originals
}

// servedBoundsTarget is the served field of the case's kind at index, as a
// pointer, so a check can both read it and re-bound it in place.
func servedBoundsTarget(detail *schema.SessionDetailPayload, kind ServedTextKind, index int) *string {
	turn := &detail.Turns[index]
	switch kind {
	case ServedTextTurnContent:
		return &turn.Content
	case ServedTextToolArguments:
		return &turn.ToolCalls[0].Arguments
	default:
		return &turn.ToolCalls[0].Result
	}
}

// servedBoundsServed reads back the served value of field index, from the same
// projection every consumer of the detail reads.
func servedBoundsServed(t *testing.T, detail *schema.SessionDetailPayload, kind ServedTextKind, index int) string {
	t.Helper()
	return *servedBoundsTarget(detail, kind, index)
}

// assertServedBoundIsLargestThatFits proves the shared bound is not merely a
// bound that fits but the largest one: raising it by a single byte per field
// must push the encoded document over the served budget. The raise goes through
// the production bounding function and the real encoder, so the claim is
// measured on a document rather than read back out of the search arithmetic.
//
// It mutates detail, so a caller runs it after every other assertion.
func assertServedBoundIsLargestThatFits(t *testing.T, detail *schema.SessionDetailPayload, kind ServedTextKind, originals []string, boundLengths map[int]struct{}) {
	t.Helper()
	if len(boundLengths) != 1 {
		t.Fatalf("bounded fields show %d distinct sizes, so the case has no single shared bound to raise", len(boundLengths))
	}
	applied := 0
	for shown := range boundLengths {
		applied = shown
	}
	if applied <= 0 {
		t.Fatalf("the shared bound is %d bytes, so every bounded field is served as its note alone", applied)
	}
	if applied >= defaults.ServedTextFieldBudgetBytes {
		t.Fatalf("the shared bound equals the per-field budget of %d bytes, so this case never reaches document pressure", defaults.ServedTextFieldBudgetBytes)
	}
	for index, original := range originals {
		*servedBoundsTarget(detail, kind, index) = boundServedText(kind, original, applied+1)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal the detail at one byte above the shared bound: %v", err)
	}
	if len(encoded) <= defaults.ServedDetailDocumentBudgetBytes {
		t.Errorf("a shared bound of %d bytes also fits (document %d bytes of the %d-byte budget), so the chosen bound of %d bytes is not the largest that fits",
			applied+1, len(encoded), defaults.ServedDetailDocumentBudgetBytes, applied)
	}
}

// TestServedBoundsProductionPath drives the canonical producer boundary, the one
// the session_detail WebSocket, the kickstart preview, export and publication all
// reach. Its success is itself the proof: SessionToDetailValidated ends in
// schema.DecodeSessionDetailPayloadRaw, which refuses a document over the
// contract's cap.
func TestServedBoundsProductionPath(t *testing.T) {
	fixture := loadServedBoundsFixture(t)
	for _, fixtureCase := range fixture.Cases {
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			kind, err := servedBoundsKind(fixtureCase.Kind)
			if err != nil {
				t.Fatal(err)
			}
			session, originals := servedBoundsSession(t, fixtureCase)
			detail, err := SessionToDetailValidated(session)
			if err != nil {
				t.Fatalf("served detail refused: %v", err)
			}
			encoded, err := json.Marshal(detail)
			if err != nil {
				t.Fatalf("marshal served detail: %v", err)
			}
			if len(encoded) > defaults.ServedDetailDocumentBudgetBytes {
				t.Errorf("served document is %d bytes, over the budget of %d", len(encoded), defaults.ServedDetailDocumentBudgetBytes)
			}
			if _, err := schema.DecodeSessionDetailPayloadRaw(encoded); err != nil {
				t.Fatalf("served document does not decode through the contract: %v", err)
			}
			boundLengths := map[int]struct{}{}
			for index, original := range originals {
				served := servedBoundsServed(t, detail, kind, index)
				if !utf8.ValidString(served) {
					t.Errorf("field %d served invalid UTF-8", index)
				}
				bounded := served != original
				if bounded {
					shown := strings.Index(served, "\n[")
					if shown < 0 {
						t.Fatalf("field %d was shortened without a note", index)
					}
					boundLengths[shown] = struct{}{}
					if notes := strings.Count(served, "bounded for display"); notes != 1 {
						t.Errorf("field %d carries %d notes, want exactly one", index, notes)
					}
					if !strings.HasPrefix(original, served[:shown]) {
						t.Errorf("field %d served text is not the leading bytes of the record", index)
					}
					if fixtureCase.EveryBoundedFieldShowsBytes && shown <= 0 {
						t.Errorf("field %d is bounded and shows %d bytes of the record, so the reader is served the note alone", index, shown)
					}
				}
				if fixtureCase.AllBounded && !bounded {
					t.Errorf("field %d was served whole, but every field of this case must be bounded", index)
				}
				if len(fixtureCase.Expected) == 0 {
					continue
				}
				expected := fixtureCase.Expected[index]
				if bounded != expected.Bounded {
					t.Fatalf("field %d bounded=%v, want %v", index, bounded, expected.Bounded)
				}
				if !expected.Bounded {
					continue
				}
				shown := strings.Index(served, "\n[")
				if expected.ShownBytes != 0 && shown != expected.ShownBytes {
					t.Errorf("field %d shows %d bytes, want %d", index, shown, expected.ShownBytes)
				}
				for _, want := range expected.NoteContains {
					if !strings.Contains(served[shown:], want) {
						t.Errorf("field %d note %q does not contain %q", index, served[shown:], want)
					}
				}
			}
			if fixtureCase.EqualBoundAcrossBoundedFields && len(boundLengths) != 1 {
				t.Errorf("bounded fields show %d distinct sizes, want one shared bound", len(boundLengths))
			}
			if fixtureCase.BoundIsLargestThatFits {
				assertServedBoundIsLargestThatFits(t, detail, kind, originals, boundLengths)
			}
		})
	}
}

// TestServedDocumentBudgetDerivesFromTheContractCap pins the one place the cap is
// stated and the margin the served budget keeps under it.
func TestServedDocumentBudgetDerivesFromTheContractCap(t *testing.T) {
	budget := DefaultServedDocumentBudget()
	if budget.Document() >= defaults.SessionDetailDocumentCapBytes {
		t.Fatalf("served document budget %d is not below the contract cap %d", budget.Document(), defaults.SessionDetailDocumentCapBytes)
	}
	if budget.Document() != defaults.SessionDetailDocumentCapBytes-defaults.ServedDetailDocumentMarginBytes {
		t.Fatalf("served document budget %d is not the contract cap less the declared margin", budget.Document())
	}
	if budget.PerField() > budget.Document() {
		t.Fatalf("per-field bound %d exceeds the document budget %d", budget.PerField(), budget.Document())
	}
	// A document built right at the budget still decodes through the contract.
	filler := strings.Repeat("x", budget.Document())
	at := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)
	session := &ingest.Session{ID: ingest.SessionID("budget-edge"), Harness: schema.HarnessClaudeCode, StartTime: at, EndTime: at,
		Turns: []ingest.Turn{{Index: 0, Role: schema.RoleAssistant, Timestamp: at, EntryType: schema.EntryTypeText,
			ToolCalls: []ingest.ToolCall{{ID: "call-0", Name: "Bash", Result: filler}}}}}
	detail, err := SessionToDetailValidated(session)
	if err != nil {
		t.Fatalf("detail at the budget refused: %v", err)
	}
	encoded, err := json.Marshal(detail)
	if err != nil {
		t.Fatalf("marshal detail at the budget: %v", err)
	}
	if len(encoded) > budget.Document() {
		t.Fatalf("detail at the budget encodes to %d bytes, over the budget %d", len(encoded), budget.Document())
	}
	if _, err := schema.DecodeSessionDetailPayloadRaw(encoded); err != nil {
		t.Fatalf("detail at the budget does not decode through the contract: %v", err)
	}
}

// TestServedDocumentBudgetConstructorRefusesIncoherentSizes keeps the boundary
// typed: a caller cannot assemble a budget whose parts contradict each other.
func TestServedDocumentBudgetConstructorRefusesIncoherentSizes(t *testing.T) {
	if _, err := NewServedDocumentBudget(16, 0); err == nil {
		t.Fatal("a zero document budget was accepted")
	}
	if _, err := NewServedDocumentBudget(-1, 16); err == nil {
		t.Fatal("a negative per-field bound was accepted")
	}
	if _, err := NewServedDocumentBudget(32, 16); err == nil {
		t.Fatal("a per-field bound over the document budget was accepted")
	}
	budget, err := NewServedDocumentBudget(16, 32)
	if err != nil {
		t.Fatalf("coherent budget refused: %v", err)
	}
	if budget.PerField() != 16 || budget.Document() != 32 {
		t.Fatalf("budget reports %d/%d, want 16/32", budget.PerField(), budget.Document())
	}
}
