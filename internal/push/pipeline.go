// Package push implements the peasant push pipeline: reading sessions from the local
// store, mapping metadata to the village schema, and uploading via concurrent
// multipart form requests.
package push

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/peasant-labs/peasant/internal/auth"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/title"
	"github.com/peasant-labs/schema"
)

// DefaultConcurrency is the default maximum number of parallel uploads.
const DefaultConcurrency = 5

// connectionAbortThreshold is the number of consecutive connection errors that
// trigger a pipeline abort.
const connectionAbortThreshold = 3

// localPersistenceBudget bounds the local SQLite writes that record work the
// village has already accepted. It is generous compared with a row update and
// exists only so a database another process is holding cannot stall git.
const localPersistenceBudget = 10 * time.Second

// persistenceContext returns the context for local bookkeeping about work that
// already happened.
//
// It is deliberately NOT derived from the upload budget. --timeout bounds how
// long peasant waits on the NETWORK; a transcript the village has already
// accepted is on the village whether or not the budget survived the response,
// and the row that records it has to be written regardless. Sharing the budget
// made the ordinary "headers arrive, body stalls" failure — a proxy, a load
// balancer, a VPN — upload the transcript, lose the terminal receipt write to
// the same deadline, and then re-send the identical full transcript on every
// subsequent commit, forever.
func persistenceContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), localPersistenceBudget)
}

// Pipeline orchestrates pushing local sessions to the Peasant village.
//
// It reads sessions from a PipelineStore, maps their metadata, reads transcript
// files via the injected FileSystem, and uploads via the injected Publisher.
// The Pipeline has no os import — all filesystem access is through p.fs.
type Pipeline struct {
	store     PipelineStore
	transport Transport
	creds     *auth.Credentials
	cfg       *config.Config
	fs        ingest.FileSystem
	runCfg    PipelineConfig
	redactor  ingest.TextRedactor // safety-net redaction applied before upload
	stderr    io.Writer           // destination for non-fatal warnings and notices
	titles    title.Pipeline
}

// Publisher is the authoritative Village publication surface required by the
// push pipeline. Legacy multipart publishing is intentionally not part of this
// contract, so an unusable fallback cannot be wired into NewPipeline.
type Publisher interface {
	PublishAuthoritative(context.Context, schema.AuthoritativePublishRequest, io.Reader, string) (schema.AuthoritativePublishResponse, int, error)
	UpdateOwner(context.Context, schema.TranscriptID, schema.OwnerTranscriptUpdateRequest) (schema.OwnerTranscriptUpdateResponse, int, error)
}

// PipelineStore is the complete local persistence surface required by the push
// pipeline. Receipt reads remain available on store.Store but are not needed to
// publish or persist authoritative results.
type PipelineStore interface {
	CandidateStore
	InsertPushLog(context.Context, ingest.PushLogEntry) error
	SessionsWithoutMetrics(context.Context) ([]ingest.HeldSession, error)
	GetQualityMetrics(context.Context, ingest.SessionID) (*schema.QualityMetrics, error)
	ListEntries(context.Context, ingest.SessionID) ([]schema.SessionEntry, error)
	ListCurrentSessionCommitAssociations(context.Context, ingest.SessionID) ([]ingest.CurrentCommitAssociation, error)
	SavePublication(context.Context, store.PublicationRecord) error
	RecordPublicationAttempt(context.Context, store.PublicationAttemptDiagnostic) error
}

// NewPipeline creates a push Pipeline with the given dependencies.
//
//   - store: candidate queries, publication receipt persistence, and audit writes
//   - transport: authoritative publication, owner update, and contract negotiation
//   - creds: authenticated credentials (API key, village URL)
//   - cfg: loaded application config (push method, visibility, output base path)
//   - fs: file system for reading metadata and transcript files
//   - runCfg: runtime flags from the CLI (dry-run, force, provider filter, etc.)
//   - redactor: safety-net redactor applied before upload (nil disables re-redaction)
//   - stderr: writer for non-fatal warnings (pass os.Stderr in production)
func NewPipeline(
	store PipelineStore,
	transport Transport,
	creds *auth.Credentials,
	cfg *config.Config,
	fs ingest.FileSystem,
	runCfg PipelineConfig,
	redactor ingest.TextRedactor,
	stderr io.Writer,
) *Pipeline {
	titles, err := title.Default()
	if err != nil {
		slog.Error("initialize canonical title pipeline for publication", "error", err)
	}
	return &Pipeline{
		store:     store,
		transport: transport,
		creds:     creds,
		cfg:       cfg,
		fs:        fs,
		runCfg:    runCfg,
		redactor:  redactor,
		stderr:    stderr,
		titles:    titles,
	}
}

