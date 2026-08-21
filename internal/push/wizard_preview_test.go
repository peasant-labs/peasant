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
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// The selection preview is the screen a user decides what to publish on, so it
// shows the transcript the push sends rather than a description of the copy on
// this machine. These cases drive the REAL read: fixture entries through the
// REAL redactor, at the level a push runs at, into the pane the wizard mounts.

//go:embed testdata/wizard_preview.yaml
var wizardPreviewData []byte

const (
	expectedPreviewSessionCount = 3
	expectedPreviewCaseCount    = 3
)

// previewEntryFixture is one recorded entry of a fixture transcript.
type previewEntryFixture struct {
	Role      schema.Role      `yaml:"role"`
	EntryType schema.EntryType `yaml:"entryType"`
	Content   string           `yaml:"content"`
}

// previewSessionFixture is the stored transcript of one session.
type previewSessionFixture struct {
	SessionID string                `yaml:"sessionId"`
	Entries   []previewEntryFixture `yaml:"entries"`
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
	ExpectedSessionCount int                     `yaml:"expectedSessionCount"`
	ExpectedCaseCount    int                     `yaml:"expectedCaseCount"`
	Sessions             []previewSessionFixture `yaml:"sessions"`
	Cases                []previewCaseFixture    `yaml:"cases"`
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
	if doc.ExpectedSessionCount != expectedPreviewSessionCount || len(doc.Sessions) != expectedPreviewSessionCount {
		return doc, fmt.Errorf("preview sessions: declared=%d actual=%d required=%d",
			doc.ExpectedSessionCount, len(doc.Sessions), expectedPreviewSessionCount)
	}
	if doc.ExpectedCaseCount != expectedPreviewCaseCount || len(doc.Cases) != expectedPreviewCaseCount {
		return doc, fmt.Errorf("preview cases: declared=%d actual=%d required=%d",
			doc.ExpectedCaseCount, len(doc.Cases), expectedPreviewCaseCount)
	}
	known := make(map[string]bool, len(doc.Sessions))
	for _, session := range doc.Sessions {
		if strings.TrimSpace(session.SessionID) == "" || known[session.SessionID] {
			return doc, fmt.Errorf("preview session is empty or duplicated: %#v", session)
		}
		known[session.SessionID] = true
		for _, entry := range session.Entries {
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
	stored := make(map[string][]schema.SessionEntry, len(doc.Sessions))
	for _, session := range doc.Sessions {
		entries := make([]schema.SessionEntry, 0, len(session.Entries))
		for index, entry := range session.Entries {
			content := entry.Content
			entries = append(entries, schema.SessionEntry{
				SessionID:      schema.SessionID(session.SessionID),
				EntryIndex:     index,
				EntryType:      entry.EntryType,
				Role:           entry.Role,
				ContentPreview: &content,
			})
		}
		stored[session.SessionID] = entries
	}
	return func(sessionID string) ([]schema.SessionEntry, error) { return stored[sessionID], nil }
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
	preview := wizardPreviewSource(testSessions(), testPublishedTurns(), testTheme())
	body, err := preview.Body(sessionID)
	if err != nil {
		t.Fatalf("preview body for %s: %v", sessionID, err)
	}
	return strings.Join(strings.Fields(ansi.Strip(body.Render(previewPaneWidth))), " ")
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
	turns, err := NewPublishedTurns(previewFixtureEntries(), nil)("sess-aaa-111")
	if err == nil {
		t.Fatalf("a preview without a redactor must fail rather than render recorded text; got %d turns", len(turns))
	}
	if len(turns) != 0 {
		t.Errorf("a failed preview read must return no turns, got %d", len(turns))
	}
}

// TestWizardPreview_ReportsAFailedRead proves a store read that fails reaches
// the pane as an error rather than as an empty transcript, which the pane would
// otherwise report as a session with nothing stored.
func TestWizardPreview_ReportsAFailedRead(t *testing.T) {
	failing := StoredEntriesFunc(func(string) ([]schema.SessionEntry, error) {
		return nil, fmt.Errorf("the local store could not be read")
	})
	preview := wizardPreviewSource(testSessions(), NewPublishedTurns(failing, testRedactor()), testTheme())
	if _, err := preview.Body("sess-aaa-111"); err == nil {
		t.Fatal("a failed entry read must reach the pane as an error")
	}
}
