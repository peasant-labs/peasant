package push

import (
	_ "embed"
	"encoding/json"
	"strconv"
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

type publicationDocumentLimitsFixture struct {
	Policy struct {
		OutputCapBytes int    `yaml:"outputCapBytes"`
		BelowCapBytes  int    `yaml:"belowCapBytes"`
		AboveCapBytes  int    `yaml:"aboveCapBytes"`
		InputCapBytes  int    `yaml:"inputCapBytes"`
		OutputRefusal  string `yaml:"outputRefusal"`
		InputRefusal   string `yaml:"inputRefusal"`
	} `yaml:"policy"`
	Mechanics struct {
		OutputLimitBytes int `yaml:"outputLimitBytes"`
		InputLimitBytes  int `yaml:"inputLimitBytes"`
	} `yaml:"mechanics"`
	Cases         []publicationDocumentLimitCase `yaml:"cases"`
	RealSizeCases []publicationDocumentLimitCase `yaml:"realSizeCases"`
}

func loadPublicationDocumentLimits(t *testing.T) publicationDocumentLimitsFixture {
	t.Helper()
	var fixture publicationDocumentLimitsFixture
	if err := yaml.Unmarshal(publicationDocumentLimitsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(publicationDocumentLimitsManifestYAML, "publication document limits")
	if err != nil {
		t.Fatal(err)
	}
	all := append(append([]publicationDocumentLimitCase(nil), fixture.Cases...), fixture.RealSizeCases...)
	names := make([]string, len(all))
	for i, c := range all {
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
	return fixture
}

// assertPublicationDocumentPolicy ties the production transcript caps to the
// literal fixture values. The expected side is the YAML data, never the
// production constant, so a cap revert or a widened input seam fails here.
func assertPublicationDocumentPolicy(t *testing.T, f publicationDocumentLimitsFixture) {
	t.Helper()
	if defaults.PushTranscriptDocumentCapBytes != f.Policy.OutputCapBytes {
		t.Fatalf("production transcript output cap=%d want literal %d", defaults.PushTranscriptDocumentCapBytes, f.Policy.OutputCapBytes)
	}
	if transcriptInputRedactionCapBytes != f.Policy.InputCapBytes {
		t.Fatalf("production transcript input cap=%d want literal %d", transcriptInputRedactionCapBytes, f.Policy.InputCapBytes)
	}
	if f.Policy.BelowCapBytes != f.Policy.OutputCapBytes-1 || f.Policy.AboveCapBytes != f.Policy.OutputCapBytes+1 {
		t.Fatalf("output boundary literals below/above=%d/%d are not cap-1/cap+1 of %d", f.Policy.BelowCapBytes, f.Policy.AboveCapBytes, f.Policy.OutputCapBytes)
	}
	if !strings.Contains(f.Policy.OutputRefusal, "schema.ScanRawJSONDocument") || !strings.Contains(f.Policy.OutputRefusal, strconv.Itoa(f.Policy.OutputCapBytes)) {
		t.Fatalf("output refusal literal %q does not name the scanner and cap %d", f.Policy.OutputRefusal, f.Policy.OutputCapBytes)
	}
	if !strings.Contains(f.Policy.InputRefusal, "schema.ScanRawJSONDocument") || !strings.Contains(f.Policy.InputRefusal, strconv.Itoa(f.Policy.InputCapBytes)) {
		t.Fatalf("input refusal literal %q does not name the scanner and cap %d", f.Policy.InputRefusal, f.Policy.InputCapBytes)
	}
	if f.Mechanics.OutputLimitBytes <= 0 || f.Mechanics.InputLimitBytes <= 0 {
		t.Fatalf("mechanics limits=%d/%d must be positive", f.Mechanics.OutputLimitBytes, f.Mechanics.InputLimitBytes)
	}
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

func transcriptContentForLimit() schema.TranscriptContent {
	return schema.TranscriptContent{
		Kind: schema.ContentKindSessionDetail, ContractVersion: "1.0.0",
		SessionDetail: &schema.SessionDetailPayload{
			ID: testutil.TestSessionUUID, Harness: schema.HarnessClaudeCode,
			Turns: []schema.TurnDetail{{Index: 0, Role: schema.RoleAssistant, Depth: 0, Content: "x"}},
		},
	}
}

// Drive the production publication marshaler, not a copy of its scan policy.
// The mechanics cases inject small limits through the marshaler seam so a small
// input grows during redaction to reach the injected output cap; the independent
// input seam remains covered. Large cases run serially without services.
func TestPushContent_PublicationDocumentLimits(t *testing.T) {
	fixture := loadPublicationDocumentLimits(t)
	assertPublicationDocumentPolicy(t, fixture)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			content := transcriptContentForLimit()
			base, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			if c.InputBytes != 0 {
				content.SessionDetail.Turns[0].Content = strings.Repeat("x", c.InputBytes-len(base)+1)
			}
			redactor := &expandingPublicationRedactor{contentBytes: c.RedactedBytes - len(base) + 1}
			body, err := marshalBuiltTranscriptContentWithPolicy(content, redactor, fixture.Mechanics.InputLimitBytes, fixture.Mechanics.OutputLimitBytes)
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

// TestPushContent_TranscriptCapMaterialization drives the one real-size
// transcript case through the production marshaler so the literal 128 MiB cap+1
// target and its exact refusal string are observed, not just asserted as data.
// It is serial and runs only in the non-race pass; the race pass observes the
// same scan through the injected-limit mechanics above.
func TestPushContent_TranscriptCapMaterialization(t *testing.T) {
	if publicationRaceEnabled {
		t.Skip("the real-size transcript cap materialization runs in the non-race pass only")
	}
	fixture := loadPublicationDocumentLimits(t)
	if len(fixture.RealSizeCases) != 1 {
		t.Fatalf("publication document limits fixture declares %d real-size cases, want exactly 1", len(fixture.RealSizeCases))
	}
	for _, c := range fixture.RealSizeCases {
		t.Run(c.Name, func(t *testing.T) {
			content := transcriptContentForLimit()
			base, err := json.Marshal(content)
			if err != nil {
				t.Fatal(err)
			}
			redactor := &expandingPublicationRedactor{contentBytes: c.RedactedBytes - len(base) + 1}
			body, err := marshalBuiltTranscriptContent(content, redactor)
			if redactor.called != c.RedactorCalled {
				t.Fatalf("redactor called = %v, want %v", redactor.called, c.RedactorCalled)
			}
			if err == nil || err.Error() != c.Error || body != nil {
				t.Fatalf("publication refusal = %v, emitted %d bytes; want %q and no output", err, len(body), c.Error)
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
