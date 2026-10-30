package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"

	"github.com/spf13/cobra"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// Flag names of the explicit selection a push can be given.
const (
	pushSessionFlag           = "session"
	pushCollectiveFlag        = "collective"
	pushWithoutSubagentsFlag  = "without-subagents"
	planSelectedByRequest     = "request"
	planSelectedByParent      = "parent"
	planShareWould            = "share"
	explicitCollectiveRuleMsg = "--collective publishes private and unlicensed, and shares each sent transcript only with the named collectives"
)

// explicitSelection is what --session and --sessions-from named, in the order
// given, without duplicates.
type explicitSelection struct{ IDs []string }

// resolveExplicitSelection merges --session and --sessions-from: the
// repeated --session values first, then the list, de-duplicated in order.
// set is false when neither flag was given. Every refusal is a usage error.
func resolveExplicitSelection(cmd *cobra.Command) (explicitSelection, bool, error) {
	named, _ := cmd.Flags().GetStringArray(pushSessionFlag)
	fromList, listSet, err := clicontract.ReadSessionsFrom(cmd)
	if err != nil {
		return explicitSelection{}, true, err
	}
	set := listSet || cmd.Flags().Changed(pushSessionFlag)
	if !set {
		return explicitSelection{}, false, nil
	}
	seen := map[string]bool{}
	var ids []string
	for _, raw := range append(append([]string{}, named...), fromList...) {
		id := strings.TrimSpace(raw)
		if id == "" {
			return explicitSelection{}, true, clicontract.NewUsageError(fmt.Sprintf(
				"%s: --session was given an empty value\n"+
					"What went wrong: a session id is empty.\n"+
					"Why: an empty selection must never widen to every session.\n"+
					"Fix: pass a session id, for example from `peasant sessions list --json`.",
				cmd.CommandPath()))
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return explicitSelection{IDs: ids}, true, nil
}

// selectedSession is one session the run is pinned to. Parent is the
// requested session at the root of the subagent tree that pulled a subagent
// session in, and "" for a session named directly.
type selectedSession struct {
	ID     string
	Parent string
}

// expandedSelection is the requested sessions with the subagent sessions that
// travel with them.
type expandedSelection struct {
	sessions []selectedSession
	// omitted counts the subagent sessions --without-subagents left out, the
	// whole tree, by the requested session at its root.
	omitted map[string]int
}

// ids returns every selected session id, in plan order.
func (e expandedSelection) ids() []string {
	out := make([]string, 0, len(e.sessions))
	for _, s := range e.sessions {
		out = append(out, s.ID)
	}
	return out
}

// inheritance maps each subagent session pulled in with a named session to
// that named session, the root whose saved-selection decision it follows.
func (e expandedSelection) inheritance() map[string]string {
	out := map[string]string{}
	for _, s := range e.sessions {
		if s.Parent != "" {
			out[s.ID] = s.Parent
		}
	}
	return out
}

// childSessionReader is the store read that names a session's subagent
// sessions.
type childSessionReader interface {
	ChildSessionsForParent(ctx context.Context, parentID string) ([]store.ChildSessionRow, error)
}

// maxSubagentDepth bounds how deep a subagent tree is followed. A tree deeper
// than this is refused rather than published in part.
const maxSubagentDepth = 64

// expandSubagents adds each requested session's subagent sessions after it,
// the whole tree of them: a subagent session's own subagent sessions too,
// breadth first. A subagent session is its own transcript, and its parent's
// publication refers to it, so without it the parent's references point at
// transcripts the readers cannot open. Each descendant is listed under the
// requested session that pulled it in. A session named directly is never
// pulled in a second time under a parent, and its own tree is expanded under
// it; naming a subagent session never pulls its parent. The walk keeps a seen
// set, so a cycle in the stored parent links ends it, and it refuses a tree
// deeper than maxSubagentDepth.
func expandSubagents(ctx context.Context, db childSessionReader, requested []string, withoutSubagents bool) (expandedSelection, error) {
	direct := make(map[string]bool, len(requested))
	for _, id := range requested {
		direct[id] = true
	}
	out := expandedSelection{omitted: map[string]int{}}
	seen := map[string]bool{}
	for _, root := range requested {
		out.sessions = append(out.sessions, selectedSession{ID: root})
		seen[root] = true
		level := []string{root}
		for depth := 1; len(level) > 0; depth++ {
			var next []string
			for _, id := range level {
				children, err := db.ChildSessionsForParent(ctx, id)
				if err != nil {
					return expandedSelection{}, fmt.Errorf("village push: read the subagent sessions of %s: %w; nothing was uploaded", id, err)
				}
				for _, child := range children {
					if direct[child.SessionID] || seen[child.SessionID] {
						continue
					}
					if depth > maxSubagentDepth {
						return expandedSelection{}, fmt.Errorf("village push: the subagent sessions of %s nest deeper than %d levels; nothing was uploaded; name the sessions to publish with --session, or pass --without-subagents", root, maxSubagentDepth)
					}
					seen[child.SessionID] = true
					next = append(next, child.SessionID)
					if withoutSubagents {
						out.omitted[root]++
						continue
					}
					out.sessions = append(out.sessions, selectedSession{ID: child.SessionID, Parent: root})
				}
			}
			level = next
		}
	}
	return out, nil
}

// unknownSessions returns the requested ids the store does not hold, in
// request order.
func unknownSessions(ctx context.Context, db *store.Store, ids []string) ([]string, map[string]bool, error) {
	all, err := db.AllSessionIDs(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("village push: read the stored sessions to resolve --session: %w; nothing was uploaded", err)
	}
	known := make(map[string]bool, len(all))
	for _, id := range all {
		known[id] = true
	}
	var unknown []string
	for _, id := range ids {
		if !known[id] {
			unknown = append(unknown, id)
		}
	}
	return unknown, known, nil
}

// unknownSessionRefusal is the refusal of a real run that names a session the
// store does not hold.
func unknownSessionRefusal(unknown []string) error {
	return fmt.Errorf(
		"village push: %d requested session(s) are not in the local store: %s\n"+
			"What went wrong: --session or --sessions-from named a session this computer has not recorded.\n"+
			"Why: a selection is published exactly as named, so a mistyped id is refused rather than skipped; nothing was uploaded.\n"+
			"Fix: check the ids with `peasant sessions list --json`, or run with --dry-run --json to see a plan entry for each id.",
		len(unknown), strings.Join(unknown, ", "))
}

// explicitSharePlan returns the autoPublishPlan the rules would build, with
// the collectives map filled from --collective for each pinned session. It
// lets the push reuse updateHold, reportAutoPublishShares and RuleShare. The
// rules file is not read: --collective is the developer's own publish action.
func explicitSharePlan(ids []string, collectives []schema.VillageUUID) autoPublishPlan {
	plan := autoPublishPlan{
		pinned:      make(map[string]bool, len(ids)),
		collectives: make(map[string][]schema.VillageUUID, len(ids)),
		held:        map[string]string{},
		publicHeld:  &sync.Map{},
	}
	for _, id := range ids {
		plan.pinned[id] = true
		plan.collectives[id] = slices.Clone(collectives)
	}
	return plan
}

// refuseExplicitCollectiveFlags refuses the flags that cannot combine with
// --collective. It runs before any work, so the refusal exits as nothing
// attempted.
func refuseExplicitCollectiveFlags(cmd *cobra.Command, explicit bool, visibility string) error {
	if !explicit {
		return fmt.Errorf("village push: --collective needs --session or --sessions-from; %s, and it shares only sessions you name; nothing was uploaded; name the sessions to share", explicitCollectiveRuleMsg)
	}
	for _, flag := range []string{"require-auto-publish-rule", "auto-publish-event"} {
		if cmd.Flags().Changed(flag) {
			return fmt.Errorf("village push: --%s was given with --collective; the hook flags publish through your auto-publish rules, and --collective publishes past them by hand; nothing was uploaded; drop --%s or drop --collective", flag, flag)
		}
	}
	if cmd.Flags().Changed("visibility") && schema.Visibility(visibility) != schema.VisibilityPrivate {
		return fmt.Errorf("village push: --visibility %s was given with --collective; %s; nothing was uploaded; drop --visibility, or pass --visibility private", visibility, explicitCollectiveRuleMsg)
	}
	if cmd.Flags().Changed("license") {
		return fmt.Errorf("village push: --license was given with --collective; %s; nothing was uploaded; drop --license", explicitCollectiveRuleMsg)
	}
	return nil
}

// collectiveConfirmationRefusal is the refusal of an explicit --collective run
// that cannot ask for confirmation.
func collectiveConfirmationRefusal(cmd *cobra.Command) error {
	return clicontract.ConfirmationRequired(cmd, "publishing past your auto-publish rules with --collective")
}

// promptCollectiveConfirmation asks once before an explicit --collective run
// skips the auto-publish rules. Anything but y or Y declines.
func promptCollectiveConfirmation(r io.Reader, w io.Writer, sessions int, names []string) (bool, error) {
	fmt.Fprintf(w, "Publish %d session(s) privately to %s? This skips your auto-publish rules for these sessions. [y/N] ",
		sessions, strings.Join(names, ", "))
	scanner := bufio.NewScanner(r)
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return false, fmt.Errorf("read confirmation: %w", err)
		}
		return false, nil
	}
	answer := strings.TrimSpace(scanner.Text())
	return answer == "y" || answer == "Y", nil
}