// Run executes the push pipeline and returns the aggregate result.
//
// Behavior:
//   - "individual" push method without --source-provider returns an error.
//   - Sessions without metrics are held back; a notice is printed to stderr.
//   - With --dry-run: store queries run but no HTTP calls, publication receipts,
//     push_log writes, or local publication-cursor updates occur.
//   - With --force: AllPushableSessions is called (all sessions, regardless of pushed_at).
//   - Uploads are concurrent (bounded by PipelineConfig.Concurrency or DefaultConcurrency).
//   - 3 consecutive connection errors abort the pipeline.
//   - Receipt persistence failures become per-session errors; InsertPushLog
//     failures are logged to stderr rather than returned.
func (p *Pipeline) Run(ctx context.Context) (*PushResult, error) {
	startedAt := time.Now().UnixMilli()

	// 1. Resolve effective visibility + content license (both uniform for the run).
	visibility := p.resolveVisibility()
	license := p.resolveLicense()

	// 2. Guard: individual mode is not yet implemented.
	if p.cfg.Push.Method == config.PushMethodIndividual && p.runCfg.SourceProvider == "" {
		return nil, fmt.Errorf(
			"push.method is set to %q in your config, which requires an interactive session "+
				"picker (not yet implemented). To push now, either run 'peasant kickstart' to "+
				"change your push method, or use --source-provider to filter by provider",
			config.PushMethodIndividual)
	}

	// 3. Query sessions from store.
	sessions, baseCount, err := p.getTargetSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("get target sessions: %w", err)
	}

	// 4. Warn about held-back sessions (missing metrics), for this push's scope
	// only. A hook fires on every commit in ONE repository; listing another
	// repository's session identifiers there is both noise and disclosure.
	p.noticeHeld(ctx)

	// 5. Handle empty-result states.
	if len(sessions) == 0 {
		return &PushResult{BaseCandidateCount: baseCount, EmptyReason: p.emptyReason(ctx, baseCount)}, nil
	}

	result := &PushResult{BaseCandidateCount: baseCount}

	// 6. Dry-run: sequential scan exercising the SAME pushSession pre-flight as the
	// real path (read → guards → map → client-side validate), diverging only at the
	// side-effecting upload + persistence, which pushSession skips when DryRun is
	// set. No village negotiation (no HTTP), so the forecast emits the CLI's own
	// contract version; no audit log.
	if stopBeforeRemoteNegotiation(p.runCfg.DryRun) {
		for _, sess := range sessions {
			sr := p.pushSession(ctx, sess, visibility, license, defaults.PublishSchemaVersion, nil)
			result.Sessions = append(result.Sessions, sr)
			result.countStatus(sr.Status)
		}
		return result, nil // no audit log for dry-run
	}
	// Fail local transcript reads before the first remote negotiation. pushSession
	// reads again under its per-session operation so concurrent store changes also
	// fail closed rather than publishing stale preflight bytes.
	for _, sess := range sessions {
		sessionID, _ := ingest.NewSessionID(sess.SessionID)
		if _, readErr := p.store.ListEntries(ctx, sessionID); readErr != nil {
			sr := entryReadFailure(sess, readErr, entryReadPreflight)
			result.Sessions = append(result.Sessions, sr)
			result.countStatus(sr.Status)
			return result, nil
		}
	}

	// 6b. Version-negotiation preflight: query the village's accepted
	// contract window and decide the emit version. Aborts the whole push on an
	// upgrade-CLI or non-downgradable mismatch; downgrade-emits (with a one-line
	// warning) when the CLI is ahead. Skipped above for dry-run (no HTTP).
	emit, capabilities, err := p.negotiate(ctx)
	if err != nil {
		return result, err
	}

	// 7. Concurrent uploads via errgroup, smallest first when a budget applies.
	sessions = orderForBudget(ctx, sessions)
	concurrency := p.runCfg.Concurrency
	if concurrency <= 0 {
		concurrency = DefaultConcurrency
	}

	var (
		mu         sync.Mutex
		connErrors int   // rolling count of consecutive connection errors
		abortErr   error // non-nil when pipeline should abort further goroutines
	)

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)

	for _, sess := range sessions {
		sess := sess // capture for goroutine
		g.Go(func() error {
			// Check if pipeline has already been aborted.
			mu.Lock()
			if abortErr != nil {
				mu.Unlock()
				return nil // skip quietly; aborting
			}
			mu.Unlock()

			sr := p.pushSession(gctx, sess, visibility, license, emit, capabilities)

			mu.Lock()
			defer mu.Unlock()
			result.Sessions = append(result.Sessions, sr)
			result.countStatus(sr.Status)

			if sr.Status == PushStatusError && isConnectionError(sr.Error) {
				connErrors++
				if connErrors >= connectionAbortThreshold {
					abortErr = fmt.Errorf(
						"aborting: %d connection failures to %s — check your network or village URL",
						connErrors, p.creds.VillageURL)
				}
			} else if sr.Status != PushStatusError {
				// Reset rolling count on any successful (or non-error) result.
				connErrors = 0
			}

			return nil // errors are collected in result.Sessions, not returned via errgroup
		})
	}
	_ = g.Wait()

	if abortErr != nil {
		return result, abortErr
	}

	// 8. Write audit log entry. Like receipt persistence this records work that
	// already happened, so it runs on its own context rather than the upload budget.
	auditCtx, cancelAudit := persistenceContext(ctx)
	defer cancelAudit()
	finishedAt := time.Now().UnixMilli()
	if err := p.store.InsertPushLog(auditCtx, ingest.PushLogEntry{
		StartedAt:       startedAt,
		FinishedAt:      &finishedAt,
		VillageURL:      p.creds.VillageURL,
		SessionsPushed:  result.New,
		SessionsUpdated: result.Updated,
		SessionsSkipped: result.Skipped,
		SessionsFailed:  result.Errors,
		UserID:          p.creds.UserID,
		Username:        p.creds.Username,
	}); err != nil {
		// The write no longer inherits the upload budget, so a failure here is a
		// real one rather than the SQLite "interrupted" a cancelled run used to
		// produce, and is reported as itself.
		fmt.Fprintf(p.stderr, "warning: audit log: %v\n", err)
	}

	return result, nil
}

// resolveVisibility returns the visibility this run will actually publish at.
//
// It defers to the shared resolver rather than repeating the precedence, so the
// value the pipeline acts on can never differ from the value the command
// disclosed to the user. That matters even while the publish contract carries no
// visibility at all: the moment it does, this is the value that would go on the
// wire, and it has to be the one that was announced.
func (p *Pipeline) resolveVisibility() schema.Visibility {
	return config.EffectiveVisibility(p.runCfg.Visibility, p.cfg).Effective
}

// resolveLicense returns the effective content license for this run.
// CLI flag (runCfg.License) takes precedence over the config default (chosen at
// kickstart). Unlike visibility there is NO forced fallback: an unset license
// returns "" so MapMetadata omits the field and the village stores NULL — peasant
// never imposes a license the contributor did not choose.
func (p *Pipeline) resolveLicense() schema.License {
	if p.runCfg.License != "" {
		return p.runCfg.License
	}
	return p.cfg.Push.License
}

// getTargetSessions determines which sessions to push based on flags and config.
//
// It delegates the base candidate query to the shared QueryPushCandidates helper
// (the SAME helper the interactive wizard uses), then applies the wizard
// whitelist and the branch-aware selection filter. cfg.Push.Sources is threaded
// into the query so the by-source method behaves identically on both paths.
// The returned baseCount is the number of candidates from QueryPushCandidates
// BEFORE any selection filtering — surfaced so callers can tell "selection
// excluded everything" from "nothing to push" without a second query.
func (p *Pipeline) getTargetSessions(ctx context.Context) (kept []ingest.PushSessionRow, baseCount int, err error) {
	base, err := QueryPushCandidates(ctx, p.store, PushCandidateQuery{
		Force:          p.runCfg.Force,
		SourceProvider: p.runCfg.SourceProvider,
		Method:         p.cfg.Push.Method,
		Sources:        p.cfg.Push.Sources,
	})
	if err != nil {
		return nil, 0, err
	}
	baseCount = len(base)

	base = p.filterByWizardSelection(base)
	kept, withheld := ApplySelection(base, p.runCfg.Selection)
	kept = ApplyRepositoryScope(kept, p.runCfg.Repository)
	// The withheld notice runs AFTER the repository narrowing, not before it: a
	// hook firing in one repository must not report branch conflicts belonging
	// to another one on every commit.
	p.noticeWithheld(ApplyRepositoryScope(withheld, p.runCfg.Repository))
	return kept, baseCount, nil
}

