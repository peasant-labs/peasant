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
// It returns no entries (not an error) for a session the store holds no
// transcript for, and an error only when the read itself failed.
type StoredEntriesFunc func(sessionID string) ([]schema.SessionEntry, error)

// PublishedTurnsFunc returns one session's turns AS THEY WILL BE PUBLISHED:
// read from the local store and redacted by the same redactor, over the same
// entries, that the push applies on the way out.
//
// It is the read seam the selection preview binds to. The mounted command fills
// it from the store and the push redactor; a test fills it with recorded turns
// directly.
type PublishedTurnsFunc func(sessionID string) ([]ingest.Turn, error)

// NewPublishedTurns builds the preview read over a stored-entry reader and the
// redactor the push runs with.
//
// The redactor is required. RedactEntries leaves the entries as recorded when
// it is handed nil, so a nil redactor here would draw the recorded text on the
// screen that promises the published text. This fails closed instead: the pane
// reports that it cannot show the published transcript, and shows nothing.
func NewPublishedTurns(entries StoredEntriesFunc, redactor redact.JSONRedactor) PublishedTurnsFunc {
	return func(sessionID string) ([]ingest.Turn, error) {
		if entries == nil || redactor == nil {
			return nil, fmt.Errorf(
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
			return nil, err
		}
		if len(stored) == 0 {
			return nil, nil
		}
		redacted, err := RedactEntries(redactor, stored)
		if err != nil {
			return nil, err
		}
		return transcript.EntriesToTurns(redacted), nil
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
		if p.turns == nil {
			body.note = previewNoTranscript
			return body, nil
		}
		recorded, err := p.turns(id)
		if err != nil {
			return nil, err
		}
		if len(recorded) == 0 {
			body.note = previewNoTranscript
			return body, nil
		}
		body.transcript = p.renderer.Document(recorded)
		return body, nil
	}
	return previewBody{th: p.th, note: previewNoSessionNote}, nil
}

// projectLines describes one project group.
func (p wizardPreview) projectLines(project string) []string {
	total, selected := 0, 0
	for _, s := range p.sessions {
		if projectLabelOf(s) != project {
			continue
		}
		total++
		if !s.Locked && s.Action == PushWithRedaction {
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
	if body := b.transcript.Render(width); body != "" {
		parts = append(parts, body)
	} else if b.note != "" {
		// The pane hands its body lines to the viewport UNSTYLED, so a body that
		// does not color itself is drawn in whatever the terminal's default ink
		// happens to be rather than the theme's.
		parts = append(parts, styles.Base.Render(ansi.Wrap(b.note, width, "")))
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
	if body := b.transcript.Render(0); body != "" {
		parts = append(parts, body)
	} else if b.note != "" {
		parts = append(parts, b.note)
	}
	return strings.Join(parts, previewSeparator)
}