// PlanVerdict is what a push does, or would do, with one requested session.
type PlanVerdict string

const (
	PlanWillPublish PlanVerdict = "will_publish"
	PlanWillUpdate  PlanVerdict = "will_update"
	PlanUnchanged   PlanVerdict = "unchanged"
	PlanHeld        PlanVerdict = "held"
	PlanExcluded    PlanVerdict = "excluded"
	PlanNotReady    PlanVerdict = "not_ready"
	PlanUnknown     PlanVerdict = "unknown_session"
	// PlanFailed is a session the run tried and could not publish: an upload
	// or a local check that failed for a reason that is not readiness.
	PlanFailed PlanVerdict = "failed"
)

// AllPlanVerdicts is the closed set of plan verdicts.
var AllPlanVerdicts = []PlanVerdict{
	PlanWillPublish, PlanWillUpdate, PlanUnchanged, PlanHeld, PlanExcluded, PlanNotReady, PlanUnknown, PlanFailed,
}

// NewPlanVerdict parses a verdict, refusing any value outside the set.
func NewPlanVerdict(raw string) (PlanVerdict, error) {
	if slices.Contains(AllPlanVerdicts, PlanVerdict(raw)) {
		return PlanVerdict(raw), nil
	}
	return "", fmt.Errorf("unknown plan verdict %q", raw)
}