// orderForBudget puts the cheapest sessions first when the run is working
// against a deadline, and leaves the order alone otherwise.
//
// A fixed budget spent on one oversized transcript buys nothing: the upload is
// cut off, nothing is recorded, and the next commit spends the whole budget on
// exactly the same session. Sending the small ones first means a bounded run
// always converts its budget into recorded progress, and the large session is
// then the only one left — which is the state that tells the user plainly that
// it needs a manual run or a bigger budget.
//
// Token total is the proxy for upload size: it is already on the row (no extra
// I/O) and it tracks the transcript content that dominates the request body.
// Without a deadline the order is untouched, so an ordinary push behaves exactly
// as it did.
func orderForBudget(ctx context.Context, sessions []ingest.PushSessionRow) []ingest.PushSessionRow {
	if _, bounded := ctx.Deadline(); !bounded || len(sessions) < 2 {
		return sessions
	}
	ordered := append([]ingest.PushSessionRow(nil), sessions...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TokensTotal != ordered[j].TokensTotal {
			return ordered[i].TokensTotal < ordered[j].TokensTotal
		}
		return ordered[i].SessionID < ordered[j].SessionID
	})
	return ordered
}

// ApplyRepositoryScope narrows sessions to the canonical project identities one
// repository admits. A nil scope applies no filter.
func ApplyRepositoryScope(sessions []ingest.PushSessionRow, scope *RepositoryScope) []ingest.PushSessionRow {
	if scope == nil {
		return sessions
	}
	kept := make([]ingest.PushSessionRow, 0, len(sessions))
	for _, session := range sessions {
		if scope.Admits(session.ProjectHash) {
			kept = append(kept, session)
		}
	}
	return kept
}

// ApplySelection partitions sessions using command-prepared branch-aware
// decisions. A nil selection applies no filter (all sessions kept). Sessions
// withheld due to a multi-project branch conflict (AND-strict) are returned
// separately so callers surface them rather than silently dropping them. This
// is the single shared selection filter used by BOTH the pipeline and the CLI
// consent listing, so the dry-run, consent, and real-push sets are identical
// by construction.
func ApplySelection(sessions []ingest.PushSessionRow, sel *SessionSelection) (kept, withheld []ingest.PushSessionRow) {
	if sel == nil {
		return sessions, nil
	}
	for _, s := range sessions {
		switch sel.Decision(ingest.SessionID(s.SessionID)) {
		case ingest.BranchMatchYes:
			kept = append(kept, s)
		case ingest.BranchMatchWithheldConflict:
			withheld = append(withheld, s)
		case ingest.BranchMatchNo:
			// Not in this selection — dropped (not surfaced).
		}
	}
	return kept, withheld
}

// emptyReason explains a push that had nothing to send, in the terms of whatever
// actually made it empty.
//
// A repository scope answers for itself on EVERY path, not only when the base
// query happened to be empty. How many candidates OTHER projects had says
// nothing about this repository, yet it used to decide the wording: on a machine
// with any other pending work at all — the normal state with more than one
// repository — a scope with 150 recorded, already-pushed sessions was told its
// candidates were "all recorded for other projects", that 'peasant ingest' would
// make its sessions "recorded", and that it could push without --repository,
// which is the containment the flag exists to provide. The three states a scope
// can actually be in are read off the scope's own recorded sessions instead.
func (p *Pipeline) emptyReason(ctx context.Context, baseCount int) string {
	if p.runCfg.Repository != nil {
		all, allErr := p.allPushable(ctx)
		return p.scopedEmptyReason(all, allErr, baseCount)
	}
	// A selection that removed every candidate is its own answer. Saying
	// "already pushed, use --force" there would be the opposite of the truth and
	// would point at a flag that cannot widen a selection.
	if p.runCfg.Selection != nil && baseCount > 0 {
		return p.renderEmpty(
			"No sessions match the configured selection. Widen the selection in your config (or run 'peasant kickstart') to include the projects and branches you want pushed.",
			fmt.Sprintf("%d candidate session(s) were eligible before it. --force re-pushes already-pushed sessions; it cannot widen a selection.", baseCount))
	}
	all, allErr := p.allPushable(ctx)
	if allErr != nil || len(all) == 0 {
		return "Nothing to push. Run 'peasant ingest' first to import your sessions."
	}
	return "All sessions already pushed. Use --force to re-push."
}

// allPushable reads every session that could ever be pushed, reporting a read
// failure once rather than silently treating it as an empty store.
func (p *Pipeline) allPushable(ctx context.Context) ([]ingest.PushSessionRow, error) {
	all, err := p.store.AllPushableSessions(ctx)
	if err != nil {
		fmt.Fprintf(p.stderr, "warning: check total sessions: %v\n", err)
	}
	return all, err
}

