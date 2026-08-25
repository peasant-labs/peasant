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

// SessionFirstTurnsFunc reads a quickly-available LEADING slice of one
// session's turns and reports whether more of the session follows.
//
// It is the two-step half of [SessionTurnsFunc], and it exists because reading
// a very long session out of a provider database takes seconds while reading
// the first part of it takes a fraction of one. The preview paints the slice at
// once and then asks SessionTurnsFunc for the result it finally shows.
//
// A false more means the slice IS the whole preview, so no second read runs.
// Its error contract matches SessionTurnsFunc: no turns for a session there is
// nothing to read for, an error only when the read itself failed.
type SessionFirstTurnsFunc func(sessionID string) (turns []ingest.Turn, more bool, err error)

// SessionMoreTurnsFunc extends the loaded turns of one session by the next
// chunk and reports whether the session continues past it.
//
// It is what a scroll to the bottom of the preview pane asks for, and it exists
// because a session too long to read whole would otherwise simply stop at the
// bound the first read used. It returns EVERYTHING loaded so far, not the chunk
// alone, so the pane replaces its body and every line already on screen keeps
// its place.
//
// Its error contract matches SessionTurnsFunc: no turns for a session there is
// nothing more to read for, an error only when the read itself failed.
type SessionMoreTurnsFunc func(sessionID string) (turns []ingest.Turn, more bool, err error)

// SessionHasMoreFunc reports whether the loaded preview of one session has more
// of that session behind it. The pane asks on its own goroutine after a load
// lands, so it must answer from memory and read nothing.
type SessionHasMoreFunc func(sessionID string) bool

// SessionPreviewNoticeFunc reports a plain sentence the preview must show ABOVE
// the turns it already loaded, or the empty string when there is nothing to
// say. It exists because a preview can be complete in itself yet still stand
// for less than the whole session: a very long session is read under a byte
// bound, so the pane must name what it left out rather than silently showing a
// prefix. It is read only after the turns load, so the sentence always
// describes the turns on screen.
type SessionPreviewNoticeFunc func(sessionID string) string

// EmptySessionPreview is the imported-but-empty session state. SourceJSON is a
// bounded JSONL excerpt that ListingPreview renders through the JSON highlighter.
type EmptySessionPreview struct {
	Note       string
	SourceJSON string
}

// EmptySessionBodyFunc distinguishes a session that the store does not hold
// from one it holds without renderable turns. The mounted preview uses the
// latter to show source evidence instead of falsely calling it unimported.
type EmptySessionBodyFunc func(sessionID string) (preview EmptySessionPreview, imported bool, err error)

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

// WithEmptySessionBody configures the imported-but-empty state. It is called
// only after SessionTurnsFunc has confirmed that the local store holds the
// session but returned no renderable turns.
func WithEmptySessionBody(body EmptySessionBodyFunc) ListingPreviewOption {
	return func(preview *ListingPreview) {
		preview.emptyBody = body
	}
}

// WithSessionPreviewNotice supplies the sentence the preview shows above the
// turns of a session it could only read in part.
func WithSessionPreviewNotice(notice SessionPreviewNoticeFunc) ListingPreviewOption {
	return func(preview *ListingPreview) {
		preview.notice = notice
	}
}

// WithSessionFirstTurns supplies the quick leading read the preview paints
// before the whole session is read. Without it the preview keeps its
// single-step behavior: the pane shows nothing until the whole read finishes.
func WithSessionFirstTurns(first SessionFirstTurnsFunc) ListingPreviewOption {
	return func(preview *ListingPreview) {
		preview.firstTurns = first
	}
}

// WithSessionMoreTurns supplies the scrolled continuation. Without it the
// preview keeps its existing behavior: it stops at what the first read loaded
// and says so.
func WithSessionMoreTurns(more SessionMoreTurnsFunc, hasMore SessionHasMoreFunc) ListingPreviewOption {
	return func(preview *ListingPreview) {
		preview.moreTurns = more
		preview.hasMore = hasMore
	}
}

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

// emptySourceBody is what the preview says for a discovered session whose
// harness transcript holds no readable turns. The pane can reach that
// transcript, so it must not repeat the not-imported explanation, which would
// blame the import for an empty source.
const emptySourceBody = "no readable turns are in this session's transcript yet."

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
	byID      map[string]ftue.SessionListing
	turns     SessionTurnsFunc
	emptyBody EmptySessionBodyFunc
	renderer  *transcriptview.Renderer
	th        theme.Theme
	contexts  ListingPreviewContextSource
	notice    SessionPreviewNoticeFunc
	// firstTurns is the optional quick leading read. When it is nil the
	// preview loads in one step.
	firstTurns SessionFirstTurnsFunc
	// moreTurns and hasMore are the optional scrolled continuation. Both nil is
	// a preview that ends where its first read ended.
	moreTurns SessionMoreTurnsFunc
	hasMore   SessionHasMoreFunc
	// extended names the sessions the reader has actually scrolled. It keeps a
	// preview drawing every turn once it runs out of session to load: without
	// it, the last chunk landing would take the draw bound back and hide most of
	// what the reader had just spent scrolls loading.
	extended map[string]bool
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
		extended: map[string]bool{},
	}
	for _, opt := range opts {
		opt(preview)
	}
	return preview
}

var (
	_ kit.BodySource            = (*ListingPreview)(nil)
	_ kit.ProgressiveBodySource = (*ListingPreview)(nil)
	_ kit.ContinuableBodySource = (*ListingPreview)(nil)
)

