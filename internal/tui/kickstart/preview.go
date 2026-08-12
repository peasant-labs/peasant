package kickstart

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/tui/ftue"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/mdrender"
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

// ListingPreviewKind identifies the non-session row a scanner preview describes.
type ListingPreviewKind uint8

const (
	ListingPreviewUnknown ListingPreviewKind = iota
	ListingPreviewProject
	ListingPreviewBranch
)

// ListingPreviewContext is the resolved repository metadata shown for a project
// or branch row. It is built from the same forest the selection tree renders.
type ListingPreviewContext struct {
	Kind           ListingPreviewKind
	Project        string
	Harnesses      []string
	Remotes        []string
	GitDirectories []string
	ClonePaths     []string
	Branches       []string
	Branch         string
	SessionCount   int
}

// ListingPreviewContextSource reads project and branch metadata by tree row ID.
// ScannerTreeSource implements it from the latest successfully loaded forest.
type ListingPreviewContextSource interface {
	ListingPreviewContext(id string) (ListingPreviewContext, bool)
}

// ListingPreviewOption configures a ListingPreview.
type ListingPreviewOption func(*ListingPreview)

// WithListingPreviewContextSource enables project and branch detail bodies from
// the exact scanner forest that supplied the highlighted row.
func WithListingPreviewContextSource(source ListingPreviewContextSource) ListingPreviewOption {
	return func(preview *ListingPreview) {
		preview.contexts = source
	}
}

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
	contexts ListingPreviewContextSource
}

// NewListingPreview builds the selection step's preview over the discovery
// listing and a store read seam. A nil turns func is the no-store first run:
// every session still gets its header and is named as not yet imported.
func NewListingPreview(th theme.Theme, sessions []ftue.SessionListing, turns SessionTurnsFunc, opts ...ListingPreviewOption) *ListingPreview {
	byID := make(map[string]ftue.SessionListing, len(sessions))
	for _, sess := range sessions {
		if sess.SessionID != "" {
			byID[sess.SessionID] = sess
		}
	}
	preview := &ListingPreview{
		byID:     byID,
		turns:    turns,
		renderer: transcriptview.New(th),
		th:       th,
	}
	for _, opt := range opts {
		opt(preview)
	}
	return preview
}

var _ kit.BodySource = (*ListingPreview)(nil)

// Body implements kit.BodySource. It is called off the UI goroutine, so the
// store read here never blocks tree navigation.
func (p *ListingPreview) Body(id string) (kit.PreviewBody, error) {
	sess, ok := p.byID[id]
	if !ok {
		if p.contexts != nil {
			if context, found := p.contexts.ListingPreviewContext(id); found {
				return listingContextBody{th: p.th, lines: listingContextLines(context)}, nil
			}
		}
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

func cloneListingPreviewContext(context ListingPreviewContext) ListingPreviewContext {
	context.Harnesses = append([]string(nil), context.Harnesses...)
	context.Remotes = append([]string(nil), context.Remotes...)
	context.GitDirectories = append([]string(nil), context.GitDirectories...)
	context.ClonePaths = append([]string(nil), context.ClonePaths...)
	context.Branches = append([]string(nil), context.Branches...)
	return context
}

func listingContextLines(context ListingPreviewContext) []string {
	lines := []string{"project: " + previewMetadataValue(context.Project)}
	if context.Kind == ListingPreviewBranch && context.Branch != "" {
		lines = append(lines, "branch: "+previewMetadataValue(context.Branch))
	}
	if len(context.Harnesses) > 0 {
		harnesses := make([]string, 0, len(context.Harnesses))
		for _, harness := range context.Harnesses {
			harnesses = append(harnesses, harnessDisplayName(harness))
		}
		label := "harness: "
		if len(harnesses) > 1 {
			label = "harnesses: "
		}
		lines = append(lines, label+strings.Join(harnesses, ", "))
	}
	if len(context.Remotes) > 0 {
		label := "remote: "
		if len(context.Remotes) > 1 {
			label = "remotes: "
		}
		values := make([]string, 0, len(context.Remotes))
		for _, remote := range context.Remotes {
			values = append(values, previewMetadataValue(remote))
		}
		lines = append(lines, label+strings.Join(values, ", "))
	}
	if len(context.GitDirectories) == 1 {
		lines = append(lines, "git directory: "+previewMetadataValue(context.GitDirectories[0]))
	} else if len(context.GitDirectories) > 1 {
		lines = appendPreviewList(lines, "git directories", context.GitDirectories)
	}
	lines = appendPreviewList(lines, "worktrees", context.ClonePaths)
	if context.Kind == ListingPreviewProject {
		lines = appendPreviewList(lines, "branches", context.Branches)
	}
	lines = append(lines, fmt.Sprintf("sessions: %d", context.SessionCount))
	return lines
}

func appendPreviewList(lines []string, label string, values []string) []string {
	if len(values) == 0 {
		return lines
	}
	lines = append(lines, fmt.Sprintf("%s: %d", label, len(values)))
	for _, value := range values {
		lines = append(lines, "  "+previewMetadataValue(value))
	}
	return lines
}

func previewMetadataValue(value string) string {
	return strings.Join(strings.Fields(mdrender.Sanitize(value)), " ")
}

type listingContextBody struct {
	th    theme.Theme
	lines []string
}

var _ kit.PreviewBody = listingContextBody{}

func (b listingContextBody) Render(width int) string {
	if width <= 0 {
		return strings.Join(b.lines, "\n")
	}
	styles := b.th.Styles()
	lines := make([]string, 0, len(b.lines))
	for _, line := range b.lines {
		lines = append(lines, styles.Base.Render(ansi.Wrap(line, width, "")))
	}
	return strings.Join(lines, "\n")
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