// renderEmpty joins the one-line answer with the fuller explanation, or returns
// the one line alone under --quiet.
//
// A hook runs with --quiet, where the empty-state line is the ENTIRE output of
// every commit and every push. The full explanation embeds the resolved scope,
// and with it every directory that scope admits: on a monorepo that measured
// 4,585 bytes per commit. The summary line is complete on its own — it names the
// scope, what was found, and the next command — so nothing actionable is lost.
func (p *Pipeline) renderEmpty(summary string, detail ...string) string {
	if p.runCfg.Quiet {
		return summary
	}
	lines := make([]string, 0, len(detail)+1)
	lines = append(lines, summary)
	for _, line := range detail {
		if line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

// scopedEmptyReason explains a repository-scoped push that sent nothing, in
// terms of that scope's OWN recorded sessions.
//
// It distinguishes the three states honestly, because the remedy differs in
// each: nothing recorded here (ingest), recorded but excluded by another
// narrowing (widen that narrowing), and recorded and already published (--force,
// or new work). None of them is ever answered by dropping the scope.
func (p *Pipeline) scopedEmptyReason(all []ingest.PushSessionRow, allErr error, baseCount int) string {
	scope := p.runCfg.Repository
	if allErr != nil {
		return p.renderEmpty(
			fmt.Sprintf(
				"Nothing was pushed for scope %s: the recorded sessions could not be read to say why. Check the analytics store, then re-run '%s'.",
				scope.Summary(), p.manualPushCommand("--dry-run")),
			"Scope: "+scope.Describe(),
			"No session was eligible, and the recorded sessions could not be read to say why. --force re-pushes already-pushed sessions; it cannot widen a repository scope.")
	}
	// The one cause of a thin or empty scope that no amount of ordinary
	// ingesting fixes. It is stated in the SUMMARY line rather than the detail
	// because a hook runs with --quiet, where the summary is the only line
	// printed — the state it diagnoses used to be invisible to exactly the user
	// living in it, who then saw the same unchanged line on every commit forever.
	stale := p.staleIdentityNote(all)
	recorded := ApplyRepositoryScope(all, scope)
	if len(recorded) == 0 {
		return p.renderEmpty(
			joinSentences(
				fmt.Sprintf("No sessions are recorded for scope %s.", scope.Summary()),
				stale,
				p.noRecordedSessionsPrecondition(scope.Root)),
			"Scope: "+scope.Describe(),
			"Nothing was eligible and nothing was withheld — this scope has no recorded sessions at all, so --force has nothing to re-push.",
			priorCandidateNote(baseCount))
	}
	// The selection is applied on top, so a scope that IS populated but whose
	// sessions the configured selection removes is reported as the selection's
	// doing rather than as an empty repository.
	selected, _ := ApplySelection(recorded, p.runCfg.Selection)
	if len(selected) == 0 {
		return p.renderEmpty(
			joinSentences(
				fmt.Sprintf(
					"No sessions match the configured selection inside scope %s: %d session(s) are recorded for this repository and the selection excluded every one of them. Widen the selection in your config (or run 'peasant kickstart').",
					scope.Summary(), len(recorded)),
				stale),
			"Scope: "+scope.Describe(),
			"--force re-pushes already-pushed sessions; it cannot widen a selection, and it cannot widen a repository scope.",
			priorCandidateNote(baseCount))
	}
	return p.renderEmpty(
		joinSentences(
			fmt.Sprintf(
				"No new sessions to push for scope %s: %d session(s) are recorded for it and none changed since they were published. Re-push them with '%s'.",
				scope.Summary(), len(selected), p.manualPushCommand("--force")),
			stale),
		"Scope: "+scope.Describe(),
		fmt.Sprintf(
			"%d session(s) are recorded for it and none were eligible for this run: they have already been pushed and are unchanged since, or a provider filter excluded them. No other project was considered.",
			len(selected)),
		fmt.Sprintf(
			"New work here becomes pushable once an agent session is recorded in %s and '%s' has imported it; ingest reads every configured source wherever it is run from, so there is nothing to re-run until there is new work.",
			scope.Root, p.ingestCommand("")),
		priorCandidateNote(baseCount))
}

// manualPushCommand renders a scoped recovery from the typed command binding,
// retaining bound state directories while dropping hook-only flags.
func (p *Pipeline) manualPushCommand(extraFlag string) string {
	root := ""
	if p.runCfg.Repository != nil {
		root = p.runCfg.Repository.Root
	}
	command := githooks.ManualCommand(root, p.runCfg.CommandBinding)
	if extraFlag != "" {
		command += " " + extraFlag
	}
	return command
}

// noRecordedSessionsPrecondition states what actually has to become true before
// a scope with nothing in it can push anything.
//
// It used to say "run 'peasant ingest' inside <root>". That is not a remedy:
// ingest is not directory-scoped — it discovers every configured harness source
// on the machine regardless of the working directory — so running it there
// changes nothing, and the identical line prints on the next push, forever. What
// is missing is not a command but recorded work carrying this repository's
// identity.
func (p *Pipeline) noRecordedSessionsPrecondition(root string) string {
	return fmt.Sprintf(
		"'%s' will not change this on its own: it imports every configured source no matter which directory it runs from, and this scope is empty because no imported session carries this repository's identity yet. "+
			"A session gets that identity by being recorded while an agent works in %s (or in a clone of the same origin remote); run that ingest command after the work exists, then push again.",
		p.ingestCommand(""), root)
}

// staleIdentityNote reports work recorded inside this repository that the scope
// cannot reach, and names the ONE command that recovers it without collateral.
//
// It is printed only against evidence: a directory that still exists inside
// this worktree, whose sessions carry an identity the scope does not admit.
// Both halves matter.
//
// Without the existence half, the advice destroys data. When a repository has
// been MOVED, the recorded working directory is gone; a re-ingest re-derives
// the identity from that dead path, and the sessions become unreachable from
// every scope with nothing to re-run. The note used to print on all three
// scoped-empty branches unconditionally, including the routine already-pushed
// one a hooked repository reaches on every single commit.
//
// Without the session half there is nothing to recover, so there is nothing to
// say — and naming a concrete session is what makes the scoped command
// runnable. The scoped form is the one recommended: the unscoped
// 'peasant ingest --force' re-ingests every project on the machine and clears
// every already-published marker, after which the next push re-uploads the
// user's whole corpus — under a hook, on every commit.
func (p *Pipeline) staleIdentityNote(all []ingest.PushSessionRow) string {
	for _, unadmitted := range p.runCfg.Repository.Unadmitted() {
		count, example := 0, ""
		for _, session := range all {
			if session.ProjectHash == string(unadmitted.Hash) {
				count++
				if example == "" || session.SessionID < example {
					example = session.SessionID
				}
			}
		}
		if count == 0 {
			continue
		}
		return fmt.Sprintf(
			"%d session(s) recorded in %s carry a project identity this scope does not admit, so they are not reachable here; "+
				"re-derive those alone with '%s' — NOT the unscoped '%s', "+
				"which re-ingests every project on this machine and clears every already-published marker, so the next push re-uploads all of them.",
			count, unadmitted.Directory,
			p.ingestCommand("--force --session "+shellQuote(example)),
			p.ingestCommand("--force"))
	}
	return ""
}

// redactedSlugRemedy explains the ONE cause of a missing metadata file that the
// user cannot discover from the path, and names the command that repairs it.
//
// A version that redacted at the maximum level rewrote the host slug it recorded
// in the database and in the metadata file, while the directory it had already
// written kept the real slug. Push resolves the metadata path from the recorded
// slug, so it looks for a directory that never existed: that session can never
// publish again, and nothing about the failure says why. Peasant no longer
// redacts at ingest time, but the rows written before that are still there, and
// nothing heals them on its own — the host-slug insert ignores conflicts and an
// unchanged session is skipped by re-ingest, so only a forced re-ingest of that
// session re-derives it.
//
// It is gated on the placeholder actually being in the slug, because a missing
// metadata file has other causes — a deleted output tree, a moved
// output.basePath — for which a forced re-ingest is not the fix and saying so
// would be confidently wrong advice.
func (p *Pipeline) redactedSlugRemedy(sess ingest.PushSessionRow) string {
	if !redactionPlaceholder.MatchString(sess.HostSlug) {
		return ""
	}
	return fmt.Sprintf(
		"What: the recorded host slug %q for session %s contains the redaction placeholder %s, so the metadata path above names a directory that was never written.\n"+
			"Why: an earlier version redacted the slug it stored while the directory it had already created kept the real one, leaving the two permanently different.\n"+
			"Where: under the configured output path %s.\n"+
			"When: while reading this session's metadata, before any upload was attempted.\n"+
			"Means: nothing was published for this session and nothing was recorded as published; every later push fails here in the same way until the slug is re-derived.\n"+
			"Fix: re-derive this one session with '%s' - NOT the unscoped '%s', which re-ingests every project on this machine and clears every already-published marker, so the next push re-uploads all of them.",
		sess.HostSlug, sess.SessionID, redactionPlaceholder.FindString(sess.HostSlug), p.cfg.Output.BasePath,
		p.ingestCommand("--force --session "+shellQuote(sess.SessionID)),
		p.ingestCommand("--force"))
}

// ingestCommand renders ingest in the same config/data/state context as push.
func (p *Pipeline) ingestCommand(flags string) string {
	prefix := githooks.CommandPrefix(p.runCfg.CommandBinding)
	command := prefix + " ingest"
	if flags != "" {
		command += " " + flags
	}
	return command
}

// joinSentences joins the parts of a one-line answer, dropping the empty ones.
// The stale-identity diagnosis is absent whenever there is no evidence for it,
// and an empty middle must not leave a double space in the line a hook prints.
func joinSentences(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			kept = append(kept, part)
		}
	}
	return strings.Join(kept, " ")
}

