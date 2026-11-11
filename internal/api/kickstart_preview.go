package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// The preview of a forest row is a bounded list of typed lines. A project or a
// branch row describes the repository from the forest's own metadata. A session
// row shows the first page of its transcript, read in place from the harness
// source, so a session that was never imported previews like one that was.
const (
	// kickstartPreviewMaxLines bounds the lines of one preview, the closing
	// notice included.
	kickstartPreviewMaxLines = 200
	// kickstartPreviewMaxLineRunes bounds one line.
	kickstartPreviewMaxLineRunes = 400
)

// kickstartPreviewShortened closes a preview that filled its line bound, or
// that stopped where the source did not say how large it is.
const kickstartPreviewShortened = "preview shortened here. the full session ingests normally."

// kickstartPreviewReader reads the first page of a discovered session.
type kickstartPreviewReader interface {
	Previewable(sessionID string) bool
	Read(ctx context.Context, sessionID string) (discovery.SourcePreview, error)
}

var _ kickstartPreviewReader = (*discovery.SourceReader)(nil)

// kickstartPreviewTarget is a forest row resolved for a preview, copied out of
// the session so the transcript read runs without the store lock.
type kickstartPreviewTarget struct {
	node       onboarding.Node
	repository discovery.PreviewContext
	isRepo     bool
	listings   []onboarding.SessionListing
}

// previewTarget resolves one row of a session's forest. A row the forest does
// not hold is an honest 404.
func (st *kickstartSessions) previewTarget(id, nodeID string) (kickstartPreviewTarget, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.liveLocked(id)
	if err != nil {
		return kickstartPreviewTarget{}, err
	}
	for _, node := range sess.forest.Nodes {
		if node.ID != nodeID {
			continue
		}
		target := kickstartPreviewTarget{node: node, listings: sess.listings}
		if node.Kind == onboarding.NodeKindProject || node.Kind == onboarding.NodeKindBranch {
			target.repository, target.isRepo = discovery.PreviewContexts(sess.forest)[node.ID]
		}
		return target, nil
	}
	return kickstartPreviewTarget{}, newKickstartError(http.StatusNotFound, "kickstart_preview_row_not_found",
		"The row %q is not in the forest of setup session %q in internal/api.kickstartSessions.previewTarget, so there is nothing to preview. Nothing was changed. Reload the tree and preview a row it lists.", nodeID, id)
}

// preview builds the bounded lines of one row.
func (st *kickstartSessions) preview(ctx context.Context, id, nodeID string) (schema.LocalKickstartPreview, error) {
	target, err := st.previewTarget(id, nodeID)
	if err != nil {
		return schema.LocalKickstartPreview{}, err
	}
	lines := newPreviewLines()
	switch {
	case target.node.Kind == onboarding.NodeKindSession:
		lines.label(target.node.Label, target.node.ID)
		if err := st.sessionPreviewLines(ctx, target, lines); err != nil {
			return schema.LocalKickstartPreview{}, err
		}
	case target.isRepo:
		// The first line names the project, so the row's own label is not
		// repeated above it.
		for _, line := range discovery.RepositoryPreviewLines(target.repository, discovery.CleanPreviewValue) {
			lines.text(line)
		}
	default:
		lines.label(target.node.Label, target.node.ID)
	}
	return schema.LocalKickstartPreview{Lines: lines.finish()}, nil
}

func (st *kickstartSessions) sessionPreviewLines(ctx context.Context, target kickstartPreviewTarget, lines *previewLines) error {
	reader := st.env.reader(target.listings)
	sessionID := target.node.SessionID
	if sessionID == "" {
		sessionID = target.node.ID
	}
	page, err := reader.Read(ctx, sessionID)
	if errors.Is(err, discovery.ErrPreviewUnavailable) {
		lines.text("no transcript preview is available for this session; it still ingests normally.")
		return nil
	}
	if err != nil {
		return newKickstartError(http.StatusInternalServerError, "kickstart_preview_unreadable",
			"The preview of session %q could not be read in internal/api.kickstartSessions.preview: %v. Nothing was changed. The session still imports normally; check that its transcript is readable, then preview it again.", sessionID, err)
	}
	if len(page.Turns) == 0 {
		lines.text(discovery.PreviewNoTurnsNote)
	}
	for _, turn := range page.Turns {
		if !lines.hasRoom() {
			break
		}
		lines.spacer()
		lines.label(turn.Role.String(), "turn")
		for _, call := range turn.ToolCalls {
			lines.text("tool: " + call.Name)
		}
		for _, line := range strings.Split(turn.Content, "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			lines.text(line)
		}
	}
	if page.More {
		notice := discovery.PreviewCutNotice(page.ConsumedBytes, page.TotalBytes, page.More, false)
		if notice == "" {
			notice = kickstartPreviewShortened
		}
		lines.cut(notice)
	}
	return nil
}

// previewLines accumulates the lines of one preview under the bounds.
type previewLines struct {
	lines []schema.LocalKickstartLine
	full  bool
	// closed is set once a note that ends the preview has been added.
	closed bool
}

func newPreviewLines() *previewLines { return &previewLines{} }

func (p *previewLines) hasRoom() bool { return !p.full && len(p.lines) < kickstartPreviewMaxLines-1 }

func (p *previewLines) add(kind schema.LocalKickstartLineKind, text string) {
	if !p.hasRoom() {
		p.full = true
		return
	}
	if utf8.RuneCountInString(text) > kickstartPreviewMaxLineRunes {
		text = string([]rune(text)[:kickstartPreviewMaxLineRunes-1]) + "…"
	}
	p.lines = append(p.lines, schema.LocalKickstartLine{Kind: kind, Text: text})
}

func (p *previewLines) label(text, fallback string) {
	if strings.TrimSpace(text) == "" {
		text = fallback
	}
	p.add(schema.LocalKickstartLineLabel, text)
}

func (p *previewLines) text(text string) { p.add(schema.LocalKickstartLineText, text) }

func (p *previewLines) spacer() { p.add(schema.LocalKickstartLineSpacer, "") }

// cut closes a preview that stopped before the end of its source.
func (p *previewLines) cut(note string) {
	p.full = true
	p.closed = true
	p.lines = append(p.lines, schema.LocalKickstartLine{Kind: schema.LocalKickstartLineText, Text: note})
}

// finish returns the lines. A preview that filled its bound says so, and the
// notice is the last line.
func (p *previewLines) finish() []schema.LocalKickstartLine {
	if p.full && !p.closed {
		p.cut(kickstartPreviewShortened)
	}
	if p.lines == nil {
		return []schema.LocalKickstartLine{}
	}
	return p.lines
}
