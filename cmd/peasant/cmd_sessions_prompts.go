package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"github.com/spf13/cobra"
)

// sessionPromptCommand is the command a prompt invoked.
type sessionPromptCommand struct {
	Name string `json:"name"`
	Args string `json:"args"`
}

// sessionPrompt is one prompt in the JSON document.
type sessionPrompt struct {
	EntryIndex int                   `json:"entryIndex"`
	Timestamp  *string               `json:"timestamp"`
	Text       string                `json:"text"`
	Truncated  bool                  `json:"truncated"`
	Command    *sessionPromptCommand `json:"command"`

	// stored is the stored text the human view prints with --include-commands.
	stored string
}

// sessionPromptsOutput is the JSON document of the prompts command.
type sessionPromptsOutput struct {
	clicontract.Meta
	SessionID       string          `json:"sessionId"`
	ParentSessionID *string         `json:"parentSessionId"`
	PromptCount     int             `json:"promptCount"`
	Prompts         []sessionPrompt `json:"prompts"`
	Note            string          `json:"note"`
}

// buildSessionsPromptsCommand constructs `peasant sessions prompts`.
func buildSessionsPromptsCommand() *cobra.Command {
	var (
		maxChars        int
		includeCommands bool
		asJSON          bool
	)

	cmd := &cobra.Command{
		Use:   "prompts <session-id>",
		Short: "Print only what the person typed in a session",
		Long: `Print the entries of one session that a person typed, in entry order.

The selection is the stored fields role = user, depth = 0 and entry_type = text.
This command adds no classification of its own: an entry a harness injected is
left out only when ingest stored it as system content.

A slash-command or skill invocation stays in the list with its command set. The
text view shows it as "/name args" on one line; --include-commands prints its
stored text instead. The JSON document always carries the command field.

A subagent session has no human prompts: its first user entry is the parent
agent's instruction. The command prints what it finds and says so in the note.

If a session's capture is a bounded preview, its text may be cut. The command
warns, on stderr and in the JSON warnings, with the code capture_bounded.

Output is local and is not redacted, as with sessions context.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				return nil
			}
			return clicontract.NewUsageError(fmt.Sprintf(
				"%s takes one session id, got %d\n"+
					"Fix: use `peasant sessions list` to find a session id.",
				cmd.CommandPath(), len(args)))
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			sid, err := schema.NewSessionID(args[0])
			if err != nil {
				return clicontract.NewUsageError(fmt.Sprintf(
					"invalid session id %q: %v\n"+
						"Fix: use `peasant sessions list` to find a valid session id.",
					args[0], err))
			}
			if maxChars < 0 {
				return clicontract.NewUsageError(fmt.Sprintf(
					"invalid --max-chars value %d: it must be 0 (no cut) or more", maxChars))
			}
			return runSessionsPrompts(cmd, sid, maxChars, includeCommands, asJSON)
		},
	}

	cmd.Flags().IntVar(&maxChars, "max-chars", 0, "Cut each prompt to this many characters (0 = no cut)")
	cmd.Flags().BoolVar(&includeCommands, "include-commands", false,
		`Print the stored text of command entries in the text view, not "/name args" (the JSON command field is always present)`)
	cmd.Flags().BoolVar(&asJSON, defaults.JSONFlagName, false, "Output as one JSON document")
	return cmd
}

// runSessionsPrompts reads the prompts and renders them. It writes nothing to
// the store.
func runSessionsPrompts(cmd *cobra.Command, sid schema.SessionID, maxChars int, includeCommands, asJSON bool) error {
	ctx := cmd.Context()
	db, cleanup, err := openDB(cmd)
	if err != nil {
		return err
	}
	defer cleanup()

	maxIdx, err := db.MaxEntryIndex(ctx, sid)
	if err != nil {
		return fmt.Errorf("sessions prompts: %w", err)
	}
	if maxIdx < 0 {
		return errSessionNotIndexed(sid)
	}
	prompts, err := db.ListUserPrompts(ctx, sid)
	if err != nil {
		return fmt.Errorf("sessions prompts: %w", err)
	}
	capture, found, err := db.GetSessionContentCapture(ctx, sid)
	if err != nil {
		return fmt.Errorf("sessions prompts: %w", err)
	}
	lineages, err := db.SessionLineages(ctx, []string{string(sid)})
	if err != nil {
		return fmt.Errorf("sessions prompts: %w", err)
	}

	warnings := clicontract.NewCollector(cmd.ErrOrStderr(), asJSON)
	if store.CaptureTextBounded(capture, found) {
		warnings.Warn(clicontract.Warning{
			Code:      clicontract.WarningCaptureBounded,
			Message:   fmt.Sprintf("this capture stores bounded previews, so a long prompt may be cut; run `peasant harvest index --session %s` to repair it", sid),
			SessionID: string(sid),
		})
	}

	lineage := lineages[string(sid)]
	note := ""
	switch {
	case lineage.ParentSessionID != nil:
		note = fmt.Sprintf("this is a subagent session, a child of %s: its first user entry is the parent agent's instruction", *lineage.ParentSessionID)
	case lineage.ParentUnresolved:
		note = "this is a subagent session whose parent could not be resolved: its first user entry is the parent agent's instruction"
	}

	out := sessionPromptsOutput{
		SessionID:       string(sid),
		ParentSessionID: lineage.ParentSessionID,
		PromptCount:     len(prompts),
		Prompts:         make([]sessionPrompt, 0, len(prompts)),
	}
	for _, prompt := range prompts {
		out.Prompts = append(out.Prompts, promptDocument(prompt, maxChars))
	}
	var emptyCode clicontract.EmptyCode
	emptyReason := ""
	if len(prompts) == 0 {
		emptyCode = clicontract.EmptyNoMatchingEntries
		emptyReason = "no entry in this session is a typed prompt: ingest stored every user entry as harness-injected, or the session has none"
		note = strings.TrimSpace(emptyReason + ". " + note)
	}
	out.Note = note

	if asJSON {
		out.Meta = warnings.Meta(emptyCode, emptyReason)
		return clicontract.WriteJSON(cmd.OutOrStdout(), out)
	}
	if note != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "note: %s\n", note)
	}
	renderPromptsText(cmd, out.Prompts, maxChars, includeCommands)
	return nil
}

// promptDocument shapes one prompt. A command entry's text is what the person
// typed, "/name args", because its stored text is the harness's markup.
func promptDocument(prompt store.UserPrompt, maxChars int) sessionPrompt {
	doc := sessionPrompt{EntryIndex: prompt.EntryIndex, Text: prompt.Text, stored: prompt.Text}
	if prompt.TimestampMS != nil {
		stamp := time.UnixMilli(*prompt.TimestampMS).UTC().Format(time.RFC3339)
		doc.Timestamp = &stamp
	}
	if prompt.Command != nil {
		doc.Command = &sessionPromptCommand{Name: prompt.Command.Name, Args: prompt.Command.Args}
		doc.Text = strings.TrimSpace("/" + prompt.Command.Name + " " + prompt.Command.Args)
	}
	doc.Text, doc.Truncated = cutPrompt(doc.Text, maxChars)
	return doc
}

// cutPrompt cuts text to maxChars runes; 0 leaves it whole.
func cutPrompt(text string, maxChars int) (string, bool) {
	if maxChars == 0 || utf8.RuneCountInString(text) <= maxChars {
		return text, false
	}
	return store.TruncateToRunes(text, maxChars), true
}

// renderPromptsText writes each prompt under a header line.
func renderPromptsText(cmd *cobra.Command, prompts []sessionPrompt, maxChars int, includeCommands bool) {
	w := cmd.OutOrStdout()
	for _, prompt := range prompts {
		text, truncated := prompt.Text, prompt.Truncated
		if prompt.Command != nil && includeCommands {
			text, truncated = cutPrompt(prompt.stored, maxChars)
		}
		stamp := "unknown time"
		if prompt.Timestamp != nil {
			stamp = *prompt.Timestamp
		}
		if truncated {
			text += "..."
		}
		fmt.Fprintf(w, "[%d] %s\n%s\n\n", prompt.EntryIndex, stamp, escapePromptControls(text))
	}
}

// escapePromptControls shows control characters other than newline and tab as
// escapes, so stored text cannot drive the terminal.
func escapePromptControls(text string) string {
	var b strings.Builder
	for _, r := range text {
		if !unicode.IsControl(r) || r == '\n' || r == '\t' {
			b.WriteRune(r)
			continue
		}
		quoted := strconv.QuoteRune(r)
		b.WriteString(quoted[1 : len(quoted)-1])
	}
	return b.String()
}