// priorCandidateNote states how many sessions this run had before the scope
// narrowed it, and claims nothing about whose they are. Asserting they were "all
// recorded for other projects" was false whenever a selection, not the scope,
// was what removed them.
func priorCandidateNote(baseCount int) string {
	if baseCount == 0 {
		return ""
	}
	return fmt.Sprintf("%d session(s) were eligible for this run before this scope narrowed it.", baseCount)
}

// noticeHeld reports the sessions that cannot be pushed until their metrics are
// computed. Repository-scoped runs omit sessions outside that repository;
// ordinary unscoped pushes retain the complete notice.
//
// The unscoped form printed other repositories' session identifiers on every
// commit a hook fired on, including under --quiet. Its sibling withheld notice
// was already moved behind the same narrowing.
func (p *Pipeline) noticeHeld(ctx context.Context) {
	held, heldErr := p.store.SessionsWithoutMetrics(ctx)
	if heldErr != nil {
		fmt.Fprintf(p.stderr, "warning: check held sessions: %v\n", heldErr)
	}
	scoped := held
	if p.runCfg.Repository != nil {
		scoped = make([]ingest.HeldSession, 0, len(held))
		for _, session := range held {
			if p.runCfg.Repository.Admits(session.ProjectHash) {
				scoped = append(scoped, session)
			}
		}
	}
	if len(scoped) == 0 {
		return
	}
	fmt.Fprintf(p.stderr,
		"notice: %d session(s) held back (missing metrics — run 'peasant ingest' to complete):\n",
		len(scoped))
	for _, session := range scoped {
		fmt.Fprintf(p.stderr, "  held %s\n", session.SessionID)
	}
}

// noticeWithheld emits a non-fatal notice for sessions withheld from the push
// due to a multi-project branch-selection conflict. No interactive override is
// currently offered, so the pipeline flags these sessions rather than silently
// including or dropping them.
func (p *Pipeline) noticeWithheld(withheld []ingest.PushSessionRow) {
	if len(withheld) == 0 {
		return
	}
	fmt.Fprintf(p.stderr, "note: %d session(s) withheld from push (conflicting branch selection)\n", len(withheld))
	if p.runCfg.Verbose {
		for _, s := range withheld {
			fmt.Fprintf(p.stderr, "  withheld: %s/%s  (conflicting branch selection)\n", s.HostSlug, s.SessionID)
		}
	}
}

// filterByWizardSelection applies the FilterSessionIDs whitelist if set.
// When FilterSessionIDs is nil or empty, all sessions pass through.
func (p *Pipeline) filterByWizardSelection(sessions []ingest.PushSessionRow) []ingest.PushSessionRow {
	if len(p.runCfg.FilterSessionIDs) == 0 {
		return sessions
	}
	allowed := make(map[string]struct{}, len(p.runCfg.FilterSessionIDs))
	for _, id := range p.runCfg.FilterSessionIDs {
		allowed[id] = struct{}{}
	}
	var out []ingest.PushSessionRow
	for _, s := range sessions {
		if _, ok := allowed[s.SessionID]; ok {
			out = append(out, s)
		}
	}
	return out
}