// PlanReason says why a session got its verdict.
type PlanReason string

const (
	ReasonNotInStore          PlanReason = "not_in_store"
	ReasonOutsideRepository   PlanReason = "outside_repository"
	ReasonHarnessMismatch     PlanReason = "harness_mismatch"
	ReasonBranchSelectionExcl PlanReason = "branch_selection_excluded"
	ReasonNeedsIngest         PlanReason = "needs_ingest"
	ReasonCohortNotReady      PlanReason = "cohort_not_ready"
	ReasonLatestAttemptKept   PlanReason = "latest_attempt_kept"
	ReasonUnchangedContent    PlanReason = "unchanged_content"
	ReasonTranscriptIsPublic  PlanReason = "transcript_is_public"
	ReasonNoMetrics           PlanReason = "no_metrics"
	ReasonNoModelReply        PlanReason = "no_model_reply"
)

// AllPlanReasons is the closed set of plan reasons.
var AllPlanReasons = []PlanReason{
	ReasonNotInStore, ReasonOutsideRepository, ReasonHarnessMismatch, ReasonBranchSelectionExcl, ReasonNeedsIngest,
	ReasonCohortNotReady, ReasonLatestAttemptKept, ReasonUnchangedContent, ReasonTranscriptIsPublic, ReasonNoMetrics,
	ReasonNoModelReply,
}

