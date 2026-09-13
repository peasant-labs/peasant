package push

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// The selection preview is the screen a user decides what to publish on, so it
// shows the transcript the push sends rather than a description of the copy on
// this machine. These cases drive the REAL read: fixture entries through the
// REAL redactor, at the level a push runs at, into the pane the wizard mounts.

//go:embed testdata/wizard_preview.yaml
var wizardPreviewData []byte

// previewEntryFixture is one recorded entry of a fixture transcript.
type previewEntryFixture struct {
	Role      schema.Role      `yaml:"role"`
	EntryType schema.EntryType `yaml:"entryType"`
	Content   string           `yaml:"content"`
	// OmittedRecord makes this entry the placeholder ingest writes where it left
	// a source record out: a real typed omission record in the extra field and
	// the real reader-facing note, both built by the production code, so the
	// pane is handed what a harvest actually stores.
	OmittedRecord bool `yaml:"omittedRecord"`
}

// previewCaptureFixture is the database's capture state for one session. The
// pane's warning is derived from it through the production predicate rather than
// set as a boolean by the fixture, so a case states a STORE STATE and the rule
// that reads it is the one the mounted command uses.
type previewCaptureFixture struct {
	Status      string `yaml:"status"`
	FailureCode string `yaml:"failureCode"`
	Format      string `yaml:"format"`
}

// previewSessionFixture is the stored transcript of one session and the capture
// state the database holds for it.
type previewSessionFixture struct {
	SessionID string                `yaml:"sessionId"`
	Capture   previewCaptureFixture `yaml:"capture"`
	Entries   []previewEntryFixture `yaml:"entries"`
}

// capture parses the fixture's capture state at the boundary, so a typo names
// itself instead of silently becoming a state the rule treats as complete.
func (f previewSessionFixture) capture() ingest.SessionContentCapture {
	status, err := ingest.NewContentCaptureStatus(f.Status())
	if err != nil {
		panic(fmt.Sprintf("wizard preview fixture: session %q declares capture status %q, which is not a capture state this build can represent, so the pane would be handed a state no store can hold and every warning assertion for it would be meaningless: %v", f.SessionID, f.Capture.Status, err))
	}
	code, err := ingest.NewContentCaptureFailureCode(f.Capture.FailureCode)
	if err != nil {
		panic(fmt.Sprintf("wizard preview fixture: session %q declares capture failure code %q, which is not one this build can represent: %v", f.SessionID, f.Capture.FailureCode, err))
	}
	format, err := ingest.NewContentCaptureFormat(f.Format())
	if err != nil {
		panic(fmt.Sprintf("wizard preview fixture: session %q declares capture format %q, which is not one this build can represent: %v", f.SessionID, f.Capture.Format, err))
	}
	return ingest.SessionContentCapture{Status: status, FailureCode: code, CaptureFormat: format}
}

// Status and Format default an unstated capture to the complete, whole-text one,
// which is what every case that says nothing about the store means. A case that
// is ABOUT the capture state states all three fields.
func (f previewSessionFixture) Status() string {
	if f.Capture.Status == "" {
		return string(ingest.ContentCaptureComplete)
	}
	return f.Capture.Status
}

func (f previewSessionFixture) Format() string {
	if f.Capture.Format == "" {
		return string(ingest.ContentCaptureFormatFull)
	}
	return f.Capture.Format
}

// previewCaseFixture names what the pane must and must not show for one
// session.
type previewCaseFixture struct {
	Name         string   `yaml:"name"`
	SessionID    string   `yaml:"sessionId"`
	WantContains []string `yaml:"wantContains"`
	WantMissing  []string `yaml:"wantMissing"`
}

type previewDoc struct {
	RequiredSessionIDs []string                `yaml:"requiredSessionIds"`
	RequiredCaseNames  []string                `yaml:"requiredCaseNames"`
	Sessions           []previewSessionFixture `yaml:"sessions"`
	Cases              []previewCaseFixture    `yaml:"cases"`
}