// Body implements kit.BodySource. It is called off the UI goroutine, so the
// store read here never blocks tree navigation.
func (p *ListingPreview) Body(id string) (kit.PreviewBody, error) {
	body, _, err := p.load(id, func(sessionID string) ([]ingest.Turn, bool, error) {
		recorded, err := p.turns(sessionID)
		return recorded, false, err
	})
	return body, err
}

// FirstBody implements kit.ProgressiveBodySource: it paints the quick leading
// slice of a session while the whole read is still running.
//
// Without a first-turns reader it is the single-step load, so mounting this
// preview over a source that cannot read a slice costs nothing and changes
// nothing.
func (p *ListingPreview) FirstBody(id string) (kit.PreviewBody, bool, error) {
	if p.firstTurns == nil {
		body, err := p.Body(id)
		return body, false, err
	}
	return p.load(id, p.firstTurns)
}

// Continuable implements kit.ContinuableBodySource. It answers from what the
// reader already loaded, so it never touches the source.
func (p *ListingPreview) Continuable(id string) bool {
	if p.hasMore == nil {
		return false
	}
	if _, ok := p.byID[id]; !ok {
		return false
	}
	return p.hasMore(id)
}

// MoreBody implements kit.ContinuableBodySource: it extends the preview by the
// next chunk of the session when the reader scrolls toward the end of it.
//
// The body it returns holds every turn loaded so far, so it is the same preview
// with more below it rather than a different one.
func (p *ListingPreview) MoreBody(id string) (kit.PreviewBody, bool, error) {
	if p.moreTurns == nil {
		body, err := p.Body(id)
		return body, false, err
	}
	var continues bool
	p.extended[id] = true
	body, _, err := p.load(id, func(sessionID string) ([]ingest.Turn, bool, error) {
		turns, more, err := p.moreTurns(sessionID)
		continues = more
		// The read reports `more` to THIS function, not to load: load reads it
		// as "a further step follows within this load", which is the two-step
		// slice contract, and a continuation is one step on its own.
		return turns, false, err
	})
	return body, continues, err
}

// load builds one preview for id from the given read, and reports whether that
// read left more of the session to come.
//
// The truncation notice is attached ONLY to a body no further read follows.
// The notice names what the WHOLE preview read left out, so putting it on a
// leading slice would state a bound the slice was not read under.
func (p *ListingPreview) load(id string, read func(string) ([]ingest.Turn, bool, error)) (kit.PreviewBody, bool, error) {
	sess, ok := p.byID[id]
	if !ok {
		if p.contexts != nil {
			if context, found := p.contexts.ListingPreviewContext(id); found {
				return listingContextBody{th: p.th, lines: listingContextLines(context)}, false, nil
			}
		}
		return sessionBody{th: p.th, note: notASessionBody}, false, nil
	}
	body := sessionBody{th: p.th, header: headerLines(sess)}
	if p.turns == nil {
		// No reader at all: nothing can say more than that the store does not
		// hold the session yet.
		body.note = notImportedBody
		return body, false, nil
	}
	recorded, more, err := read(id)
	if err != nil {
		return nil, false, err
	}
	if len(recorded) == 0 {
		if p.emptyBody != nil {
			preview, imported, err := p.emptyBody(id)
			if err != nil {
				return nil, false, err
			}
			if imported {
				body.note = preview.Note
				body.rawJSON = preview.SourceJSON
				return body, false, nil
			}
		}
		body.note = emptyNote(sess)
		return body, false, nil
	}
	body.transcript = p.document(id, recorded)
	if p.notice != nil && !more {
		body.notice = p.notice(id)
	}
	return body, more, nil
}

// document binds the loaded turns to the renderer, choosing the draw bound by
// how those turns got here.
//
// A preview that can be EXTENDED holds turns because the reader scrolled for
// them, so every one of them is drawn: bounding the draw would make the pane
// stop growing while the loader kept working, which is the same thing as the
// feature not working. A preview handed a whole recorded session keeps the
// standing bound, because nothing there was asked for turn by turn.
//
// The choice is made per session rather than per preview: a store-backed
// session in the same list is bounded even while a harness-read one beside it
// is not.
func (p *ListingPreview) document(id string, turns []ingest.Turn) transcriptview.Document {
	if p.Continuable(id) || p.extended[id] {
		return p.renderer.UnboundedDocument(turns)
	}
	return p.renderer.Document(turns)
}

// emptyNote explains a session that produced no turns. A session whose harness
// transcript discovery found is readable now, so its empty pane is about the
// transcript. A session with no known transcript is simply not imported yet.
func emptyNote(sess ftue.SessionListing) string {
	if sess.Source.IsZero() {
		return notImportedBody
	}
	return emptySourceBody
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
	// notice stands WITH the transcript rather than instead of it: it says what
	// the loaded turns leave out. note replaces a transcript that is not there.
	notice  string
	note    string
	rawJSON string
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
	if b.notice != "" {
		parts = append(parts, styles.Muted.Render(ansi.Wrap(b.notice, width, "")))
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
	if b.rawJSON != "" {
		parts = append(parts, mdrender.New(b.th).Render("```json\n"+b.rawJSON+"\n```", width))
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
	if b.notice != "" {
		parts = append(parts, b.notice)
	}
	if body := b.transcript.Render(0); body != "" {
		parts = append(parts, body)
	} else if b.note != "" {
		parts = append(parts, b.note)
	}
	if b.rawJSON != "" {
		parts = append(parts, b.rawJSON)
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
