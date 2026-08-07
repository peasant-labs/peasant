package kickstart

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/transcriptview"
)

// SessionTurnsFunc reads the recorded TURNS of one session from the local
// store, in the canonical full-content form the session viewer uses.
//
// It returns no turns (not an error) for a session the store does not hold
// yet, which is the normal case during onboarding, and an error only when the
// read itself failed. It is the read seam the preview binds to: the mounted
// flow fills it from the store's session read path, and tests fill it with
// recorded turns directly.
type SessionTurnsFunc func(sessionID string) ([]ingest.Turn, error)

// notImportedBody is what the preview says for a discovered session the local
// store does not hold yet - the normal case on a first run.
const notImportedBody = "not imported yet. peasant shows the transcript here after it imports this session."

// notASessionBody is what the preview says for a row that is not a session (a
// project or a branch).
const notASessionBody = "select a session to preview it."

// ListingPreview is the preview source the selection tree renders beside its
// rows: for the highlighted session it shows the WHOLE recorded transcript -
// role-tagged turns, prose and fenced code laid out through the shared
// markdown renderer - under a short header naming the session's project,
// harness, and branch.
//
// The header comes from the SAME discovery listing the tree was folded from,
// and the turns come from the local store. A row that is not a session, and a
// session the store does not hold yet, each get a plain explanation rather than
// a blank pane or an error.
//
// It loads STRUCTURE, not text: [kit.BodySource] hands it no width, so nothing
// here can lay anything out. Layout happens per draw, at the pane's current
// width, inside [transcriptview.Document].
type ListingPreview struct {
	byID     map[string]ftue.SessionListing
	turns    SessionTurnsFunc
	renderer *transcriptview.Renderer
	th       theme.Theme
}

// NewListingPreview builds the selection step's preview over the discovery
// listing and a store read seam. A nil turns func is the no-store first run:
// every session still gets its header and is named as not yet imported.
func NewListingPreview(th theme.Theme, sessions []ftue.SessionListing, turns SessionTurnsFunc) *ListingPreview {
	byID := make(map[string]ftue.SessionListing, len(sessions))
	for _, sess := range sessions {
		if sess.SessionID != "" {
			byID[sess.SessionID] = sess
		}
	}
	return &ListingPreview{
		byID:     byID,
		turns:    turns,
		renderer: transcriptview.New(th),
		th:       th,
	}
}

var _ kit.BodySource = (*ListingPreview)(nil)

// Body implements kit.BodySource. It is called off the UI goroutine, so the
// store read here never blocks tree navigation.
func (p *ListingPreview) Body(id string) (kit.PreviewBody, error) {
	sess, ok := p.byID[id]
	if !ok {
		return sessionBody{th: p.th, note: notASessionBody}, nil
	}
	body := sessionBody{th: p.th, header: headerLines(sess)}
	if p.turns == nil {
		body.note = notImportedBody
		return body, nil
	}
	recorded, err := p.turns(id)
	if err != nil {
		return nil, err
	}
	if len(recorded) == 0 {
		body.note = notImportedBody
		return body, nil
	}
	body.transcript = p.renderer.Document(recorded)
	return body, nil
}

// headerLines names the highlighted session in the pane's own lowercase chrome.
//
// It does NOT repeat the session title. A title is derived from the session's
// first user message, and that message is the first thing the transcript below
// renders - printing both put the same sentence on screen twice, which is what
// this header replaced.
func headerLines(sess ftue.SessionListing) []string {
	lines := []string{fmt.Sprintf("harness: %s", harnessDisplayName(sess.Harness))}
	if sess.ProjectName != "" {
		lines = append(lines, fmt.Sprintf("project: %s", sess.ProjectName))
	}
	if sess.Branch != "" {
		lines = append(lines, fmt.Sprintf("branch: %s", sess.Branch))
	}
	return lines
}

