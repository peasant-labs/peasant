package tui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	schema "github.com/peasant-labs/schema"
)

// commitResult is the internal message returned after attempting to commit pending
// annotations to the backend HTTP server.
type commitResult struct {
	committed int
	err       error
}

// SessionModel shows a table of sessions with drill-down detail.
// The detail view renders annotation badges inline with turns and supports
// vim-inspired navigation and annotation creation via the 'a' key.
type SessionModel struct {
	sessions    []ingest.Session
	table       table.Model
	viewport    viewport.Model
	inDetail    bool
	selectedIdx int
	width       int
	height      int

	// Annotation display: session_id → loaded annotations from backend.
	annotations map[string][]schema.AnnotationSummary

	// Annotation types for the picker (loaded from backend GET /api/v1/annotation-types).
	annotationTypes []schema.AnnotationTypeSummary

	// Pending annotations: locally created in the TUI, not yet committed to backend.
	pending      []store.PendingAnnotationRecord
	pendingStore *store.Store // nil disables pending annotation CRUD

	// serverURL is the base URL for the annotation backend (e.g. "http://localhost:8690").
	// Empty disables HTTP commit and annotation-type fetch.
	serverURL string

	// Editor modal: opened by 'a' key in the detail view.
	editorOpen bool
	editor     AnnotationEditorModel

	// markedTurns holds the set of turn indices currently marked for annotation.
	markedTurns map[int]bool

	// cursorTurnIdx is the index into sessions[selectedIdx].Turns for the currently focused turn.
	cursorTurnIdx int

	// rangeStart is the first turn index in an active range selection (Shift+Space). -1 = inactive.
	rangeStart int
}

// NewSession creates a SessionModel with the given sessions.
func NewSession(sessions []ingest.Session) SessionModel {
	columns := []table.Column{
		{Title: "ID", Width: defaults.ColWidthID},
		{Title: "Provider", Width: defaults.ColWidthProvider},
		{Title: "Outcome", Width: defaults.ColWidthOutcome},
		{Title: "Date", Width: defaults.ColWidthDate},
		{Title: "Duration", Width: defaults.ColWidthDuration},
		{Title: "Tokens", Width: defaults.ColWidthTokens},
		{Title: "Turns", Width: defaults.ColWidthTurns},
	}

	rows := make([]table.Row, len(sessions))
	for i, s := range sessions {
		outcome := "─"
		if s.Metadata.Quality != nil && s.Metadata.Quality.Outcome != nil {
			outcome = s.Metadata.Quality.Outcome.String()
		}
		rows[i] = table.Row{
			string(s.ID[:defaults.SessionIDDisplayLen]),
			string(s.Harness),
			outcome,
			s.StartTime.Format("Jan 02 15:04"),
			formatDuration(s.Metadata.Duration),
			fmt.Sprintf("%d", s.Metadata.TotalTokens),
			fmt.Sprintf("%d", s.Metadata.TurnCount),
		}
	}

	t := table.New(
		table.WithColumns(columns),
		table.WithRows(rows),
		table.WithFocused(true),
		table.WithHeight(10),
	)

	s := table.DefaultStyles()
	s.Header = s.Header.
		BorderStyle(lipgloss.NormalBorder()).
		BorderBottom(true).
		Bold(true)
	s.Selected = s.Selected.
		Foreground(lipgloss.Color(defaults.ColorDark.String())).
		Background(lipgloss.Color(defaults.ColorPastelLilac.String())).
		Bold(false)
	t.SetStyles(s)

	return SessionModel{
		sessions:    sessions,
		table:       t,
		markedTurns: make(map[int]bool),
		rangeStart:  -1,
	}
}

// WithAnnotations returns a copy of the model with the given annotations pre-loaded
// for the specified session ID. Used for testing and for populating data from messages.
func (m SessionModel) WithAnnotations(sessionID string, anns []schema.AnnotationSummary) SessionModel {
	if m.annotations == nil {
		m.annotations = make(map[string][]schema.AnnotationSummary)
	} else {
		// Copy map to avoid mutating the shared reference.
		cp := make(map[string][]schema.AnnotationSummary, len(m.annotations))
		for k, v := range m.annotations {
			cp[k] = v
		}
		m.annotations = cp
	}
	m.annotations[sessionID] = anns
	return m
}

// WithAnnotationTypes returns a copy of the model with the given annotation types
// pre-loaded for the picker. Used for testing and for populating data from messages.
func (m SessionModel) WithAnnotationTypes(types []schema.AnnotationTypeSummary) SessionModel {
	m.annotationTypes = types
	return m
}