// NewPlanReason parses a reason, refusing any value outside the set.
func NewPlanReason(raw string) (PlanReason, error) {
	if slices.Contains(AllPlanReasons, PlanReason(raw)) {
		return PlanReason(raw), nil
	}
	return "", fmt.Errorf("unknown plan reason %q", raw)
}

// planShare is one collective a session's transcript is, or would be, shared
// with. A real run adds the outcome RuleShare reported.
type planShare struct {
	CollectiveID schema.VillageUUID         `json:"collective_id"`
	Step         schema.SyncPushStep        `json:"step"`
	Would        string                     `json:"would"`
	Outcome      schema.SyncPushStepOutcome `json:"outcome,omitempty"`
	Reason       string                     `json:"reason,omitempty"`
}

// planEntry is one requested session in the --json plan. Subagent sessions
// that travel with a main session are listed under it.
type planEntry struct {
	SessionID  string      `json:"session_id"`
	Verdict    PlanVerdict `json:"verdict"`
	Reason     PlanReason  `json:"reason,omitempty"`
	Detail     string      `json:"detail,omitempty"`
	Title      string      `json:"title,omitempty"`
	SelectedBy string      `json:"selectedBy"`
	Next       string      `json:"next,omitempty"`
	Shares     []planShare `json:"shares,omitempty"`
	// SelectionInheritedFrom names the parent whose saved-selection decision
	// let this subagent session through, set only where its own branch would
	// have been excluded.
	SelectionInheritedFrom string      `json:"selectionInheritedFrom,omitempty"`
	SubagentsOmitted       int         `json:"subagentsOmitted,omitempty"`
	Subagents              []planEntry `json:"subagents,omitempty"`
}

// pushCounts is the summary of a push: how many sessions ended in each
// outcome. A push that names its sessions derives it from its plan, so the
// counts and the plan cannot disagree; any other push reads the pipeline's
// counters.
type pushCounts struct {
	New, Updated, Skipped, Errors, Held int
	// NotReady, Excluded and UnknownSession are counted only from a plan.
	NotReady, Excluded, UnknownSession int
	// fromPlan says the counts are a plan's tally.
	fromPlan bool
}

// countsOfResult is the summary of a push that does not name its sessions.
func countsOfResult(result *push.PushResult) pushCounts {
	return pushCounts{New: result.New, Updated: result.Updated, Skipped: result.Skipped, Errors: result.Errors, Held: result.Held}
}

// countsOfPlan tallies a plan: every entry, a subagent session under its
// parent included, adds one to exactly the count of its verdict, so the
// counts add up to the number of entries.
func countsOfPlan(entries []planEntry) pushCounts {
	c := pushCounts{fromPlan: true}
	var add func(entry planEntry)
	add = func(entry planEntry) {
		switch entry.Verdict {
		case PlanWillPublish:
			c.New++
		case PlanWillUpdate:
			c.Updated++
		case PlanUnchanged:
			c.Skipped++
		case PlanHeld:
			c.Held++
		case PlanFailed:
			c.Errors++
		case PlanNotReady:
			c.NotReady++
		case PlanExcluded:
			c.Excluded++
		case PlanUnknown:
			c.UnknownSession++
		}
		for _, sub := range entry.Subagents {
			add(sub)
		}
	}
	for _, entry := range entries {
		add(entry)
	}
	return c
}

// runCounts is the summary a push prints: the plan's tally when it has a
// plan, otherwise the pipeline's counters.
func runCounts(result *push.PushResult, plan *jsonPushPlan) pushCounts {
	if plan != nil {
		return countsOfPlan(plan.Plan)
	}
	return countsOfResult(result)
}

// planCollective is a collective the run shares with, by id and name.
type planCollective struct {
	ID   schema.VillageUUID `json:"id"`
	Name string             `json:"name"`
}