// sessionBody is one loaded preview: the header chrome, and then EITHER the
// recorded transcript or a plain explanation of why there is none.
//
// It carries both because they are the same pane in two states, and a caller
// that had to switch between two body types would be re-deriving that at every
// draw.
type sessionBody struct {
	th         theme.Theme
	header     []string
	transcript transcriptview.Document
	note       string
}

var _ kit.PreviewBody = sessionBody{}

// headerSeparator ends the pane's own chrome: one blank line between what the
// pane wrote about the session and the conversation the session recorded.
const headerSeparator = "\n\n"

// Render implements kit.PreviewBody, laying the preview out at the pane's
// CURRENT width.
func (b sessionBody) Render(width int) string {
	if width <= 0 {
		return b.plain()
	}
	styles := b.th.Styles()
	var parts []string
	if len(b.header) > 0 {
		head := make([]string, 0, len(b.header))
		for _, line := range b.header {
			head = append(head, styles.Muted.Render(ansi.Truncate(line, width, "")))
		}
		parts = append(parts, strings.Join(head, "\n"))
	}
	if body := b.transcript.Render(width); body != "" {
		parts = append(parts, body)
	} else if b.note != "" {
		// The pane hands its body lines to the viewport UNSTYLED, so a body that
		// does not color itself is drawn in whatever the terminal's default ink
		// happens to be rather than the theme's. The explanation is prose the
		// pane wrote, so it takes the same body ink the rest of the prose does.
		parts = append(parts, styles.Base.Render(ansi.Wrap(b.note, width, "")))
	}
	return strings.Join(parts, headerSeparator)
}

// plain returns the body's raw, unstyled text for the width<=0 case, where no
// layout is possible: the header lines joined as-is, then the rendered
// transcript or the plain note - mirroring [textBody.Render]'s width<=0
// short-circuit.
func (b sessionBody) plain() string {
	var parts []string
	if len(b.header) > 0 {
		parts = append(parts, strings.Join(b.header, "\n"))
	}
	if body := b.transcript.Render(0); body != "" {
		parts = append(parts, body)
	} else if b.note != "" {
		parts = append(parts, b.note)
	}
	return strings.Join(parts, headerSeparator)
}

// sessionItem is a preview-list row carrying a stable session ID distinct from
// its label, so kit.PreviewSplit keys previews (and its stale-result guard) on
// the session ID even when two rows render the same title.
type sessionItem struct {
	id    string
	label string
}

// FilterValue implements kit.ListItem.
func (i sessionItem) FilterValue() string { return i.label }

// ID implements kit.IdentifiedItem, the identity PreviewSplit requests previews
// by.
func (i sessionItem) ID() string { return i.id }

var _ kit.IdentifiedItem = sessionItem{}

// NewSessionPreview mounts a kit.PreviewSplit over the given sessions with a
// side preview loaded from source. The left pane is a plain kit.List (its Focus
// is side-effect-free, so kit.PreviewSplit.setActive dropping the focus command
// on a tab toggle loses nothing); the initial preview load command IS threaded
// back to the caller here, since the mount is the one focus-start whose command
// must not be lost. Drive the returned command from the program's Init/entry.
func NewSessionPreview(th theme.Theme, sessions []ftue.SessionListing, source kit.BodySource) (kit.PreviewSplit, tea.Cmd) {
	items := make([]kit.ListItem, 0, len(sessions))
	for _, sess := range sessions {
		items = append(items, sessionItem{id: sess.SessionID, label: sessionLabel(sess)})
	}
	list := kit.NewList(th, items)
	left := kit.NewListLeftPane(list)
	split := kit.NewPreviewSplitWithBodies(th, left, source)

	// Thread both the focus-start command and the first preview load command,
	// so neither the cursor-start side-effect nor the initial async load is lost
	// (the caveat kit.PreviewSplit.setActive would otherwise drop on toggle).
	focusCmd := split.Focus()
	loadCmd := split.Load()
	return split, tea.Batch(focusCmd, loadCmd)
}