func decodeWizardPreview(data []byte) (previewDoc, error) {
	var doc previewDoc
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		return doc, fmt.Errorf("decode testdata/wizard_preview.yaml: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found a second YAML document")
		}
		return doc, fmt.Errorf("wizard_preview.yaml must hold exactly one document: %w", err)
	}
	if len(doc.RequiredSessionIDs) == 0 || len(doc.RequiredCaseNames) == 0 {
		return doc, fmt.Errorf("testdata/wizard_preview.yaml declares no required session or case names, so a deleted row would go unnoticed")
	}
	known := make(map[string]bool, len(doc.Sessions))
	for _, session := range doc.Sessions {
		if strings.TrimSpace(session.SessionID) == "" || known[session.SessionID] {
			return doc, fmt.Errorf("preview session is empty or duplicated: %#v", session)
		}
		known[session.SessionID] = true
		for _, entry := range session.Entries {
			if entry.OmittedRecord {
				// The placeholder's role, type and text are not the fixture's to
				// state: they come from the production constructors, so the pane
				// is handed exactly what a harvest stores.
				if strings.TrimSpace(string(entry.Role)) != "" || strings.TrimSpace(string(entry.EntryType)) != "" ||
					strings.TrimSpace(entry.Content) != "" {
					return doc, fmt.Errorf("preview entry of %q sets omittedRecord AND its own role, type or content: %#v; the placeholder is built by the production code so the pane cannot be shown a shape no harvest writes", session.SessionID, entry)
				}
				continue
			}
			if strings.TrimSpace(string(entry.Role)) == "" || strings.TrimSpace(string(entry.EntryType)) == "" ||
				strings.TrimSpace(entry.Content) == "" {
				return doc, fmt.Errorf("preview entry of %q is incomplete: %#v", session.SessionID, entry)
			}
		}
	}
	names := make(map[string]bool, len(doc.Cases))
	for _, row := range doc.Cases {
		if strings.TrimSpace(row.Name) == "" || names[row.Name] || !known[row.SessionID] ||
			len(row.WantContains) == 0 || len(row.WantMissing) == 0 {
			return doc, fmt.Errorf("preview case is invalid, duplicated, or assertion-free: %#v", row)
		}
		names[row.Name] = true
		for _, value := range append(append([]string{}, row.WantContains...), row.WantMissing...) {
			if strings.TrimSpace(value) == "" {
				return doc, fmt.Errorf("preview case %q holds an empty value", row.Name)
			}
		}
	}
	// Deletion protection is by NAME: a row that is removed or renamed is named
	// in the failure, and adding a row does not churn a count.
	for _, required := range doc.RequiredSessionIDs {
		if !known[required] {
			return doc, fmt.Errorf("testdata/wizard_preview.yaml no longer holds the required session %q", required)
		}
	}
	for _, required := range doc.RequiredCaseNames {
		if !names[required] {
			return doc, fmt.Errorf("testdata/wizard_preview.yaml no longer holds the required case %q", required)
		}
	}
	return doc, nil
}