// planInputs is everything explainRequested reads besides the store. Each
// value is a decision the run already made; the plan reports it and never
// decides again.
type planInputs struct {
	selection   expandedSelection
	known       map[string]bool
	result      *push.PushResult
	runCfg      push.PipelineConfig
	cfg         *config.Config
	autoPublish autoPublishPlan
	collectives []schema.VillageUUID
	refs        []string
	shared      []schema.SyncPushSessionResult
	dryRun      bool
}

// explainRequested returns one entry per requested session, in request order,
// with each subagent session under its parent. It reads the run's PushResult
// for the sessions the pipeline considered, and the store with the run's own
// narrowing for the rest, so every verdict restates a decision the run made.
func explainRequested(ctx context.Context, db *store.Store, in planInputs) ([]planEntry, error) {
	pushable, err := db.AllPushableSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("village push: read the stored sessions to explain the plan: %w", err)
	}
	rows := make(map[string]ingest.PushSessionRow, len(pushable))
	for _, row := range pushable {
		rows[row.SessionID] = row
	}
	results := map[string]push.SessionPushResult{}
	if in.result != nil {
		for _, sr := range in.result.Sessions {
			results[sr.SessionID] = sr
		}
	}
	sharedBy := map[string]schema.SyncPushSessionResult{}
	for _, s := range in.shared {
		sharedBy[s.SessionID] = s
	}

	entries := []planEntry{}
	index := map[string]int{}
	for _, selected := range in.selection.sessions {
		entry := explainOne(selected.ID, in, rows, results, sharedBy)
		if selected.Parent == "" {
			entry.SelectedBy = planSelectedByRequest
			entry.SubagentsOmitted = in.selection.omitted[selected.ID]
			index[selected.ID] = len(entries)
			entries = append(entries, entry)
			continue
		}
		entry.SelectedBy = planSelectedByParent
		parent := index[selected.Parent]
		entries[parent].Subagents = append(entries[parent].Subagents, entry)
	}
	return entries, nil
}