// WithPendingStore returns a copy of the model wired to a local SQLite pending store.
// Call this in production to enable pending annotation CRUD.
func (m SessionModel) WithPendingStore(s *store.Store, serverURL string) SessionModel {
	m.pendingStore = s
	m.serverURL = serverURL
	return m
}

func (m SessionModel) Init() tea.Cmd {
	return nil
}

func (m SessionModel) Update(msg tea.Msg) (SessionModel, tea.Cmd) {
	var cmd tea.Cmd

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		contentHeight := m.height - 4
		if contentHeight < 3 {
			contentHeight = 3
		}
		m.table.SetHeight(contentHeight)
		m.viewport.Width = m.width
		m.viewport.Height = contentHeight
		if m.inDetail {
			m.viewport.SetContent(m.renderDetail())
		}
		if m.editorOpen {
			var edCmd tea.Cmd
			m.editor, edCmd = m.editor.Update(msg)
			_ = edCmd
		}
		return m, nil

	case AnnotationPickedMsg:
		// User selected an annotation type and value from the editor.
		m.editorOpen = false
		if m.pendingStore != nil && m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
			sess := m.sessions[m.selectedIdx]
			rec := store.PendingAnnotationRecord{
				ID:         fmt.Sprintf("%d", time.Now().UnixNano()), // rough unique ID; production uses UUID
				SessionID:  string(sess.ID),
				TypeID:     msg.TypeID,
				Value:      msg.Value,
				EntryIndex: msg.EntryIdx,
			}
			_ = m.pendingStore.CreatePendingAnnotation(context.Background(), rec)
			// Reload pending for this session.
			m.pending, _ = m.pendingStore.ListPendingBySession(context.Background(), string(sess.ID))
		}
		m.viewport.SetContent(m.renderDetail())
		return m, nil

	case AnnotationEditorCancelMsg:
		m.editorOpen = false
		m.viewport.SetContent(m.renderDetail())
		return m, nil

	case commitResult:
		if msg.err == nil {
			// Clear pending for the current session.
			if m.pendingStore != nil && m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
				sess := m.sessions[m.selectedIdx]
				_ = m.pendingStore.DeleteAllPendingBySession(context.Background(), string(sess.ID))
				m.pending = []store.PendingAnnotationRecord{}
			}
		}
		m.viewport.SetContent(m.renderDetail())
		return m, nil

	case tea.KeyMsg:
		if m.editorOpen {
			// Delegate all key events to the editor when open.
			m.editor, cmd = m.editor.Update(msg)
			return m, cmd
		}

		if m.inDetail {
			switch msg.String() {
			case defaults.KeyEscape.String(), defaults.KeyBackspace.String():
				m.inDetail = false
				m.editorOpen = false
				return m, nil

			case defaults.KeyAnnotate.String():
				// Open annotation type picker.
				m.editorOpen = true
				m.editor = NewAnnotationEditor(m.annotationTypes, nil)
				return m, nil

			case defaults.KeyCommitPending.String():
				// Commit pending annotations via HTTP POST.
				if m.pendingStore != nil && m.serverURL != "" && m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
					pending := append([]store.PendingAnnotationRecord{}, m.pending...)
					serverURL := m.serverURL
					return m, func() tea.Msg {
						committed, err := commitPendingAnnotations(pending, serverURL)
						return commitResult{committed: committed, err: err}
					}
				}
				return m, nil

			case defaults.KeyDeletePending.String():
				// Delete the last pending annotation for this session.
				if m.pendingStore != nil && len(m.pending) > 0 {
					last := m.pending[len(m.pending)-1]
					_ = m.pendingStore.DeletePendingByID(context.Background(), last.ID)
					m.pending = m.pending[:len(m.pending)-1]
					m.viewport.SetContent(m.renderDetail())
				}
				return m, nil

			case defaults.KeyVimLeft.String():
				// h: jump to previous depth=0 turn.
				if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) || len(m.sessions[m.selectedIdx].Turns) == 0 {
					return m, nil
				}
				m.cursorTurnIdx = m.prevDepthZeroTurn(m.cursorTurnIdx)
				m.viewport.SetContent(m.renderDetail())
				m.scrollToTurn(m.cursorTurnIdx)
				return m, nil

			case defaults.KeyVimRight.String():
				// l: jump to next depth=0 turn.
				if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) || len(m.sessions[m.selectedIdx].Turns) == 0 {
					return m, nil
				}
				m.cursorTurnIdx = m.nextDepthZeroTurn(m.cursorTurnIdx)
				m.viewport.SetContent(m.renderDetail())
				m.scrollToTurn(m.cursorTurnIdx)
				return m, nil

			case defaults.KeySpace.String():
				// Toggle-mark the current turn for annotation.
				if m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
					turns := m.sessions[m.selectedIdx].Turns
					if m.cursorTurnIdx >= 0 && m.cursorTurnIdx < len(turns) {
						turnIdx := turns[m.cursorTurnIdx].Index
						if m.markedTurns[turnIdx] {
							delete(m.markedTurns, turnIdx)
						} else {
							m.markedTurns[turnIdx] = true
						}
						m.viewport.SetContent(m.renderDetail())
					}
				}
				return m, nil

			case defaults.KeyShiftSpace.String():
				// Shift+Space: begin/end range selection.
				if m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
					turns := m.sessions[m.selectedIdx].Turns
					if m.cursorTurnIdx >= 0 && m.cursorTurnIdx < len(turns) {
						curIdx := turns[m.cursorTurnIdx].Index
						if m.rangeStart < 0 {
							// First press: set range start.
							m.rangeStart = curIdx
						} else {
							// Second press: mark the range [start, end] inclusive.
							start, end := m.rangeStart, curIdx
							if start > end {
								start, end = end, start
							}
							for _, t := range turns {
								if t.Index >= start && t.Index <= end {
									m.markedTurns[t.Index] = true
								}
							}
							m.rangeStart = -1
						}
						m.viewport.SetContent(m.renderDetail())
					}
				}
				return m, nil

			case defaults.KeyFind.String():
				// 'f' key: reserved for find/filter; unimplemented in this version.
				return m, nil

			case defaults.KeyPageDown.String():
				m.viewport.HalfPageDown()
				return m, nil

			case defaults.KeyPageUp.String():
				m.viewport.HalfPageUp()
				return m, nil
			}
			m.viewport, cmd = m.viewport.Update(msg)
			return m, cmd
		}

		switch msg.String() {
		case defaults.KeyEnter.String():
			m.selectedIdx = m.table.Cursor()
			m.inDetail = true
			m.editorOpen = false
			m.markedTurns = make(map[int]bool)
			m.cursorTurnIdx = 0
			m.rangeStart = -1
			// Load turns for the selected session from the store.
			if m.pendingStore != nil && m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
				sess := m.sessions[m.selectedIdx]
				sid, sidErr := ingest.NewSessionID(string(sess.ID))
				if sidErr == nil {
					entries, err := m.pendingStore.ListEntries(context.Background(), sid)
					if err == nil {
						m.sessions[m.selectedIdx].Turns = api.EntriesToTurns(entries)
					}
				}
			}
			contentHeight := m.height - 4
			if contentHeight < 3 {
				contentHeight = 3
			}
			m.viewport = viewport.New(m.width, contentHeight)
			// Load pending for this session if store is available.
			if m.pendingStore != nil && m.selectedIdx >= 0 && m.selectedIdx < len(m.sessions) {
				sess := m.sessions[m.selectedIdx]
				m.pending, _ = m.pendingStore.ListPendingBySession(context.Background(), string(sess.ID))
			} else {
				m.pending = nil
			}
			m.viewport.SetContent(m.renderDetail())
			return m, nil
		}
	}

	if !m.inDetail {
		m.table, cmd = m.table.Update(msg)
	}
	return m, cmd
}