// pushSession reads metadata + transcript from the filesystem and uploads them.
// All filesystem access uses p.fs.ReadFile — no os import.
func (p *Pipeline) pushSession(
	ctx context.Context,
	sess ingest.PushSessionRow,
	visibility schema.Visibility,
	license schema.License,
	emit schema.PushContractVersion,
	contentCapabilities []schema.ContentCapability,
) SessionPushResult {
	// 1. Read metadata.json via injected FileSystem. The path is resolved by the
	// shared ingest helper so subagent sessions (which live under
	// {parentID}/subagents/{id}) are read from the correct location rather than
	// the top-level {slug}/{id} dir.
	metadataPath := ingest.SessionMetadataPath(
		p.cfg.Output.BasePath, sess.HostSlug, sess.SessionID, sess.ParentID,
	)
	metaBytes, err := p.fs.ReadFile(metadataPath)
	if err != nil {
		failure := fmt.Errorf("read metadata %s: %w: %w", metadataPath, ErrMetadataMissing, err)
		if remedy := p.redactedSlugRemedy(sess); remedy != "" {
			failure = fmt.Errorf("%w\n%s", failure, remedy)
		}
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     failure,
		}
	}

	var meta ingest.UnifiedMetadata
	if err := json.Unmarshal(metaBytes, &meta); err != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("parse metadata: %w", err),
		}
	}

	// Refuse modelless sessions client-side, before any upload, so the village
	// never sees a request that would 400. The root cause is in ingest; until
	// then this is a clean client-side Error (not a Held type).
	if meta.Model == "" {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("session %s: %w", sess.SessionID, ErrNoModel),
		}
	}

	// Per-session timing recorder (Nop unless --timing threaded one onto ctx).
	rec := perf.RecorderFromContext(ctx)

	// 1b. Safety-net redaction: re-redact metadata before upload.
	// This catches sessions ingested before redaction was added or with minimal level.
	if p.redactor != nil {
		redactStart := time.Now()
		redacted := p.redactor.RedactMetadata(&meta)
		rec.RecordPhase(perf.PhaseRedact, time.Since(redactStart))
		meta = *redacted
	}

	// 2. Fetch quality metrics from the store (non-fatal on error).
	sessionID, _ := ingest.NewSessionID(sess.SessionID)
	metrics, metricsErr := p.store.GetQualityMetrics(ctx, sessionID)
	if metricsErr != nil {
		slog.Warn("failed to get quality metrics, continuing without",
			"session_id", sess.SessionID,
			"error", metricsErr,
		)
		// metrics stays nil — graceful degradation
	}

	// 3. Fetch session entries from the store. Transcript bytes and schema-owned
	// evidence are indivisible publication input, so an unreadable entry set fails closed.
	entries, entriesErr := p.store.ListEntries(ctx, sessionID)
	if entriesErr != nil {
		return entryReadFailure(sess, entriesErr, entryReadPostNegotiation)
	}
	// 3b. Redact them ONCE, here, before anything can attach them to a request.
	//
	// The entries are the transcript's text: contentPreview, toolInput and
	// toolOutput are what was recorded. They reach the wire TWICE - through the
	// transcript part below, and through PublishRequest.Entries in the metadata
	// part - and only the transcript part was ever redacted, so a publish carried
	// the recorded text verbatim in the part beside it. Redacting at the point
	// they are READ rather than at each point they are USED is what makes that
	// unrepeatable: a consumer added later cannot get the unredacted ones,
	// because after this line they do not exist.
	//
	// Unlike the two reads above this is NOT graceful-degradation territory. A
	// redaction that cannot be completed must stop the session, not publish what
	// it failed to redact.
	//
	// WHY THIS STAYS, even though both published parts now redact themselves as
	// assembled documents and so cover the entries twice over.
	//
	// It defends what document-level redaction cannot: a consumer of `entries`
	// that is not one of those parts, or a part somebody forgets to redact. Both
	// observed leaks were exactly that - transcript text reaching the wire
	// through a route whose author was not thinking about redaction. Document
	// redaction fixes a route once it exists; this makes the unredacted values
	// unavailable to write the route with in the first place.
	//
	// The guarantee is scoped to THIS FUNCTION after this rebind: `entries` is
	// re-bound here, so nothing below can reach the unredacted slice. It is not a
	// module-wide property and an earlier version of this comment implied it was.
	//
	// No measurement is quoted here on purpose. This comment used to say deleting
	// the call was green across the whole module. That was true when measured and
	// FALSE by the end of the same round, because the fail-closed guard added
	// afterwards reads this call - and the sentence went stale in the exact way
	// the sentences this work spent two rounds correcting did. The decision is
	// durable; a measurement of the suite at one moment is not, so the suite is
	// where it now lives: suppressing this call fails
	// TestPipeline_RedactionFailureStopsTheSessionInsteadOfPublishing, which
	// re-runs on every change instead of aging in a comment.
	entries, entriesErr = RedactEntries(p.redactor, entries)
	if entriesErr != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     entriesErr,
		}
	}

	// 4. Load the producer-owned durable associations. Unlike metrics and
	// transcript entries this is not optional: omitting an authoritative current
	// relationship would make a later association annotation unresolvable at the
	// village and could silently sever rewrite history.
	storedAssociations, associationErr := p.store.ListCurrentSessionCommitAssociations(ctx, sessionID)
	if associationErr != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("load durable commit associations: %w", associationErr),
		}
	}
	publishedAssociations := make([]schema.PublishedAssociation, 0, len(storedAssociations))
	for _, association := range storedAssociations {
		publishedAssociations = append(publishedAssociations, schema.PublishedAssociation{
			ID:                 association.ID,
			ObservedCommitHash: association.ObservedCommitHash,
		})
	}

	// 5. Map metadata to publishRequest JSON.
	publishJSON, err := MapMetadata(MapOptions{
		Meta:          &meta,
		Metrics:       metrics,
		Entries:       entries,
		Associations:  publishedAssociations,
		License:       license,
		Fields:        p.cfg.Push.Fields,
		TitlePipeline: p.titles,
		// The metadata part is redacted as an assembled DOCUMENT, the way the
		// transcript part always has been. Per-source redaction historically
		// missed user-derived quality fields attached from the store. MapMetadata
		// first canonicalizes the title, then applies whole-document rules to it
		// and every other assembled field.
		Redactor: p.redactor,
	})
	if err != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("map metadata: %w", err),
		}
	}

	// 4b. Client-side schema pre-flight. Validate the mapped
	// PublishRequest body against the SAME generated publish-request schema the
	// village vendors + enforces, so a body missing the required model object /
	// model.harness / model.model is rejected HERE — before any upload — with an
	// actionable error, rather than incurring a doomed round-trip that the village
	// answers with a 422. The village remains the authority; this is pre-flight.
	// (Single current contract version; the multi-version compatibility matrix is
	// contract.) This is part of the shared pre-flight both real-push and --dry-run
	// run, so a dry-run surfaces the same rejection.
	if err := schema.ValidatePublishRequest(publishJSON); err != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("session %s: %w: %w", sess.SessionID, ErrInvalidPublishBody, err),
		}
	}

	// Build human-readable title for this session (needed by both the dry-run
	// forecast and the real result).
	title := fmt.Sprintf("%s — %s (%s)",
		meta.Project.Name,
		string(meta.ModelHarness),
		time.UnixMilli(meta.Timestamp.Start).UTC().Format("2006-01-02"),
	)
	content, err := BuildTranscriptContentValidated(&meta, entries, emit, p.cfg.Push.Fields)
	if err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Status: PushStatusError, Error: fmt.Errorf("build structured content: %w", err)}
	}
	requiredCapabilities := schema.RequiredContentCapabilities(*content.SessionDetail)

	// 5. DRY-RUN DIVERGENCE. Everything above — read metadata, the metadata/model
	// guards, redaction, mapping, and the client-side schema validation — is the
	// SAME code path the real push runs; --dry-run forecasts the outcome WITHOUT
	// performing the side effects (no content upload, receipt persistence, or audit log).
	// The forecast status mirrors the real classify: a previously-pushed session
	// would be an update, otherwise a new upload.
	if p.runCfg.DryRun {
		status := PushStatusNew
		if sess.PushedAt != nil {
			status = PushStatusUpdated
		}
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Title:     title,
			Status:    status,
		}
	}
	missingCapabilities := missingContentCapabilities(contentCapabilities, requiredCapabilities)
	if len(missingCapabilities) > 0 {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error: fmt.Errorf(
				"enriched transcript push refused\n  what: session %s carries observedModel source evidence\n  why: the target Village did not advertise the exact %q capability token\n  where: push.Pipeline.pushSession\n  when: after local canonical content construction and validation, and before serialization or upload\n  meaning: no transcript bytes or metadata were sent, because silently removing the evidence would misattribute assistant output\n  fix: use a Village target that advertises the exact capability after its preservation proof passes, or push a legacy session with no observed model evidence, then retry",
				sess.SessionID, schema.ContentCapabilityObservedModelV1,
			),
		}
	}

	// 6. Build the versioned, structured content body: a
	// TranscriptContent envelope wrapping the SessionDetailPayload produced by
	// the same builder the local dashboard uses.
	//
	// Content comes from the indexed entries rather than from a raw provider
	// file, so no provider JSONL is re-read here. Those entries AS STORED are not
	// redacted - no supported level redacts before indexing, whatever this comment
	// used to claim - so they are indexed from the bytes as recorded. The entries
	// reaching this line are the ones step 3b already redacted.
	//
	// Content IS redacted on the way out now, at the effective level, through the
	// shared fail-closed redactJSONDocument helper. It invokes the same RedactJSON
	// rules as `peasant redact` while preserving decode and encode errors instead
	// of passing the original document through. Redacting the serialized document
	// here a second time is a no-op on the entry text and covers the rest of the
	// envelope.
	//
	// It used to be published as recorded, and the reason recorded here was TRUE:
	// content redaction did replace a whole fenced code block with a single
	// placeholder. I deleted that claim on a measurement that counted rules and
	// missed both that ten more are appended elsewhere and that the masking was
	// not a rule at all - it was step 1 of RedactText. The behaviour was real at
	// Standard, the only offered level.
	//
	// What was wrong was the conclusion, not the observation. The masking was an
	// inverted gate - the unoffered Maximum preserved structure while the offered
	// levels destroyed it - and fixing that in the redaction module is what makes redacting
	// content on this path safe. Matched tokens are replaced; blocks survive.
	//
	// Metadata redaction at step 1b and the village's server-side secret scan
	// remain, and are no longer the only things protecting a publish.
	// Raw project, path, branch, and remote fields are consent-gated before this
	// document is assembled; redaction is defense in depth, not a consent gate.
	transcriptBytes, err := marshalBuiltTranscriptContent(content, p.redactor)
	if err != nil {
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Status:    PushStatusError,
			Error:     fmt.Errorf("build structured content: %w", err),
		}
	}

	// 7. Upload via Publisher interface. The uploaded body is the structured
	// TranscriptContent envelope (JSON), named "--content.json" to distinguish
	// it from the legacy raw "--transcript.{jsonl,json}" body.
	//
	// When --timing is on, attach a per-upload UploadTrace to the context so the
	// Publisher's httptrace hooks split this request into setup/server time; the
	// populated sample is recorded after the call. When off, uploadCtx == ctx and
	// trace == nil (no instrumentation cost).
	uploadCtx := ctx
	var trace *perf.UploadTrace
	if rec.Enabled() {
		uploadCtx, trace = perf.ContextWithUploadTrace(ctx)
	}
	transcriptFilename := sess.SessionID + "--content.json"
	client := p.transport
	ledger := p.store
	var request schema.AuthoritativePublishRequest
	var requestDocument map[string]json.RawMessage
	if err := json.Unmarshal(publishJSON, &requestDocument); err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("build authoritative publication request from mapped metadata: %w", err)}
	}
	contentHash := schema.ComputeTranscriptContentHash(transcriptBytes)
	if err := promoteAuthoritativePublishFields(requestDocument); err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("promote mapped metadata to the authoritative publication contract: %w", err)}
	}
	requestDocument["contentHash"], _ = json.Marshal(contentHash)
	requestDocument["visibilityIntent"], _ = json.Marshal(schema.VisibilityIntentPrivate)
	authoritativeJSON, err := json.Marshal(requestDocument)
	if err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("encode authoritative publication request: %w", err)}
	}
	request, err = schema.DecodeAuthoritativePublishRequest(authoritativeJSON)
	if err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("validate authoritative publication request: %w", err)}
	}
	operation, err := schema.CanonicalizePublishRequest(request)
	if err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("canonicalize authoritative publication operation: %w", err)}
	}
	expectedFingerprint, err := schema.FingerprintPublishOperation(operation)
	if err != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("fingerprint authoritative publication operation: %w", err)}
	}
	projectHash, hashErr := schema.NewProjectHash(sess.ProjectHash)
	if hashErr != nil {
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: fmt.Errorf("publish authoritative session: local project identity is invalid: %w", hashErr)}
	}
	receipt, statusCode, err := client.PublishAuthoritative(uploadCtx, request, bytes.NewReader(transcriptBytes), transcriptFilename)
	if trace != nil {
		rec.RecordUpload(trace.Sample(sess.SessionID))
	}
	if err != nil {
		diagnosticCtx, cancelDiagnostic := persistenceContext(ctx)
		defer cancelDiagnostic()
		_ = ledger.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Stage: store.PublicationAttemptStagePublish, Message: err.Error()})
		// Distinguish a transport failure (statusCode 0 / connection error) from
		// a village rejection (non-2xx status) so the error-summary table groups
		// them correctly. Both sentinels preserve the underlying message via %w,
		// so the connection-abort heuristic (isConnectionError) still matches.
		sentinel := ErrVillageRejected
		if statusCode == 0 || isConnectionError(err) {
			sentinel = ErrNetwork
		}
		return SessionPushResult{
			SessionID: sess.SessionID,
			HostSlug:  sess.HostSlug,
			Title:     title,
			Status:    PushStatusError,
			Error:     fmt.Errorf("upload: %w: %w", sentinel, err),
		}
	}
	if receipt.ContentHash != request.ContentHash || receipt.RequestOperationFingerprint != expectedFingerprint {
		err = fmt.Errorf("authoritative receipt mismatch: Village returned content hash %s and operation fingerprint %s, expected %s and %s from the exact request; local applied state was not changed", receipt.ContentHash, receipt.RequestOperationFingerprint, request.ContentHash, expectedFingerprint)
		diagnosticCtx, cancelDiagnostic := persistenceContext(ctx)
		defer cancelDiagnostic()
		_ = ledger.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Stage: store.PublicationAttemptStageValidate, Message: err.Error()})
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: err}
	}
	if visibility == schema.VisibilityPrivate || visibility == schema.VisibilityPublic {
		desired := schema.TranscriptUpdateVisibility(visibility)
		if schema.Visibility(receipt.Visibility) != visibility {
			updated, _, updateErr := client.UpdateOwner(uploadCtx, receipt.TranscriptID, schema.OwnerTranscriptUpdateRequest{Visibility: &desired})
			if updateErr != nil {
				primary := fmt.Errorf("publication content succeeded but visibility convergence failed; the remote resource remains at its authoritative access state and no local terminal receipt was advanced; retry this session to apply the current configuration: %w", updateErr)
				diagnosticCtx, cancelDiagnostic := persistenceContext(ctx)
				defer cancelDiagnostic()
				_ = ledger.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Stage: store.PublicationAttemptStageVisibility, Message: primary.Error()})
				return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: primary}
			}
			if schema.Visibility(updated.Visibility) != visibility || updated.TranscriptID != receipt.TranscriptID || updated.TranscriptURL != receipt.TranscriptURL {
				primary := fmt.Errorf("owner update returned inconsistent authoritative identity or access state; local applied state was not changed; retry this session after verifying Village health")
				diagnosticCtx, cancelDiagnostic := persistenceContext(ctx)
				defer cancelDiagnostic()
				_ = ledger.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Stage: store.PublicationAttemptStageVisibility, Message: primary.Error()})
				return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: primary}
			}
			receipt.Visibility = visibility
			receipt.Applied.NormalizedValues.Visibility = visibility
			receipt.UpdatedAt = updated.UpdatedAt
		}
	}

	// 8. Persist the authoritative terminal receipt and advance the local
	// publication cursor with its applied license (empty means NULL).
	//
	// The upload above has already been accepted, so this write runs on a
	// context of its own: losing it to the upload budget is what made a
	// successfully-uploaded transcript re-upload on every commit.
	persistCtx, cancelPersist := persistenceContext(ctx)
	defer cancelPersist()
	if err := ledger.SavePublication(persistCtx, store.PublicationRecord{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Receipt: receipt}); err != nil {
		primary := fmt.Errorf("Village accepted the publication but the terminal receipt could not be persisted; local applied state remains non-current and this session's publication cursor was not advanced; retry this session to converge without replaying other successes: %w", err)
		diagnosticCtx, cancelDiagnostic := persistenceContext(ctx)
		defer cancelDiagnostic()
		_ = ledger.RecordPublicationAttempt(diagnosticCtx, store.PublicationAttemptDiagnostic{VillageOrigin: p.creds.VillageURL, OwnerUserID: p.creds.UserID, SessionID: sess.SessionID, ProjectHash: projectHash, Stage: store.PublicationAttemptStagePersistence, Message: primary.Error()})
		return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Title: title, Status: PushStatusError, Error: primary}
	}
	// 9. Classify result: 200 = updated existing, 201 = new upload.
	status := PushStatusNew
	if statusCode == 200 {
		status = PushStatusUpdated
	}

	return SessionPushResult{
		SessionID: sess.SessionID,
		HostSlug:  sess.HostSlug,
		Title:     title,
		Status:    status,
	}
}