// explainOne decides the verdict of one session.
func explainOne(id string, in planInputs, rows map[string]ingest.PushSessionRow, results map[string]push.SessionPushResult, shared map[string]schema.SyncPushSessionResult) planEntry {
	entry := planEntry{SessionID: id}
	if !in.known[id] {
		entry.Verdict, entry.Reason = PlanUnknown, ReasonNotInStore
		return entry
	}
	root, inherited := in.runCfg.SelectionInheritedFrom(id)
	if sr, considered := results[id]; considered {
		entry.Title = sr.Title
		if inherited {
			entry.SelectionInheritedFrom = root
		}
		switch sr.Status {
		case push.PushStatusNew:
			entry.Verdict = PlanWillPublish
			entry.Shares = planShares(id, in, shared)
		case push.PushStatusUpdated:
			entry.Verdict = PlanWillUpdate
			entry.Shares = planShares(id, in, shared)
		case push.PushStatusSkipped:
			entry.Verdict, entry.Reason = PlanUnchanged, ReasonUnchangedContent
			if len(in.refs) > 0 {
				entry.Next = "peasant village share " + id + " --collective " + strings.Join(in.refs, " --collective ")
			}
		case push.PushStatusHeld:
			entry.Verdict, entry.Detail = PlanHeld, sr.HeldReason
			if in.autoPublish.heldPublic(id) {
				entry.Reason = ReasonTranscriptIsPublic
			}
		default:
			if reason, ok := push.NotReadyReasonOf(sr.Error); ok {
				entry.Verdict = PlanNotReady
				switch reason {
				case push.NotReadyLatestAttemptKept:
					entry.Reason = ReasonLatestAttemptKept
					entry.Detail = "a newer incomplete attempt is kept beside the last-good transcript; re-index the complete source, or publish the last-good with --last-good"
				case push.NotReadyNoModelReply:
					entry.Reason = ReasonNoModelReply
					entry.Detail = "the session never reached a model: it holds no assistant reply, only setup or local commands, so its source names no model; peasant online requires one and re-indexing cannot add it; leave the session out of the push"
				default:
					entry.Reason = ReasonNeedsIngest
					// --force: a plain harvest skips a source it has
					// already indexed and reports the session unchanged.
					entry.Detail = "run `peasant harvest --session " + id + " --force` to re-index this session from its source, then retry"
				}
				return entry
			}
			entry.Verdict = PlanFailed
			if sr.Error != nil {
				entry.Detail = sr.Error.Error()
			}
		}
		return entry
	}
	row, pushable := rows[id]
	if !pushable {
		entry.Verdict, entry.Reason = PlanNotReady, ReasonNoMetrics
		entry.Detail = "the session has no computed metrics yet; run `peasant harvest --session " + id + "`, then retry"
		return entry
	}
	// The checks run in the order the run narrows (push.NarrowCandidates): the
	// candidate query's harness filter, the auto-publish rules, the saved
	// selection, then the repository scope, so a session two of them exclude
	// is reported with the one the run applied.
	pinnedOut := in.runCfg.PinnedSessionIDs != nil && !in.runCfg.PinnedSessionIDs[id]
	switch {
	case !harnessAdmitted(row, in.runCfg, in.cfg):
		entry.Verdict, entry.Reason = PlanExcluded, ReasonHarnessMismatch
	case pinnedOut && in.autoPublish.held[id] != "":
		entry.Verdict, entry.Detail = PlanHeld, in.autoPublish.held[id]
	case pinnedOut:
		// A rule-required run sends only sessions a rule binds.
		entry.Verdict = PlanExcluded
		entry.Detail = "no auto-publish rule binds it, and this run publishes only sessions a rule binds"
	case in.runCfg.Selection != nil && in.runCfg.Selection.Decision(ingest.SessionID(id)) != ingest.BranchMatchYes:
		entry.Verdict, entry.Reason = PlanExcluded, ReasonBranchSelectionExcl
		if inherited {
			// The parent passed the saved selection but is not sent: it was
			// left out or is not ready, so there is no decision to inherit.
			entry.Detail = "its own branch is outside the saved selection, and it follows its parent " + root + "'s selection only when this run sends that parent; the parent is not sent"
		}
	case in.runCfg.Repository != nil && !in.runCfg.Repository.Admits(row.ProjectHash):
		entry.Verdict, entry.Reason = PlanExcluded, ReasonOutsideRepository
	default:
		// Every filter admits it, yet the run reports nothing for it: the run
		// stopped before it reached the session.
		entry.Verdict = PlanFailed
		entry.Detail = "the run stopped before it reached this session, for example at its time budget or on an earlier error; nothing was sent for it; retry the push"
	}
	return entry
}

// harnessAdmitted restates the candidate query's harness filter: the
// --source-harness flag, or the configured sources of the by-source method.
func harnessAdmitted(row ingest.PushSessionRow, runCfg push.PipelineConfig, cfg *config.Config) bool {
	if runCfg.SourceProvider != "" {
		return row.ModelHarness == runCfg.SourceProvider
	}
	if cfg != nil && !runCfg.Force && cfg.Push.Method == config.PushMethodBySource {
		return slices.Contains(cfg.Push.Sources, row.ModelHarness)
	}
	return true
}

// planShares lists the collectives a sent session is shared with: what a dry
// run would ask for, and what a real run's RuleShare reported.
func planShares(id string, in planInputs, shared map[string]schema.SyncPushSessionResult) []planShare {
	if len(in.collectives) == 0 {
		return nil
	}
	outcomes := map[schema.VillageUUID]schema.SyncPushStepResult{}
	if result, ok := shared[id]; ok {
		for _, step := range result.Steps {
			if step.Step == schema.SyncPushStepAddCollective && step.CollectiveID != nil {
				outcomes[*step.CollectiveID] = step
			}
		}
	}
	shares := make([]planShare, 0, len(in.collectives))
	for _, collective := range in.collectives {
		share := planShare{CollectiveID: collective, Step: schema.SyncPushStepAddCollective, Would: planShareWould}
		if step, ok := outcomes[collective]; ok && !in.dryRun {
			share.Outcome, share.Reason = step.Outcome, step.Reason
		}
		shares = append(shares, share)
	}
	return shares
}