func (m SessionModel) View() string {
	if m.inDetail {
		content := m.viewport.View()
		if m.editorOpen {
			// Overlay the editor modal centered on the screen.
			editorView := m.editor.View()
			return lipgloss.Place(m.width, m.height, lipgloss.Center, lipgloss.Center, editorView)
		}
		return content
	}
	return m.table.View()
}

// InDetail returns whether the session detail view is active.
func (m SessionModel) InDetail() bool {
	return m.inDetail
}

func (m SessionModel) renderDetail() string {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) {
		return "No session selected"
	}

	s := m.sessions[m.selectedIdx]
	sessionID := string(s.ID)

	// Look up annotations for this session.
	var anns []schema.AnnotationSummary
	if m.annotations != nil {
		anns = m.annotations[sessionID]
	}

	var b strings.Builder

	b.WriteString(HeaderStyle.Render("Session Detail"))
	b.WriteString("\n\n")
	b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("ID:"), s.ID))
	b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("Provider:"), string(s.Harness)))
	b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("Start:"), s.StartTime.Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("End:"), s.EndTime.Format("2006-01-02 15:04:05")))
	b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("Duration:"), formatDuration(s.Metadata.Duration)))
	b.WriteString(fmt.Sprintf("  %s  %d in / %d out / %d total\n",
		DimStyle.Render("Tokens:"), s.Metadata.TokensIn, s.Metadata.TokensOut, s.Metadata.TotalTokens))

	// Session-level annotation badges.
	if sessAnns := sessionLevelAnnotations(anns); len(sessAnns) > 0 {
		b.WriteString("\n")
		b.WriteString(DimStyle.Render("  Annotations: "))
		b.WriteString(renderAnnotationBadges(sessAnns))
		b.WriteString("\n")
	}

	// Quality metrics section
	if s.Metadata.Quality != nil {
		q := s.Metadata.Quality
		outcome := derefOutcome(q.Outcome)
		b.WriteString("\n")
		b.WriteString(HeaderStyle.Render("Quality Metrics"))
		b.WriteString("\n\n")
		b.WriteString(fmt.Sprintf("  %s  %s\n", DimStyle.Render("Outcome:"), outcomeStyle(outcome).Render(outcome.String())))
		b.WriteString(fmt.Sprintf("  %s  %d files, %d lines\n", DimStyle.Render("Changes:"), derefInt(q.FilesTouched), derefInt(q.LinesChanged)))
		b.WriteString(fmt.Sprintf("  %s  %d loops (%s tokens wasted)\n",
			DimStyle.Render("Retries:"), derefInt(q.RetryLoops), formatNumber(derefInt(q.RetryTokensWasted))))
		b.WriteString(fmt.Sprintf("  %s  %.0f%%\n", DimStyle.Render("Signal Density:"), derefFloat(q.SignalDensity)*100))
		b.WriteString(fmt.Sprintf("  %s  %d within session\n", DimStyle.Render("Reverts:"), derefInt(q.WithinSessionReverts)))
		b.WriteString(fmt.Sprintf("  %s  %.0f / 100\n", DimStyle.Render("Spec Score:"), derefFloat(q.SpecQualityScore)*100))
		b.WriteString(fmt.Sprintf("  %s  %.0f%%\n", DimStyle.Render("Exploration:"), derefFloat(q.ExplorationRatio)*100))
		b.WriteString(fmt.Sprintf("  %s  %d directories\n", DimStyle.Render("Scope Breadth:"), derefInt(q.ScopeBreadth)))
		b.WriteString(fmt.Sprintf("  %s  %d turns\n", DimStyle.Render("Discovery:"), derefInt(q.DiscoveryTurns)))
	}

	b.WriteString("\n")
	b.WriteString(HeaderStyle.Render("Turns"))
	b.WriteString("\n\n")

	for turnArrayIdx, t := range s.Turns {
		content := t.Content
		if len(content) > defaults.ContentTruncLen {
			content = content[:defaults.ContentTruncLen] + "..."
		}
		// Show cursor (>) for focused turn, mark (*) for marked turns.
		prefix := "  "
		if turnArrayIdx == m.cursorTurnIdx {
			prefix = "> "
		}
		mark := " "
		if m.markedTurns[t.Index] {
			mark = "*"
		}
		b.WriteString(fmt.Sprintf("%s%sTurn %d [%s]: %s\n", prefix, mark, t.Index, AccentStyle.Render(string(t.Role)), content))

		// Render entry-level annotation badges inline with this turn.
		if entryAnns := annotationsForEntry(anns, t.Index); len(entryAnns) > 0 {
			b.WriteString("    ")
			b.WriteString(renderAnnotationBadges(entryAnns))
			b.WriteString("\n")
		}

		for _, tc := range t.ToolCalls {
			args := tc.Arguments
			if len(args) > defaults.ToolArgsTruncLen {
				args = args[:defaults.ToolArgsTruncLen] + "..."
			}
			b.WriteString(fmt.Sprintf("    → %s(%s)\n", tc.Name, args))
		}
	}

	// Pending annotations section.
	if len(m.pending) > 0 {
		b.WriteString("\n")
		b.WriteString(HeaderStyle.Render("Pending Annotations"))
		b.WriteString(DimStyle.Render("  (press 'c' to commit, 'd' to delete last)"))
		b.WriteString("\n\n")
		for i, p := range m.pending {
			b.WriteString(fmt.Sprintf("  [%d] %s = %s\n", i+1, DimStyle.Render(p.TypeID), p.Value))
		}
	}

	// Key bindings help for detail view.
	b.WriteString("\n")
	b.WriteString(HelpStyle.Render("  h/l: prev/next turn  Space: mark  Shift+Space: range  a: annotate  c: commit  d: delete  esc: back"))
	b.WriteString("\n")

	return b.String()
}

