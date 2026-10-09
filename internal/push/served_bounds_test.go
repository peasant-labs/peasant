package push

import (
	_ "embed"
	"encoding/json"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication_document_limits.yaml
var publicationDocumentLimitsYAML []byte

//go:embed testdata/publication_document_limits.manifest.yaml
var publicationDocumentLimitsManifestYAML []byte

type publicationDocumentLimitCase struct {
	Name           string `yaml:"name"`
	InputBytes     int    `yaml:"inputBytes,omitempty"`
	RedactedBytes  int    `yaml:"redactedBytes"`
	Accepted       bool   `yaml:"accepted"`
	RedactorCalled bool   `yaml:"redactorCalled"`
	Error          string `yaml:"error,omitempty"`
}

func loadPublicationDocumentLimits(t *testing.T) []publicationDocumentLimitCase {
	t.Helper()
	var fixture struct {
		Cases []publicationDocumentLimitCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(publicationDocumentLimitsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publicationDocumentLimitsManifestYAML, "publication document limits")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for i, c := range fixture.Cases {
		names[i] = c.Name
		invalidSizes := c.InputBytes < 0 || c.RedactedBytes <= 0
		missingRefusal := !c.Accepted && c.Error == ""
		if invalidSizes || missingRefusal {
			t.Fatalf("publication document limit fixture %q has invalid sizes or no refusal message", c.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "publication document limits"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// expandingPublicationRedactor models redaction growth at the real dependency
// seam. Only turn text changes; envelope shape and turn identity stay intact.
type expandingPublicationRedactor struct {
	contentBytes int
	called       bool
}

func (r *expandingPublicationRedactor) RedactJSON(value any) any {
	r.called = true
	document := value.(map[string]any)
	detail := document["sessionDetail"].(map[string]any)
	turns := detail["turns"].([]any)
	turns[0].(map[string]any)["content"] = strings.Repeat("x", r.contentBytes)
	return document
}

var _ redact.JSONRedactor = (*expandingPublicationRedactor)(nil)

// Drive the production publication marshaler, not a copy of its scan policy.
// A small input grows during redaction to reach the output cap; the independent
// 64 MiB input seam remains covered. Large cases run serially without services.
func TestPushContent_PublicationDocumentLimits(t *testing.T) {
	for _, c := range loadPublicationDocumentLimits(t) {
		t.Run(c.Name, func(t *testing.T) {
			content := schema.TranscriptContent{
				Kind: schema.ContentKindSessionDetail, ContractVersion: "1.0.0",
				SessionDetail: &schema.SessionDetailPayload{
					ID: testutil.TestSessionUUID, Harness: schema.HarnessClaudeCode,
					Turns: []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "x"}},
				},
			}
			base, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			if c.InputBytes != 0 {
				content.SessionDetail.Turns[0].Content = strings.Repeat("x", c.InputBytes-len(base)+1)
			}
			redactor := &expandingPublicationRedactor{contentBytes: c.RedactedBytes - len(base) + 1}
			body, err := marshalBuiltTranscriptContent(content, redactor)
			if redactor.called != c.RedactorCalled {
				t.Fatalf("redactor called = %v, want %v", redactor.called, c.RedactorCalled)
			}
			if !c.Accepted {
				if err == nil || err.Error() != c.Error || body != nil {
					t.Fatalf("publication refusal = %v, emitted %d bytes; want %q and no output", err, len(body), c.Error)
				}
				return
			}
			if err != nil {
				t.Fatalf("publication document below or at contract cap refused: %v", err)
			}
			if len(body) != c.RedactedBytes {
				t.Fatalf("publication emitted %d bytes, want %d", len(body), c.RedactedBytes)
			}
		})
	}
}

// TestPushContent_BoundsAnOversizedToolResult builds the outward publication body
// for a session holding a tool result larger than the per-field display budget.
// The body is assembled, redacted and re-scanned against the publication caller's
// budget, so the assembly succeeding is the proof that
// the scan in marshalBuiltTranscriptContent passes; the note travels with the
// document so the receiving UI can say the result was shortened.
func TestPushContent_BoundsAnOversizedToolResult(t *testing.T) {
	t.Parallel()

	sessionID := schema.SessionID("11111111-2222-3333-4444-555555555555")
	meta := &ingest.UnifiedMetadata{
		SessionID:    sessionID,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        schema.ModelID(testutil.TestModel),
		CWD:          "/home/example/dev/widgets",
	}
	preview := "reading the whole log"
	toolID := "toolu_push_oversized"
	toolInput := `{"command":"cat huge.log"}`
	toolOutput := strings.Repeat("x", 9<<20)
	entries := []schema.SessionEntry{
		{
			SessionID: sessionID, EntryIndex: 0, Depth: 0, Role: schema.RoleAssistant,
			EntryType: schema.EntryTypeText, ContentPreview: &preview, HasToolUse: true,
		},
		{
			SessionID: sessionID, EntryIndex: 1, Depth: 1, Role: schema.RoleAssistant,
			EntryType: schema.EntryTypeToolUse, HasToolUse: true,
			ToolCallID: &toolID, ToolInput: &toolInput, ToolOutput: &toolOutput,
			ParentIndex: intPtrPushBounds(0), ToolNamesCSV: strPtrPushBounds("Bash"),
		},
	}
	const emit = schema.PushContractVersion("0.1.1")
	redactor, err := redact.NewRedactor(redact.Standard, nil, redact.XDGPaths{})
	if err != nil {
		t.Fatalf("build the redactor the push path uses: %v", err)
	}
	fields := config.PushFieldVisibility{ProjectPath: testutil.BoolPtr(true)}

	body, err := marshalTranscriptContent(meta, entries, emit, fields, sessionorigin.Agent, redactor)
	if err != nil {
		t.Fatalf("the publication body was refused for a session with an oversized record: %v", err)
	}
	if len(body) > defaults.PushTranscriptDocumentCapBytes {
		t.Errorf("the publication body is %d bytes, over the caller budget %d", len(body), defaults.PushTranscriptDocumentCapBytes)
	}
	if _, err := schema.DecodeTranscriptContentRaw(body); err != nil {
		t.Fatalf("the publication body does not decode through the contract: %v", err)
	}

	var content schema.TranscriptContent
	if err := json.Unmarshal(body, &content); err != nil {
		t.Fatalf("decode the publication body: %v", err)
	}
	if content.SessionDetail == nil {
		t.Fatal("the publication body carries no session detail")
	}
	found := ""
	for _, turn := range content.SessionDetail.Turns {
		for _, call := range turn.ToolCalls {
			if len(call.Result) > len(found) {
				found = call.Result
			}
		}
	}
	note := strings.Index(found, "\n[")
	if note < 0 {
		t.Fatalf("the published tool result carries no bound note")
	}
	if note != defaults.ServedTextFieldBudgetBytes {
		t.Errorf("the published tool result shows %d bytes, want the per-field budget %d", note, defaults.ServedTextFieldBudgetBytes)
	}
	for _, want := range []string{"tool result bounded for display", "showing 1 MiB of 9 MiB", "kept by the peasant store that recorded this session"} {
		if !strings.Contains(found[note:], want) {
			t.Errorf("bound note %q does not contain %q", found[note:], want)
		}
	}
	// This body is uploaded and read on another machine. A reader there has no
	// store of their own, so nothing about the record is local to them.
	if strings.Contains(found[note:], "stored locally") {
		t.Errorf("the published bound note tells a remote reader the record is %q: %q", "stored locally", found[note:])
	}
}

func intPtrPushBounds(v int) *int       { return &v }
func strPtrPushBounds(v string) *string { return &v }