type entryReadStage uint8

const (
	entryReadPreflight entryReadStage = iota
	entryReadPostNegotiation
)

func entryReadFailure(sess ingest.PushSessionRow, err error, stage entryReadStage) SessionPushResult {
	when := "before remote capability negotiation, redaction, content construction, or upload"
	meaning := "no transcript bytes or metadata were uploaded, and no publication receipt, attempt, or run audit was persisted"
	if stage == entryReadPostNegotiation {
		when = "after run-level capability negotiation and before redaction, content construction, or upload"
		meaning = "no transcript bytes or metadata were uploaded, and no publication receipt or attempt was persisted; the ordinary local run audit still records this failed session"
	}
	return SessionPushResult{SessionID: sess.SessionID, HostSlug: sess.HostSlug, Status: PushStatusError, Error: fmt.Errorf(
		"transcript entry read failed\n  what: session %s entries could not be read\n  why: the local store returned: %v\n  where: push.Pipeline transcript read\n  when: %s\n  meaning: %s\n  fix: verify the local database is readable, re-index the session if needed, and retry the push", sess.SessionID, err, when, meaning)}
}

func promoteAuthoritativePublishFields(document map[string]json.RawMessage) error {
	for _, change := range []struct{ object, oldName, newName string }{
		{object: "identity", oldName: "parentUuid", newName: "parentSessionId"},
		{object: "model", oldName: "version", newName: "harnessVersion"},
	} {
		raw, ok := document[change.object]
		if !ok {
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(raw, &object); err != nil {
			return fmt.Errorf("decode %s object: %w", change.object, err)
		}
		if value, present := object[change.oldName]; present {
			object[change.newName] = value
			delete(object, change.oldName)
		}
		encoded, err := json.Marshal(object)
		if err != nil {
			return fmt.Errorf("encode %s object: %w", change.object, err)
		}
		document[change.object] = encoded
	}

	// The authoritative contract requires a nonempty project display name, while
	// raw project names remain opt-in through PushFieldVisibility. When the raw
	// basename was withheld, derive a stable label only from the already-published
	// per-installation salted hash. This preserves project grouping without
	// bypassing the user's identity-field choice.
	rawProject, ok := document["project"]
	if !ok {
		return nil
	}
	var project map[string]json.RawMessage
	if err := json.Unmarshal(rawProject, &project); err != nil {
		return fmt.Errorf("decode project object: %w", err)
	}
	var name string
	if rawName, present := project["name"]; present {
		if err := json.Unmarshal(rawName, &name); err != nil {
			return fmt.Errorf("decode project name: %w", err)
		}
	}
	if strings.TrimSpace(name) != "" {
		return nil
	}
	var hash string
	if rawHash, present := project["hash"]; present {
		if err := json.Unmarshal(rawHash, &hash); err != nil {
			return fmt.Errorf("decode project hash: %w", err)
		}
	}
	if hash == "" {
		return nil
	}
	encodedName, err := json.Marshal(privacySafeProjectLabel(hash))
	if err != nil {
		return fmt.Errorf("encode privacy-safe project name: %w", err)
	}
	project["name"] = encodedName
	encoded, err := json.Marshal(project)
	if err != nil {
		return fmt.Errorf("encode project object: %w", err)
	}
	document["project"] = encoded
	return nil
}

// filterByProvider returns only the sessions whose ModelHarness equals provider.
func filterByProvider(sessions []ingest.PushSessionRow, provider string) []ingest.PushSessionRow {
	var out []ingest.PushSessionRow
	for _, s := range sessions {
		if s.ModelHarness == provider {
			out = append(out, s)
		}
	}
	return out
}

// isConnectionError reports whether err indicates a network-level connection failure.
// Used to trigger the connection-abort heuristic after connectionAbortThreshold failures.
func isConnectionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "network is unreachable") ||
		strings.Contains(msg, "i/o timeout")
}