// commitPendingAnnotations POSTs all pending annotations to the backend batch endpoint
// in a single atomic request. Returns the count committed and the first error encountered.
func commitPendingAnnotations(pending []store.PendingAnnotationRecord, serverURL string) (int, error) {
	if len(pending) == 0 {
		return 0, nil
	}

	items := make([]schema.CreateAnnotationRequest, len(pending))
	for i, p := range pending {
		items[i] = schema.CreateAnnotationRequest{
			SessionID:     p.SessionID,
			TypeID:        p.TypeID,
			Value:         p.Value,
			AnnotatorName: "human-web",
		}
	}

	batchReq := schema.BatchCreateAnnotationsRequest{Annotations: items}
	body, err := json.Marshal(batchReq)
	if err != nil {
		return 0, fmt.Errorf(
			"commitPendingAnnotations (session.go): failed to marshal batch request: %w", err,
		)
	}

	resp, err := http.Post(
		serverURL+defaults.RouteAnnotationsBatch.String(),
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		return 0, fmt.Errorf(
			"commitPendingAnnotations (session.go): POST %s%s failed: %w — "+
				"check that the peasant web server is running and reachable at %s",
			serverURL, defaults.RouteAnnotationsBatch, err, serverURL,
		)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		var errResp schema.BatchCreateAnnotationsErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&errResp)
		return 0, fmt.Errorf(
			"commitPendingAnnotations (session.go): server returned %d (failingIndex=%d, error=%q) — "+
				"check the server logs at %s for details",
			resp.StatusCode, errResp.FailingIndex, errResp.Error, serverURL,
		)
	}

	return len(pending), nil
}

