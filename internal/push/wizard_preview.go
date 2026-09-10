package push

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/x/ansi"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/peasant/internal/tui/kit"
	"github.com/peasant-labs/peasant/internal/tui/theme"
	"github.com/peasant-labs/peasant/internal/tui/transcriptview"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// StoredEntriesFunc reads the indexed entries of one session from the local
// store. It is the SAME read the push pipeline publishes from, which is what
// lets the preview show the transcript the push will send rather than a second
// approximation of it.
//
// It returns the AVAILABLE stored content: no entries for a verified empty
// capture, and the bounded projection when the full capture is missing or
// incomplete. Readiness is not its question — publication readiness is enforced
// at the publish action, in Pipeline.preflight, because a preview that first
// demands a complete capture cannot show the session the user has to repair.
// An unreadable or unknown-format projection still returns an error: available
// content can be partial, never invented.
type StoredEntriesFunc func(sessionID string) (StoredContent, error)

// StoredContent is one preview read's answer: the available stored entries, and
// whether they stand for only part of the session.
//
// Partial is carried BESIDE the entries because it cannot be derived from them.
// A bounded projection of a long session and a complete short session both come
// back as "some entries", so a pane handed entries alone can only guess, and it
// guessed "this is the whole session" every time. The store already proves the
// difference through the session's capture status; this is that proof, reaching
// the one screen that has to state it.
//
// It stays inside the TUI: no wire, JSON or WebSocket payload reports it.
type StoredContent struct {
	// Entries is the available stored content, empty for a session the store
	// holds nothing for.
	Entries []schema.SessionEntry
	// Partial is true when the stored capture of this session is not complete,
	// so the entries above are as much of it as the database can prove it has.
	Partial bool
}

// PublishedTurnsFunc returns one session's turns AS THEY WILL BE PUBLISHED:
// read from the local store and redacted by the same redactor, over the same
// entries, that the push applies on the way out.
//
// It is the read seam the selection preview binds to. The mounted command fills
// it from the store and the push redactor; a test fills it with recorded turns
// directly.
type PublishedTurnsFunc func(sessionID string) (PublishedTranscript, error)

// PublishedTranscript is one session's turns as they will be published, with
// the same partial-capture flag the stored read reported. The pane needs both
// in one answer: it draws the turns and, above them, the line that says the
// turns are only part of the session.
type PublishedTranscript struct {
	Turns   []ingest.Turn
	Partial bool
}

// NewPublishedTurns builds the preview read over a stored-entry reader and the
// redactor the push runs with.
//
// The redactor is required. RedactEntries leaves the entries as recorded when
// it is handed nil, so a nil redactor here would draw the recorded text on the
// screen that promises the published text. This fails closed instead: the pane
// reports that it cannot show the published transcript, and shows nothing.
func NewPublishedTurns(entries StoredEntriesFunc, redactor redact.JSONRedactor) PublishedTurnsFunc {
	return func(sessionID string) (PublishedTranscript, error) {
		if entries == nil || redactor == nil {
			return PublishedTranscript{}, fmt.Errorf(
				"push preview: the transcript of session %s cannot be shown as it will be published.\n"+
					"What went wrong: the preview was mounted without a stored-entry reader or without the push redactor.\n"+
					"Where: push.NewPublishedTurns, drawing the selection page of the push wizard.\n"+
					"When: while the user chooses sessions, before anything was uploaded.\n"+
					"Means: the pane can only show recorded text, which is not what a push sends.\n"+
					"Fix: mount the wizard with the store and the redactor the push runs with",
				sessionID)
		}
		stored, err := entries(sessionID)
		if err != nil {
			return PublishedTranscript{}, err
		}
		// The partial flag survives an empty read: a session whose capture broke
		// before any entry was stored is still a partial session, and the pane
		// says so rather than calling it simply unrecorded.
		if len(stored.Entries) == 0 {
			return PublishedTranscript{Partial: stored.Partial}, nil
		}
		redacted, err := RedactEntries(redactor, stored.Entries)
		if err != nil {
			return PublishedTranscript{}, err
		}
		turns, err := transcript.EntriesToTurnsValidated(redacted)
		if err != nil {
			return PublishedTranscript{}, err
		}
		return PublishedTranscript{Turns: turns, Partial: stored.Partial}, nil
	}
}

// Preview chrome. Every line the pane writes itself is lower-case, like the
// rest of the wizard.
const (
	previewNoSessionNote  = "select a session to see what this push sends."
	previewNoTranscript   = "no transcript is stored for this session yet."
	previewSelectedNote   = "selected: this session is in the push."
	previewUnselectedNote = "not selected: this session stays on your machine."
	previewWithheldNote   = "withheld: this branch matches more than one project, so peasant cannot tell which one records it. this session stays out of the push."
	previewProjectHint    = "press space to select every session in this project."
	// previewNeedsIngestNote sits ABOVE the available transcript rather than
	// replacing it: it states what publication still needs, and never claims the
	// stored content cannot be shown.
	previewNeedsIngestNote = "publication needs database metadata and matching entries.\n\nrun peasant ingest with the retained source available, then retry. nothing has been uploaded."
)

// wizardPreview is the split's right pane: for the highlighted session, a short
// header naming it and then the transcript AS IT WILL BE PUBLISHED. A project
// row describes the group instead.
//
// It says nothing about the STORED copy's redaction record. That record
// described a file on this machine, not the upload, and a reader had no action
// to take on it. The transcript below the header is the honest form of the same
// question: it is the text the push sends.
//
// It loads STRUCTURE, not text: [kit.BodySource] hands it no width, so the
// layout happens per draw, at the pane's current width.
type wizardPreview struct {
	sessions []PushWizardSession
	turns    PublishedTurnsFunc
	renderer *transcriptview.Renderer
	th       theme.Theme
}

