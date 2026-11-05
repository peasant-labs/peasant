package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/sessionreview"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// Reasons a session has status error. They are fixed words: a store error can
// quote what it failed on, and no output of this command may carry content.
const (
	reviewReasonNotInStore  = "the local store does not hold this session; check the id with `peasant sessions list --json`"
	reviewReasonInvalidID   = "not a session id"
	reviewReasonPreviewOnly = "only a bounded preview of this session is stored, so it cannot be reviewed in full; run `peasant harvest index --force` with stored artifacts to rebuild it"
	reviewReasonUnreadable  = "the stored entries failed validation; run `peasant harvest index --force` with stored artifacts to repair them"
	reviewReasonSubagents   = "its subagent sessions could not be read, and they travel with it when it is published"
)

// sessionsReviewFindingsError is a review that ran to the end and found
// residue. main maps it to defaults.ExitFindings.
type sessionsReviewFindingsError struct{ sessions int }

func (e *sessionsReviewFindingsError) Error() string {
	return fmt.Sprintf("sessions review: %d session(s) have findings", e.sessions)
}

// sessionReviewFinding is one finding in the document. SessionID names the
// session it is in, which is a subagent session of the row's session when it
// differs from the row's.
type sessionReviewFinding struct {
	Kind       sessionreview.FindingKind `json:"kind"`
	SessionID  string                    `json:"sessionId,omitempty"`
	EntryIndex int                       `json:"entryIndex"`
	Role       schema.Role               `json:"role"`
	Rule       string                    `json:"rule"`
	Preview    string                    `json:"preview"`
}

// sessionReviewRow is one requested session. Its status covers its whole
// subagent tree, because `village push` sends that tree with it.
type sessionReviewRow struct {
	SessionID string                   `json:"sessionId"`
	Status    clicontract.ReviewStatus `json:"status"`
	Reason    string                   `json:"reason"`
	Subagents int                      `json:"subagents"`
	Findings  []sessionReviewFinding   `json:"findings"`
}

type sessionsReviewSummary struct {
	Sessions     int `json:"sessions"`
	Clean        int `json:"clean"`
	WithFindings int `json:"withFindings"`
	Errors       int `json:"errors"`
	Findings     int `json:"findings"`
}

// sessionsReviewDocument is the sessions review --json document.
type sessionsReviewDocument struct {
	Level          string                `json:"level"`
	RuleSetVersion string                `json:"ruleSetVersion"`
	Summary        sessionsReviewSummary `json:"summary"`
	Sessions       []sessionReviewRow    `json:"sessions"`
	clicontract.Meta
}

// sessionsReviewDeps is what the command reads from outside the store.
type sessionsReviewDeps struct {
	// gitEmail returns the git identity, or "" when there is none.
	gitEmail func(context.Context) string
}

func buildSessionsReviewCommand() *cobra.Command {
	return buildSessionsReviewCommandWith(sessionsReviewDeps{
		gitEmail: gitops.NewExecGitRepository(".").UserEmail,
	})
}