// outcomeStyle returns the appropriate style for a session outcome.
func outcomeStyle(outcome ingest.SessionOutcome) lipgloss.Style {
	switch outcome {
	case ingest.OutcomeResolved:
		return OutcomeResolvedStyle
	case ingest.OutcomePartial:
		return OutcomePartialStyle
	case ingest.OutcomeFailed:
		return OutcomeFailedStyle
	default:
		return DimStyle
	}
}

func formatDuration(d time.Duration) string {
	mins := d.Minutes()
	if mins >= 60 {
		return fmt.Sprintf("%dh %dm", int(mins)/60, int(mins)%60)
	}
	return fmt.Sprintf("%dm", int(mins))
}

// prevDepthZeroTurn returns the index into Turns of the previous depth=0 turn
// before the given position. Returns 0 if none found (clamp to start).
func (m SessionModel) prevDepthZeroTurn(current int) int {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) {
		return 0
	}
	turns := m.sessions[m.selectedIdx].Turns
	if len(turns) == 0 {
		return 0
	}
	for i := current - 1; i >= 0; i-- {
		if turns[i].Depth == 0 {
			return i
		}
	}
	return current // no previous depth=0 found; stay put
}

// nextDepthZeroTurn returns the index into Turns of the next depth=0 turn
// after the given position. Returns len-1 if none found (clamp to end).
func (m SessionModel) nextDepthZeroTurn(current int) int {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) {
		return 0
	}
	turns := m.sessions[m.selectedIdx].Turns
	if len(turns) == 0 {
		return 0
	}
	for i := current + 1; i < len(turns); i++ {
		if turns[i].Depth == 0 {
			return i
		}
	}
	return current // no next depth=0 found; stay put
}

// scrollToTurn sets the viewport Y offset to approximately show the given turn.
// Counts rendered lines up to the turn in the detail view.
func (m *SessionModel) scrollToTurn(turnArrayIdx int) {
	if m.selectedIdx < 0 || m.selectedIdx >= len(m.sessions) {
		return
	}
	turns := m.sessions[m.selectedIdx].Turns
	if turnArrayIdx < 0 || turnArrayIdx >= len(turns) {
		return
	}
	// Approximate: header is ~15 lines, each turn is ~2-3 lines.
	// Count lines in the rendered content up to the target turn.
	content := m.viewport.View()
	lines := strings.Split(content, "\n")

	turnPrefix := fmt.Sprintf("  Turn %d ", turns[turnArrayIdx].Index)
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " "), strings.TrimLeft(turnPrefix, " ")) {
			m.viewport.SetYOffset(i)
			return
		}
	}
}
