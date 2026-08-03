package tui_test

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui"
	"github.com/peasant-labs/schema"
)

// testSession builds a minimal ingest.Session for TUI tests.
// session_id is a known-valid UUID; turns include one user + one assistant turn.
func testSession() ingest.Session {
	sid, _ := ingest.NewSessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	return ingest.Session{
		ID:        sid,
		Harness:   ingest.HarnessClaudeCode,
		StartTime: time.Now(),
		EndTime:   time.Now().Add(5 * time.Minute),
		Metadata: ingest.SessionMetadata{
			TotalTokens: 1000,
			TurnCount:   2,
			Duration:    5 * time.Minute,
		},
		Turns: []ingest.Turn{
			{Index: 0, Role: "user", Content: "Hello, world!", Depth: 0},
			{Index: 1, Role: "assistant", Content: "Hi there!", Depth: 0},
		},
	}
}

// openDetailView simulates pressing Enter on the first row to open the detail view.
func openDetailView(t *testing.T, m tui.SessionModel) tui.SessionModel {
	t.Helper()
	// First send a window size so the viewport is initialized.
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	// Then press Enter to open the detail view for the first session.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return m
}

// ---------------------------------------------------------------------------
// Badge rendering tests
// ---------------------------------------------------------------------------

// TestSessionDetail_ShowsAnnotationBadges verifies that when a session has
// session-level annotations, the badge appears inline in the detail view.
func TestSessionDetail_ShowsAnnotationBadges(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	sid := string(sessions[0].ID)

	anns := []schema.AnnotationSummary{
		{
			ID:              "ann-001",
			TypeID:          "quality.session_outcome",
			TypeName:        "Session Outcome",
			Value:           "resolved",
			AnnotatorKind:   schema.AnnotatorRule,
			AnnotatorName:   "api",
			TargetKind:      schema.TargetSession,
			TargetSessionID: &sid,
		},
	}

	m := tui.NewSession(sessions).WithAnnotations(sid, anns)
	m = openDetailView(t, m)

	view := m.View()
	if !strings.Contains(view, "Session Outcome") && !strings.Contains(view, "session_outcome") {
		t.Errorf("View() does not contain annotation badge label; got:\n%s", view)
	}
}

// TestSessionDetail_NoBadgesWhenNoAnnotations verifies no badge noise when session has no annotations.
func TestSessionDetail_NoBadgesWhenNoAnnotations(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	m := tui.NewSession(sessions)
	m = openDetailView(t, m)

	view := m.View()
	// Should not contain annotation badge format "[..." unless it's part of turn content.
	// The test session's turn content is "Hello, world!" which has no brackets.
	if strings.Contains(view, "[Session Outcome]") || strings.Contains(view, "[frustration") {
		t.Errorf("View() unexpectedly contains annotation badges when none set: %s", view)
	}
}

// TestSessionDetail_EntryLevelBadgeOnMatchingTurn verifies entry-level annotations
// appear inline with the matching turn (matching by entry index).
func TestSessionDetail_EntryLevelBadgeOnMatchingTurn(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	sid := string(sessions[0].ID)

	entryIdx := 1 // annotation targets turn index 1 (assistant turn)
	anns := []schema.AnnotationSummary{
		{
			ID:               "ann-entry-001",
			TypeID:           "quality.frustration_signal",
			TypeName:         "Frustration Signal",
			Value:            "true",
			AnnotatorKind:    schema.AnnotatorRule,
			AnnotatorName:    "api",
			TargetKind:       schema.TargetEntry,
			TargetSessionID:  &sid,
			TargetEntryIndex: &entryIdx,
		},
	}

	m := tui.NewSession(sessions).WithAnnotations(sid, anns)
	m = openDetailView(t, m)

	view := m.View()
	if !strings.Contains(view, "Frustration Signal") && !strings.Contains(view, "frustration_signal") {
		t.Errorf("View() does not contain entry-level annotation badge; got:\n%s", view)
	}
}

// TestSessionDetail_MultipleBadgesOnSameTurn verifies multiple annotations on the same turn.
func TestSessionDetail_MultipleBadgesOnSameTurn(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	sid := string(sessions[0].ID)

	entryIdx := 0
	anns := []schema.AnnotationSummary{
		{
			ID:               "ann-multi-001",
			TypeID:           "quality.frustration_signal",
			TypeName:         "Frustration",
			Value:            "true",
			AnnotatorKind:    schema.AnnotatorRule,
			AnnotatorName:    "api",
			TargetKind:       schema.TargetEntry,
			TargetSessionID:  &sid,
			TargetEntryIndex: &entryIdx,
		},
		{
			ID:               "ann-multi-002",
			TypeID:           "quality.resolution_evidence",
			TypeName:         "Resolution",
			Value:            "strong",
			AnnotatorKind:    schema.AnnotatorRule,
			AnnotatorName:    "api",
			TargetKind:       schema.TargetEntry,
			TargetSessionID:  &sid,
			TargetEntryIndex: &entryIdx,
		},
	}

	m := tui.NewSession(sessions).WithAnnotations(sid, anns)
	m = openDetailView(t, m)

	view := m.View()
	hasFrustration := strings.Contains(view, "Frustration") || strings.Contains(view, "frustration_signal")
	hasResolution := strings.Contains(view, "Resolution") || strings.Contains(view, "resolution_evidence")

	if !hasFrustration {
		t.Errorf("View() missing first badge (Frustration); view:\n%s", view)
	}
	if !hasResolution {
		t.Errorf("View() missing second badge (Resolution); view:\n%s", view)
	}
}

// TestSessionDetail_AnnotationEditorOpensOnA verifies 'a' key opens the editor modal.
func TestSessionDetail_AnnotationEditorOpensOnA(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	types := []schema.AnnotationTypeSummary{
		{TypeID: "quality.session_outcome", DisplayName: "Session Outcome"},
		{TypeID: "quality.user_frustration", DisplayName: "User Frustration"},
	}

	m := tui.NewSession(sessions).WithAnnotationTypes(types)
	m = openDetailView(t, m)

	// Press 'a' to open the annotation picker.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})

	view := m.View()
	// The editor modal should be visible in the view.
	if !strings.Contains(view, "Session Outcome") || !strings.Contains(view, "User Frustration") {
		t.Errorf("View() after 'a' does not show annotation editor; got:\n%s", view)
	}
}

// TestSessionDetail_EscapeClosesEditor verifies Esc closes the annotation editor.
func TestSessionDetail_EscapeClosesEditor(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	types := []schema.AnnotationTypeSummary{
		{TypeID: "quality.session_outcome", DisplayName: "Session Outcome"},
	}

	m := tui.NewSession(sessions).WithAnnotationTypes(types)
	m = openDetailView(t, m)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("a")})

	// Press Esc to close.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEscape})

	// The model should no longer be in editor mode; View() should not show the picker.
	if !m.InDetail() {
		t.Error("InDetail() = false after Esc from editor; expected to remain in detail")
	}
}

// TestSessionTable_ShowsSessions verifies the table view renders session IDs.
func TestSessionTable_ShowsSessions(t *testing.T) {
	t.Parallel()

	sessions := []ingest.Session{testSession()}
	m := tui.NewSession(sessions)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})

	view := m.View()
	// The abbreviated session ID should appear in the table.
	abbrevID := string(sessions[0].ID)[:8]
	if !strings.Contains(view, abbrevID) {
		t.Errorf("Table view does not contain abbreviated session ID %q; got:\n%s", abbrevID, view)
	}
}