func buildSessionsReviewCommandWith(deps sessionsReviewDeps) *cobra.Command {
	var (
		sessions    []string
		ownerEmails []string
		level       string
		jsonOutput  bool
	)
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Check sessions for what redaction leaves behind before you publish them",
		Long: `Read each named session, run the configured redactor on it in memory, and
report what is left: credentials in formats no rule knows, tool results that
printed a credential file, other people's email addresses, links that anyone
holding them can open, and the redaction module's own advisory hits.

Nothing is written. Findings hold a masked preview, never the matched text. A
session's subagent sessions are reviewed with it, because they are published
with it, and its status covers them.

Exit status: 0 every session is clean, 4 at least one has findings and none
failed, 1 at least one could not be reviewed.`,
		Example: `  peasant sessions review --session <id>
  peasant sessions list --json | peasant sessions review --sessions-from - --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			requested, warnings, err := resolveReviewSelection(cmd, sessions, jsonOutput)
			if err != nil {
				return err
			}
			built, err := buildRedactor(cmd, level, sessionsReviewRefusal, func(path string) (*config.Config, error) {
				return loadRunConfig(cmd, path, true)
			})
			if err != nil {
				return err
			}
			db, err := openReviewStore(cmd, built.Config)
			if err != nil {
				return err
			}
			defer db.Close()

			ctx := cmd.Context()
			opts := sessionreview.Options{OwnerEmails: ownerEmails}
			if email := deps.gitEmail(ctx); email != "" {
				opts.OwnerEmails = append(slices.Clone(ownerEmails), email)
			}
			run, err := newSessionsReview(ctx, db, built.Redactor, opts)
			if err != nil {
				return err
			}
			rows := make([]sessionReviewRow, 0, len(requested))
			for _, id := range requested {
				rows = append(rows, run.reviewTree(ctx, id))
			}
			document := sessionsReviewDocument{
				Level:          built.Level.String(),
				RuleSetVersion: redact.Version(),
				Summary:        summarizeReview(rows),
				Sessions:       rows,
				Meta:           warnings.Meta("", ""),
			}
			if jsonOutput {
				if err := clicontract.WriteJSON(cmd.OutOrStdout(), document); err != nil {
					return err
				}
			} else {
				printSessionsReview(cmd, document)
			}
			return reviewOutcome(cmd, document.Summary)
		},
	}
	cmd.Flags().StringArrayVar(&sessions, "session", nil, "Session to review (repeatable)")
	clicontract.RegisterSessionsFrom(cmd)
	cmd.Flags().StringVar(&level, "level", "", fmt.Sprintf("Override redaction level (%s)", config.RedactionLevelMenu()))
	cmd.Flags().StringArrayVar(&ownerEmails, "owner-email", nil, "An address that counts as yours, beside your git identity (repeatable)")
	cmd.Flags().BoolVar(&jsonOutput, defaults.JSONFlagName, false, "Output one JSON document")
	return cmd
}

// sessionsReviewRefusal is the level refusal text of this command.
var sessionsReviewRefusal = redactionRefusalText{
	Operation: "peasant sessions review",
	Step:      "before any stored transcript was read",
	Impact:    "Nothing was reviewed.",
}

// resolveReviewSelection returns the requested sessions, once each, and the
// collector holding the warnings of the input list. Selecting nothing is a
// usage error: a review never widens to every session.
func resolveReviewSelection(cmd *cobra.Command, sessions []string, jsonOutput bool) ([]string, *clicontract.Collector, error) {
	list, set, err := clicontract.ResolveSessionsFrom(cmd)
	if err != nil {
		return nil, nil, err
	}
	warnings := clicontract.NewCollector(cmd.ErrOrStderr(), jsonOutput)
	for _, warning := range list.Warnings() {
		warnings.Warn(warning)
	}
	if set {
		return list.IDs, warnings, nil
	}
	var requested []string
	for _, raw := range sessions {
		if id := strings.TrimSpace(raw); id != "" && !slices.Contains(requested, id) {
			requested = append(requested, id)
		}
	}
	if len(requested) == 0 {
		return nil, nil, clicontract.NewUsageError(fmt.Sprintf(
			"%s: no session selected\n"+
				"What went wrong: neither --session nor --%s named a session.\n"+
				"Why: a review reads whole transcripts, and it never widens to every session.\n"+
				"Fix: pass --session <id>, or --%s <file|-> with a list, for example from `peasant sessions list --json`.",
			cmd.CommandPath(), clicontract.SessionsFromFlag, clicontract.SessionsFromFlag))
	}
	return requested, warnings, nil
}

// openReviewStore opens the store read-only. The configuration comes from
// buildRedactor; its output path is the managed-generation root.
func openReviewStore(cmd *cobra.Command, cfg *config.Config) (*store.Store, error) {
	ownedRoot := ""
	if cfg.Output.BasePath != "" {
		resolved, err := ingest.NewResolvedPath(cfg.Output.BasePath)
		if err != nil {
			return nil, fmt.Errorf("resolve output path: %w", err)
		}
		ownedRoot = string(resolved)
	}
	db, err := openRunStore(cmd, true, ownedRoot, ingest.WriteConfig{})
	if err != nil {
		return nil, fmt.Errorf("open analytics store: %w", err)
	}
	return db, nil
}

// reviewStore is the store reads a review makes.
type reviewStore interface {
	childSessionReader
	ingest.FullSessionEntryReader
	AllSessionIDs(context.Context) ([]string, error)
}

var _ reviewStore = (*store.Store)(nil)

// sessionsReview holds one run, so a session in two requested trees is read
// once.
type sessionsReview struct {
	db       reviewStore
	redactor redact.Redactor
	opts     sessionreview.Options
	known    map[string]bool
	reviewed map[string]sessionReviewRow
}

func newSessionsReview(ctx context.Context, db reviewStore, r redact.Redactor, opts sessionreview.Options) (*sessionsReview, error) {
	ids, err := db.AllSessionIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the stored sessions: %w", err)
	}
	known := make(map[string]bool, len(ids))
	for _, id := range ids {
		known[id] = true
	}
	return &sessionsReview{db: db, redactor: r, opts: opts, known: known, reviewed: map[string]sessionReviewRow{}}, nil
}

// reviewTree reviews a session and every subagent session that travels with
// it. The tree comes from the expansion `village push` applies, so a clean
// session never carries an unreviewed one into a publish. An error anywhere in
// the tree wins over findings, as it does in the exit status.
func (r *sessionsReview) reviewTree(ctx context.Context, id string) sessionReviewRow {
	row := sessionReviewRow{SessionID: id, Findings: []sessionReviewFinding{}}
	tree, err := expandSubagents(ctx, r.db, []string{id}, false)
	if err != nil {
		row.Status, row.Reason = clicontract.ReviewError, reviewReasonSubagents
		return row
	}
	row.Subagents = len(tree.sessions) - 1
	var failures []string
	for _, member := range tree.sessions {
		reviewed := r.reviewSession(ctx, member.ID)
		row.Findings = append(row.Findings, reviewed.Findings...)
		switch {
		case reviewed.Status != clicontract.ReviewError:
		case member.ID == id:
			failures = append(failures, reviewed.Reason)
		default:
			failures = append(failures, "subagent session "+member.ID+": "+reviewed.Reason)
		}
	}
	switch {
	case len(failures) > 0:
		row.Status, row.Reason = clicontract.ReviewError, strings.Join(failures, "; ")
	case len(row.Findings) > 0:
		row.Status = clicontract.ReviewFindings
	default:
		row.Status = clicontract.ReviewClean
	}
	return row
}

func (r *sessionsReview) reviewSession(ctx context.Context, id string) sessionReviewRow {
	if row, ok := r.reviewed[id]; ok {
		return row
	}
	row := r.readAndScan(ctx, id)
	r.reviewed[id] = row
	return row
}

func (r *sessionsReview) readAndScan(ctx context.Context, id string) sessionReviewRow {
	failed := func(reason string) sessionReviewRow {
		return sessionReviewRow{SessionID: id, Status: clicontract.ReviewError, Reason: reason}
	}
	sid, err := ingest.NewSessionID(id)
	if err != nil {
		return failed(reviewReasonInvalidID)
	}
	if !r.known[id] {
		return failed(reviewReasonNotInStore)
	}
	// The read export and publication use: it validates the stored format,
	// serves native and mirrored sessions alike, hydrates full tool output, and
	// refuses a preview-only capture instead of serving bounded text.
	entries, _, err := transcript.LoadEntriesForDetail(ctx, r.db, sid, transcript.DetailLoadOptions{})
	if errors.Is(err, store.ErrContentCaptureIncomplete) {
		return failed(reviewReasonPreviewOnly)
	}
	if err != nil {
		return failed(reviewReasonUnreadable)
	}
	row := sessionReviewRow{SessionID: id, Status: clicontract.ReviewClean}
	for _, finding := range sessionreview.Review(entries, r.redactor, r.opts) {
		row.Findings = append(row.Findings, sessionReviewFinding{
			Kind: finding.Kind, SessionID: id, EntryIndex: finding.EntryIndex,
			Role: finding.Role, Rule: finding.Rule, Preview: finding.Preview,
		})
		row.Status = clicontract.ReviewFindings
	}
	return row
}

func summarizeReview(rows []sessionReviewRow) sessionsReviewSummary {
	summary := sessionsReviewSummary{Sessions: len(rows)}
	for _, row := range rows {
		summary.Findings += len(row.Findings)
		switch row.Status {
		case clicontract.ReviewClean:
			summary.Clean++
		case clicontract.ReviewFindings:
			summary.WithFindings++
		case clicontract.ReviewError:
			summary.Errors++
		}
	}
	return summary
}

func printSessionsReview(cmd *cobra.Command, document sessionsReviewDocument) {
	out := cmd.OutOrStdout()
	for _, row := range document.Sessions {
		if row.Status == clicontract.ReviewError {
			fmt.Fprintf(out, "%s error %s\n", row.SessionID, row.Reason)
		}
		for _, finding := range row.Findings {
			fmt.Fprintf(out, "%s entry %d %s %s %s\n", finding.SessionID, finding.EntryIndex, finding.Role, finding.Kind, finding.Preview)
		}
	}
	summary := document.Summary
	fmt.Fprintf(out, "%d session(s) reviewed: %d clean, %d with findings, %d errors, %d findings\n",
		summary.Sessions, summary.Clean, summary.WithFindings, summary.Errors, summary.Findings)
}

// reviewOutcome is the exit status: an error wins over findings.
func reviewOutcome(cmd *cobra.Command, summary sessionsReviewSummary) error {
	if summary.Errors > 0 {
		return fmt.Errorf("sessions review: %d session(s) could not be reviewed", summary.Errors)
	}
	if summary.WithFindings > 0 {
		// Findings are the result, already printed. The status carries them.
		cmd.SilenceErrors = true
		return &sessionsReviewFindingsError{sessions: summary.WithFindings}
	}
	return nil
}
