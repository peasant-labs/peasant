package api

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/onboarding"
	"github.com/peasant-labs/schema"
)

// The preview of a forest row is a bounded list of typed lines. A project or a
// branch row describes the repository from the forest's own metadata. Session
// transcripts are served by the socket capture, not this HTTP preview.
const (
	// kickstartPreviewMaxLines bounds the lines of one preview, the closing
	// notice included.
	kickstartPreviewMaxLines = 200
	// kickstartPreviewMaxLineRunes bounds one line.
	kickstartPreviewMaxLineRunes = 400
)

// kickstartPreviewTarget is a forest row resolved for a preview, copied out of
// the session so a socket capture can run without the store lock.
type kickstartPreviewTarget struct {
	node       onboarding.Node
	repository discovery.PreviewContext
	isRepo     bool
	listings   []onboarding.SessionListing
	scanGen    uint64
}

// previewTarget resolves one row of a session's forest. A row the forest does
// not hold is an honest 404.
func (st *kickstartSessions) previewTarget(id, nodeID string) (kickstartPreviewTarget, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	sess, err := st.liveLocked(id)
	if err != nil {
		// Unlike other setup routes, a preview's identifiers may contain source
		// values. Keep its error actionable without echoing those identifiers.
		return kickstartPreviewTarget{}, newKickstartError(schema.LocalKickstartErrorSessionNotFound,
			"The setup session for this preview is not available in internal/api.kickstartSessions.previewTarget: it was never opened, was saved, or expired. Nothing was changed. Open a session again with POST %s.", defaults.RouteKickstartSessions)
	}
	for _, node := range sess.forest.Nodes {
		if node.ID != nodeID {
			continue
		}
		target := kickstartPreviewTarget{node: node, listings: sess.listings, scanGen: sess.scanGen}
		if node.Kind == onboarding.NodeKindProject || node.Kind == onboarding.NodeKindBranch {
			target.repository, target.isRepo = discovery.PreviewContexts(sess.forest)[node.ID]
		}
		return target, nil
	}
	return kickstartPreviewTarget{}, newKickstartError(schema.LocalKickstartErrorPreviewRowNotFound,
		"The requested row is not in the setup session's forest in internal/api.kickstartSessions.previewTarget, so there is nothing to preview. Nothing was changed. Reload the tree and preview a row it lists.")
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
		lines.text("open this session in the local preview to read its transcript")
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
		p.cut(discovery.PreviewShortenedNote)
	}
	if p.lines == nil {
		return []schema.LocalKickstartLine{}
	}
	return p.lines
}