func loadWizardPreviewDoc(t *testing.T) previewDoc {
	t.Helper()
	doc, err := decodeWizardPreview(wizardPreviewData)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// previewFixtureEntries is the stored-entry read the preview binds to in a
// test: the fixture transcripts, in the recorded (unredacted) form the local
// store holds.
//
// It panics on a broken fixture rather than reporting no entries, because a
// preview that silently reads nothing would pass every case that asserts a
// value is ABSENT from the pane.
func previewFixtureEntries() StoredEntriesFunc {
	doc, err := decodeWizardPreview(wizardPreviewData)
	if err != nil {
		panic(err)
	}
	stored := make(map[string]StoredContent, len(doc.Sessions))
	for _, session := range doc.Sessions {
		entries := make([]schema.SessionEntry, 0, len(session.Entries))
		for index, entry := range session.Entries {
			if entry.OmittedRecord {
				entries = append(entries, previewOmissionPlaceholder(session.SessionID, index))
				continue
			}
			content := entry.Content
			entries = append(entries, schema.SessionEntry{
				SessionID:      schema.SessionID(session.SessionID),
				EntryIndex:     index,
				EntryType:      entry.EntryType,
				Role:           entry.Role,
				ContentPreview: &content,
			})
		}
		stored[session.SessionID] = StoredContent{
			Entries: entries,
			// The rule the mounted wizard runs, over the state the fixture
			// declares. Nothing here decides the warning on its own.
			PartialNotice: store.PartialPreviewNeeded(session.capture()),
		}
	}
	return func(sessionID string) (StoredContent, error) { return stored[sessionID], nil }
}

// previewOmissionPlaceholderLimit is the per-record limit the omitted-records
// fixture session was harvested under. It is small so the note it produces is
// distinguishable from the production one at a glance.
const previewOmissionPlaceholderLimit = 8192

// previewOmissionPlaceholder builds the entry a harvest stores where it left a
// source record out, through the production constructors: the typed omission
// record in the extra field and the production reader-facing note. A pane handed
// a hand-written stand-in would never see the sentence users read.
func previewOmissionPlaceholder(sessionID string, index int) schema.SessionEntry {
	record, err := ingest.NewOmittedRecord(
		ingest.OmittedRecordTooLarge, index+1,
		previewOmissionPlaceholderLimit+1, previewOmissionPlaceholderLimit,
	)
	if err != nil {
		panic(fmt.Sprintf("wizard preview fixture: the omission placeholder for session %q could not be built: %v", sessionID, err))
	}
	extra, err := record.Extra()
	if err != nil {
		panic(fmt.Sprintf("wizard preview fixture: the omission record for session %q could not be encoded: %v", sessionID, err))
	}
	note := ingest.OmissionPlaceholderNote(record)
	return schema.SessionEntry{
		SessionID:      schema.SessionID(sessionID),
		EntryIndex:     index,
		EntryType:      schema.EntryTypeToolResult,
		Role:           schema.RoleTool,
		Depth:          0,
		ContentPreview: &note,
		Extra:          &extra,
	}
}

// testRedactor is the redactor the preview tests publish through: the real one,
// at the level a push runs at. It is built once because construction reads the
// shipped rule set.
var testRedactor = sync.OnceValue(func() redact.JSONRedactor {
	redactor, err := redact.NewRedactor(config.RecommendedRedactionLevel, nil, redact.XDGPaths{})
	if err != nil {
		panic(err)
	}
	return redactor
})

// testPublishedTurns is the preview read every wizard test mounts with: fixture
// entries, redacted the way the push redacts them.
func testPublishedTurns() PublishedTurnsFunc {
	return NewPublishedTurns(previewFixtureEntries(), testRedactor())
}

// previewScreen renders one session's pane body at the width the selection page
// gives it, normalised the way the screen guards are.
func previewScreen(t *testing.T, sessionID string) string {
	t.Helper()
	preview := wizardPreviewSource(previewPaneSessions(t), testPublishedTurns(), testTheme())
	body, err := preview.Body(sessionID)
	if err != nil {
		t.Fatalf("preview body for %s: %v", sessionID, err)
	}
	return strings.Join(strings.Fields(ansi.Strip(body.Render(previewPaneWidth))), " ")
}

// previewPaneSessions is the row list the preview pane is mounted over: the
// wizard's own fixture sessions, plus a plain selected row for every fixture
// transcript they do not cover.
//
// The pane only previews rows it was given, so a fixture session with no row
// would silently render the "select a session" note and pass any case that
// asserts something is ABSENT. Deriving the extra rows from the fixture keeps a
// new transcript from needing a code change, and leaves the shared wizard
// fixture - which the rendered golden screens are pinned to - untouched.
func previewPaneSessions(t *testing.T) []PushWizardSession {
	t.Helper()
	sessions := testSessions()
	present := make(map[string]bool, len(sessions))
	for _, session := range sessions {
		present[session.Row.SessionID] = true
	}
	for _, fixture := range loadWizardPreviewDoc(t).Sessions {
		if present[fixture.SessionID] {
			continue
		}
		sessions = append(sessions, PushWizardSession{
			Row: ingest.PushSessionRow{
				SessionID:    fixture.SessionID,
				ModelHarness: string(defaults.HarnessClaudeCode),
				ProjectName:  "my-project",
				StartMs:      1700003000000,
			},
			Action: PushWithRedaction,
		})
	}
	return sessions
}

// previewPaneWidth is a pane width close to what the mounted split gives the
// preview at the review's larger region.
const previewPaneWidth = 60

// TestWizardPreview_ShowsThePublishedTranscript proves the pane draws the
// transcript the push sends: recorded secrets and personal data are gone, the
// prose around them is intact, and nothing on the pane describes the stored
// copy's redaction record.
func TestWizardPreview_ShowsThePublishedTranscript(t *testing.T) {
	doc := loadWizardPreviewDoc(t)
	for _, row := range doc.Cases {
		t.Run(row.Name, func(t *testing.T) {
			screen := previewScreen(t, row.SessionID)
			for _, want := range row.WantContains {
				if !strings.Contains(screen, want) {
					t.Errorf("the preview of %s must show %q; got:\n%s", row.SessionID, want, screen)
				}
			}
			for _, forbidden := range row.WantMissing {
				if strings.Contains(screen, forbidden) {
					t.Errorf("the preview of %s must not show %q; got:\n%s", row.SessionID, forbidden, screen)
				}
			}
		})
	}
}

// TestWizardPreview_NamesTheSessionAndItsState proves the header survives
// beside the transcript: the pane still says which session it is showing and
// whether the push carries it.
func TestWizardPreview_NamesTheSessionAndItsState(t *testing.T) {
	screen := previewScreen(t, "sess-aaa-111")
	for _, want := range []string{"session: sess-aaa-111", "project: my-project", previewSelectedNote} {
		if !strings.Contains(screen, want) {
			t.Errorf("the preview header must show %q; got:\n%s", want, screen)
		}
	}
}

// TestWizardPreview_FailsClosedWithoutARedactor proves the pane cannot fall
// back to recorded text. RedactEntries returns the entries as recorded when it
// is handed no redactor, so a preview built without one would draw exactly the
// values this screen exists to show removed.
func TestWizardPreview_FailsClosedWithoutARedactor(t *testing.T) {
	published, err := NewPublishedTurns(previewFixtureEntries(), nil)("sess-aaa-111")
	if err == nil {
		t.Fatalf("a preview without a redactor must fail rather than render recorded text; got %d turns", len(published.Turns))
	}
	if len(published.Turns) != 0 {
		t.Errorf("a failed preview read must return no turns, got %d", len(published.Turns))
	}
}

// TestWizardPreview_ReportsAFailedRead proves a store read that fails reaches
// the pane as an error rather than as an empty transcript, which the pane would
// otherwise report as a session with nothing stored.
func TestWizardPreview_ReportsAFailedRead(t *testing.T) {
	failing := StoredEntriesFunc(func(string) (StoredContent, error) {
		return StoredContent{}, fmt.Errorf("the local store could not be read")
	})
	preview := wizardPreviewSource(testSessions(), NewPublishedTurns(failing, testRedactor()), testTheme())
	if _, err := preview.Body("sess-aaa-111"); err == nil {
		t.Fatal("a failed entry read must reach the pane as an error")
	}
}

// TestWizardPreview_NeedsIngestShowsTheAvailableTranscript proves the preview
// stopped being gated on publication readiness.
//
// The pane used to answer a session that cannot publish with the repair note
// ALONE, which removed the transcript from exactly the sessions a user opens the
// previewer to look at: they were asked to repair something they could not see.
// Both now appear, note first, and the note still says nothing was uploaded.
func TestWizardPreview_NeedsIngestShowsTheAvailableTranscript(t *testing.T) {
	session := testSessions()[0]
	session.NeedsIngest = true
	preview := wizardPreviewSource([]PushWizardSession{session}, testPublishedTurns(), testTheme())
	body, err := preview.Body(session.Row.SessionID)
	if err != nil {
		t.Fatalf("preview body for a session that needs ingest: %v", err)
	}
	screen := strings.Join(strings.Fields(ansi.Strip(body.Render(previewPaneWidth))), " ")
	// The repair note, the redacted transcript, and the header all survive
	// together; the recorded secret does not reach the pane either way.
	for _, want := range []string{"publication needs database metadata", "redacts them", "<ANTHROPIC_KEY>", "session: " + session.Row.SessionID} {
		if !strings.Contains(screen, want) {
			t.Errorf("the preview of a session that needs ingest must show %q; got:\n%s", want, screen)
		}
	}
	if strings.Contains(screen, "sk-ant-api03") {
		t.Errorf("the preview published a recorded secret; got:\n%s", screen)
	}
}