var _ kit.BodySource = wizardPreview{}

// wizardPreviewSource builds the pane the selection page mounts. It is the one
// construction path, so a test previews through the same object the wizard
// draws with.
func wizardPreviewSource(sessions []PushWizardSession, turns PublishedTurnsFunc, th theme.Theme) wizardPreview {
	return wizardPreview{sessions: sessions, turns: turns, renderer: transcriptview.New(th), th: th}
}

// Body implements kit.BodySource. It is called off the UI goroutine, so the
// store read and the redaction here never block tree navigation, and a result
// for a row the user has already left is dropped by the split.
func (p wizardPreview) Body(id string) (kit.PreviewBody, error) {
	if strings.HasPrefix(id, projectNodePrefix) {
		return previewBody{th: p.th, header: p.projectLines(strings.TrimPrefix(id, projectNodePrefix))}, nil
	}
	for _, s := range p.sessions {
		if s.Row.SessionID != id {
			continue
		}
		body := previewBody{th: p.th, header: sessionHeaderLines(s)}
		if s.NeedsIngest {
			// A session that cannot be published can still be READ. Gating the
			// pane on publication readiness took away half of what the previewer
			// is for: the user could not see the transcript they were being asked
			// to repair. The note says what publishing still needs; the
			// transcript below it is the available stored content.
			body.note = previewNeedsIngestNote
		}
		if p.turns == nil {
			if body.note == "" {
				body.note = previewNoTranscript
			}
			return body, nil
		}
		recorded, err := p.turns(id)
		if err != nil {
			return nil, err
		}
		if recorded.Partial {
			// Said LAST, so it sits directly above the transcript it describes.
			// The publication note above it answers a different question - what
			// this session still needs before it can be pushed - and a reader
			// needs both when both are true.
			body.note = joinNotes(body.note, transcriptview.PartialPreviewNote)
		}
		if len(recorded.Turns) == 0 {
			if body.note == "" {
				body.note = previewNoTranscript
			}
			return body, nil
		}
		body.transcript = p.renderer.Document(recorded.Turns)
		return body, nil
	}
	return previewBody{th: p.th, note: previewNoSessionNote}, nil
}

// joinNotes stacks the pane's notes in the order they were added, so adding a
// second note never silently replaces the first.
func joinNotes(existing, added string) string {
	if existing == "" {
		return added
	}
	return existing + "\n\n" + added
}

// projectLines describes one project group.
func (p wizardPreview) projectLines(project string) []string {
	total, selected := 0, 0
	for _, s := range p.sessions {
		if projectLabelOf(s) != project {
			continue
		}
		total++
		if !s.Locked && !s.NeedsIngest && s.Action == PushWithRedaction {
			selected++
		}
	}
	return []string{
		"project: " + project,
		fmt.Sprintf("sessions: %d", total),
		fmt.Sprintf("selected: %d", selected),
		"",
		previewProjectHint,
	}
}

// sessionHeaderLines names the highlighted session and says whether the push
// carries it. It is the whole of the pane's own chrome: what follows is the
// transcript.
func sessionHeaderLines(s PushWizardSession) []string {
	return []string{
		"session: " + s.Row.SessionID,
		"project: " + projectLabelOf(s),
		"harness: " + s.Row.ModelHarness,
		"started: " + sessionStartText(s.Row),
		"",
		sessionStateNote(s),
	}
}

// sessionStateNote is the one sentence saying what the push does with this
// session.
func sessionStateNote(s PushWizardSession) string {
	switch {
	case s.NeedsIngest:
		return previewUnselectedNote
	case s.Locked:
		return previewWithheldNote
	case s.Action == PushWithRedaction:
		return previewSelectedNote
	default:
		return previewUnselectedNote
	}
}

// previewBody is one loaded preview: the pane's own header lines, and then
// EITHER the published transcript or a plain note saying why there is none.
type previewBody struct {
	th         theme.Theme
	header     []string
	transcript transcriptview.Document
	note       string
}

var _ kit.PreviewBody = previewBody{}

// previewSeparator ends the pane's own chrome: one blank line between what the
// pane wrote about the session and the transcript the push sends.
const previewSeparator = "\n\n"

// Render implements kit.PreviewBody, laying the preview out at the pane's
// CURRENT width.
func (b previewBody) Render(width int) string {
	if width <= 0 {
		return b.plain()
	}
	styles := b.th.Styles()
	var parts []string
	if len(b.header) > 0 {
		head := make([]string, 0, len(b.header))
		for _, line := range b.header {
			head = append(head, styles.Muted.Render(ansi.Wrap(line, width, "")))
		}
		parts = append(parts, strings.Join(head, "\n"))
	}
	if b.note != "" {
		// The pane hands its body lines to the viewport UNSTYLED, so a body that
		// does not color itself is drawn in whatever the terminal's default ink
		// happens to be rather than the theme's.
		parts = append(parts, styles.Base.Render(ansi.Wrap(b.note, width, "")))
	}
	if body := b.transcript.Render(width); body != "" {
		parts = append(parts, body)
	}
	return strings.Join(parts, previewSeparator)
}

// plain returns the body's raw, unstyled text for the width<=0 case, where no
// layout is possible.
func (b previewBody) plain() string {
	var parts []string
	if len(b.header) > 0 {
		parts = append(parts, strings.Join(b.header, "\n"))
	}
	if b.note != "" {
		parts = append(parts, b.note)
	}
	if body := b.transcript.Render(0); body != "" {
		parts = append(parts, body)
	}
	return strings.Join(parts, previewSeparator)
}
