package push

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/peasant-labs/peasant/internal/githooks"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
)

// shellQuote delegates to the hook command renderer so every command Peasant
// prints for a user to paste follows the same quoting rules.
func shellQuote(value string) string { return githooks.ShellQuote(value) }

// AnnotationPushSummary is the aggregate outcome of a single annotation push run.
type AnnotationPushSummary struct {
	// Total is the number of CANDIDATE annotations considered for push (after the
	// system-type filter and the user's label/session selection), on both the
	// dry-run and live paths. The number actually uploaded is Total - Skipped.
	Total int
	// Created is the count of annotations uploaded for the first time.
	Created int
	// Updated is the count of annotations updated (server already had them).
	Updated int
	// Skipped is the count of annotations skipped: the client-side skip-gate
	// (hash already in the server manifest) plus any server-side deduplication.
	Skipped int
	// Retracted is the count of retractions sent (locally-superseded annotations
	// the server still holds). Each instructs the village to drop its stale copy.
	Retracted int
	// Errors is the count of annotations that failed.
	Errors int
	// SkipReason is non-empty when the entire annotation push was skipped
	// (e.g., village server does not support annotation push). When set,
	// the per-item counters (Created, Updated, etc.) are zero.
	SkipReason string
	// Unpublishable lists the stored annotations that cannot be put on the wire
	// in their current state, and are therefore left behind rather than sent.
	//
	// They are reported instead of aborting the run because the state is not
	// transient: an annotation whose stored target does not satisfy its own
	// target arm fails validation identically on every future attempt. Failing
	// the whole push on one of them walls the user out of publishing anything,
	// on every commit, forever — while the rest of the corpus is perfectly
	// publishable. Each carries the recovery for its own case.
	Unpublishable []UnpublishableAnnotation
}

// UnpublishableAnnotation is one stored annotation that cannot be rendered onto
// the wire, with everything a user needs to find and clear it.
type UnpublishableAnnotation struct {
	// ID is the store identifier of the annotation.
	ID string
	// AnnotatorName is who created it. It is the ONE key
	// 'peasant annotate prune' takes, so a recovery that does not name it is
	// not a recovery.
	AnnotatorName string
	// TypeID is the annotation type, so a user can tell a computed label from
	// one of their own.
	TypeID string
	// TargetKind is the target arm the annotation claims.
	TargetKind schema.TargetKind
	// SessionID is the session it targets when its target arm carries one. It is
	// empty for target kinds without session context and when a legacy re-index
	// removed the entry-target child row.
	SessionID string
	// Reason is the validation failure, in the wire contract's own words.
	Reason string
	// commandPrefix is `peasant` plus any explicitly-bound global path
	// overrides. The CLI fills it before rendering recovery; empty uses the
	// ordinary `peasant` command for non-CLI callers and tests.
	commandPrefix string
}

// WithCommandPrefix returns a copy whose recovery commands retain the caller's
// explicitly-bound config/data/state paths. The prefix is presentation context,
// not annotation data, so it remains outside exported serialization surfaces.
func (u UnpublishableAnnotation) WithCommandPrefix(prefix string) UnpublishableAnnotation {
	u.commandPrefix = prefix
	return u
}

// Recovery names the existing command that can clear this annotation and states
// its collateral. Re-ingest is deliberately not offered: it can preserve valid
// entry targets during a re-index, but it cannot infer or repair a stored target
// that has already failed wire validation.
func (u UnpublishableAnnotation) Recovery() string {
	prefix := u.commandPrefix
	if prefix == "" {
		prefix = "peasant"
	}
	if u.SessionID != "" {
		dryRun := fmt.Sprintf("%s annotate prune %s --session %s --dry-run", prefix, shellQuote(u.AnnotatorName), shellQuote(u.SessionID))
		prune := fmt.Sprintf("%s annotate prune %s --session %s", prefix, shellQuote(u.AnnotatorName), shellQuote(u.SessionID))
		list := fmt.Sprintf("%s annotate list %s", prefix, shellQuote(u.SessionID))
		return fmt.Sprintf(
			"re-ingesting cannot reconstruct a missing annotation target. Preview how many rows the smallest available cleanup would remove with '%s', and inspect that session's current annotations with '%s'. If you are prepared to recreate every annotation by %q on session %s, run '%s'. This removes all of those rows, not only %s",
			dryRun, list, u.AnnotatorName, u.SessionID, prune, u.ID)
	}
	return fmt.Sprintf(
		"re-ingesting cannot repair this stored annotation's wire validation failure, and the malformed row has no session scope available for cleanup. The existing cleanup is annotator-wide: first preview how many rows it would remove with '%s annotate prune %s --dry-run'; only if you are prepared to recreate every annotation by %q, run '%s annotate prune %s'. The annotation to clear is %s",
		prefix, shellQuote(u.AnnotatorName), u.AnnotatorName, prefix, shellQuote(u.AnnotatorName), u.ID)
}

// annotationBatchSize is the maximum number of annotations per HTTP request.
// Village bulk-upserts each bounded request. 500 items per batch limits payload
// and timeout pressure while preserving that batched server path.
const annotationBatchSize = 500

// AnnotationSelection narrows the set of annotations pushed to the village.
//
// It is the load-bearing wire for the share wizard's label-selection step:
// when the user excludes some labels (annotations), only the chosen ones are
// published. An empty selection (both maps empty) means "push everything"
// (the historical all-or-nothing behaviour).
//
// An annotation matches the selection when its store ID is in IDs OR its
// content hash is in ContentHashes. Matching on either key keeps the web
// (which knows annotation IDs from GET /api/v1/annotations) and the CLI
// (which can pass either) interchangeable.
type AnnotationSelection struct {
	// IDs is the set of annotation store IDs to include. Matched against
	// AnnotationPushRow.ID.
	IDs map[string]bool
	// ContentHashes is the set of pre-computed content hashes to include.
	// Matched against AnnotationPushRow.ContentHash (when present) or the
	// freshly computed hash of the push item.
	ContentHashes map[string]bool
	// SessionIDs, when non-nil, additionally restricts annotations to those
	// whose target session is in this set — the branch-aware session selection
	// applied to the annotation path (so a narrowed `selection` narrows
	// annotations too, not just sessions). nil = no session filter. Annotations
	// not tied to a session are not excluded by this gate on its own; an active
	// repository scope still applies its independent attribution rule.
	SessionIDs map[string]bool
	// RepositoryProjectHashes, when non-empty, is the set of canonical project
	// identities a repository-scoped push covers. It closes the one hole the
	// session gate leaves: an annotation with no target session is attributable
	// to no session, so a push scoped to one repository has no basis to publish
	// it — unless the annotation targets a project in this set, which does prove
	// it belongs to the repository the user consented to. Empty means no
	// repository scope, and the pre-existing session-gate behaviour is unchanged.
	RepositoryProjectHashes map[string]bool
}

// IsEmpty reports whether the LABEL selection (IDs/hashes) imposes no filter.
// The session-ID gate is independent and checked separately.
func (s AnnotationSelection) IsEmpty() bool {
	return len(s.IDs) == 0 && len(s.ContentHashes) == 0
}

// labelMatches applies the annotation ID / content-hash filter (empty = all).
func (s AnnotationSelection) labelMatches(row ingest.AnnotationPushRow, computedHash string) bool {
	if s.IsEmpty() {
		return true
	}
	if row.ID != "" && s.IDs[row.ID] {
		return true
	}
	if s.ContentHashes[computedHash] {
		return true
	}
	if row.ContentHash != nil && s.ContentHashes[*row.ContentHash] {
		return true
	}
	return false
}

// storedLabelMatches applies only the label keys available when a malformed row
// cannot be converted far enough to compute a fresh content hash. Without this
// gate, asking to push one annotation also reports every unrelated malformed row
// in the store, outside the user's explicit label selection.
func (s AnnotationSelection) storedLabelMatches(row ingest.AnnotationPushRow) bool {
	if s.IsEmpty() {
		return true
	}
	if row.ID != "" && s.IDs[row.ID] {
		return true
	}
	return row.ContentHash != nil && s.ContentHashes[*row.ContentHash]
}

func (s AnnotationSelection) unresolvedAnchorMatches(row ingest.AnnotationTargetAnchorRow) bool {
	if s.SessionIDs != nil && !s.SessionIDs[row.SessionID] {
		return false
	}
	if len(s.RepositoryProjectHashes) > 0 && s.SessionIDs == nil {
		return false
	}
	if s.IsEmpty() {
		return true
	}
	if row.AnnotationID != "" && s.IDs[row.AnnotationID] {
		return true
	}
	return row.ContentHash != nil && s.ContentHashes[*row.ContentHash]
}

// sessionMatches applies the selected-session gate and, independently, the
// repository-attribution gate. A nil SessionIDs set imposes no session filter,
// but non-empty RepositoryProjectHashes still fail closed.
//
// A row with no target session is not session-scoped. Without a repository scope
// it passes, as it always has. With one it must instead prove it belongs to that
// repository by targeting one of its project identities; anything Peasant cannot
// attribute is withheld, because a per-repository hook publishing unattributable
// annotations would carry content the user's consent never covered.
func (s AnnotationSelection) sessionMatches(row ingest.AnnotationPushRow) bool {
	repositoryScoped := len(s.RepositoryProjectHashes) > 0
	if s.SessionIDs == nil && !repositoryScoped {
		return true
	}

	switch row.TargetKind {
	case schema.TargetAssociation:
		if s.SessionIDs != nil {
			return row.AssociationSessionID != nil && s.SessionIDs[string(*row.AssociationSessionID)]
		}
	case schema.TargetSession, schema.TargetEntry:
		if s.SessionIDs != nil {
			return row.SessionID != nil && s.SessionIDs[*row.SessionID]
		}
	case schema.TargetProject:
		if repositoryScoped {
			return row.ProjectHash != nil && s.RepositoryProjectHashes[*row.ProjectHash]
		}
		return true
	}

	// Selection-only behavior historically admits targets without session
	// context. A repository scope cannot: no field on this row proves ownership.
	return !repositoryScoped
}

// PushAnnotations sends all system-origin annotations to the village.
//
// Equivalent to PushAnnotationsSelected with an empty selection (push
// everything) at the default concurrency. Retained for callers that do not
// narrow the set (e.g. the web sync handler).
func PushAnnotations(
	ctx context.Context,
	client *village.VillageClient,
	store ingest.AnnotationQueryStore,
	dryRun bool,
) (*AnnotationPushSummary, error) {
	return PushAnnotationsSelected(ctx, client, store, AnnotationSelection{}, dryRun, DefaultConcurrency)
}

// PushAnnotationsSelected sends the selected system-origin annotations to the village.
//
// Behavior:
//   - Queries all non-superseded annotations; filters to system type_ids only
//     (the village rejects unknown type_ids); computes each ContentHash
//     (SHA3-256 of canonical JSON); drops annotations the selection excludes.
//   - dryRun: returns the candidate count with NO HTTP calls (no manifest, no
//     skip-gate, no upload).
//   - Otherwise: fetches the server annotation manifest once and
//     SKIPS any local annotation whose hash the village already holds. FAIL-SAFE:
//     if the manifest cannot be fetched (transport error OR non-2xx, including a
//     404 from a village that predates the endpoint), the skip-gate is disabled
//     and ALL annotations are pushed — never skip on an unknown server set.
//   - Computes retractions as (server manifest) ∩ (hashes of locally
//     superseded annotations); sends them via the additive Retractions field so
//     the village drops copies this machine retired. Structurally cannot retract
//     another machine's annotation (only hashes WE superseded AND the server
//     holds are emitted).
//   - POSTs in batches of annotationBatchSize; the first batch is synchronous
//     (so a 404 short-circuits cleanly and carries the retractions), the rest run
//     bounded-parallel at `concurrency`.
//
// Returns a summary and any fatal error. Partial failures (per-item server
// errors) are reflected in AnnotationPushSummary.Errors, not the error return.
func PushAnnotationsSelected(
	ctx context.Context,
	client *village.VillageClient,
	store ingest.AnnotationQueryStore,
	selection AnnotationSelection,
	dryRun bool,
	concurrency int,
) (result *AnnotationPushSummary, resultErr error) {
	rec := perf.RecorderFromContext(ctx)
	span := rec.StartChildSpan(perf.StagePushAnnotationsPublish, perf.ParentSpanFromContext(ctx), nil)
	ctx = perf.ContextWithParentSpan(ctx, span.ID())
	defer func() {
		outcome := perf.OutcomeOK
		if resultErr != nil || (result != nil && (result.Errors > 0 || len(result.Unpublishable) > 0)) {
			outcome = perf.OutcomeFailed
			rec.Error(perf.StagePushAnnotationsPublish, fmt.Errorf("annotation publication was incomplete; inspect command diagnostics and repair failed annotations before retrying"), nil)
		} else if dryRun || (result != nil && (result.SkipReason != "" || result.Created+result.Updated+result.Retracted == 0)) {
			outcome = perf.OutcomeSkipped
		}
		span.End(outcome, nil)
	}()
	rec.Count(perf.CounterPushDBReads, 1, perf.UnitCount, nil)
	rows, err := store.ListSystemAnnotations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list system annotations: %w", err)
	}
	rec.Count(perf.CounterPushDBReads, 1, perf.UnitCount, nil)
	unresolved, err := store.ListUnresolvedAnnotationTargetAnchors(ctx, "")
	if err != nil {
		return nil, fmt.Errorf("check unresolved annotation targets before publish: %w", err)
	}
	inScopeUnresolved := unresolved[:0]
	for _, row := range unresolved {
		if selection.unresolvedAnchorMatches(row) {
			inScopeUnresolved = append(inScopeUnresolved, row)
		}
	}
	if len(inScopeUnresolved) > 0 {
		return nil, unresolvedAnnotationTargetError(inScopeUnresolved)
	}

	items := make([]schema.AnnotationPushItem, 0, len(rows))
	var unpublishable []UnpublishableAnnotation
	for _, row := range rows {
		// Apply the repository/session boundary before validation. A malformed
		// annotation that cannot be attributed to this repository is outside a
		// repository-scoped hook's consent just like any other annotation; it must
		// not leak into that hook's report merely because validation happens before
		// content hashing.
		if !selection.sessionMatches(row) {
			continue
		}
		item, err := annotationRowToPushItem(row)
		if err != nil {
			// A row that cannot be rendered onto the wire is left behind and
			// reported, not turned into a fatal error. Its state is permanent —
			// the same row fails the same way on every attempt — so aborting
			// here stops the user publishing ANY annotation, in ANY repository,
			// on every push, until they find and delete it unaided.
			if selection.storedLabelMatches(row) {
				unpublishable = append(unpublishable, describeUnpublishable(row, err))
			}
			continue
		}
		// Compute content hash over all other fields.
		item.ContentHash = item.ComputeContentHash()
		// Honour the user's label selection: skip excluded annotations.
		if !selection.labelMatches(row, item.ContentHash) {
			continue
		}
		items = append(items, item)
	}

	summary := &AnnotationPushSummary{Total: len(items), Unpublishable: unpublishable}

	if dryRun {
		// Dry-run: no HTTP calls. Report what would be uploaded (pre-skip-gate,
		// since the gate requires a manifest fetch).
		return summary, nil
	}

	// The retraction source is the locally-superseded annotations. Queried up
	// front (cheap local DB read) so that when there is neither anything to push
	// NOR anything locally retired, we make NO network call at all.
	rec.Count(perf.CounterPushDBReads, 1, perf.UnitCount, nil)
	superseded, err := store.ListSupersededAnnotations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list superseded annotations: %w", err)
	}
	// Retraction mutates the village just as publication does. Keep it inside the
	// same selected-session/repository boundary rather than retracting annotations
	// from unrelated repositories during a repository-scoped hook run.
	eligibleSuperseded := superseded[:0]
	for _, row := range superseded {
		if selection.sessionMatches(row) {
			eligibleSuperseded = append(eligibleSuperseded, row)
		}
	}
	superseded = eligibleSuperseded
	if len(items) == 0 && len(superseded) == 0 {
		return summary, nil
	}

	// Server-authoritative skip-gate: fetch the manifest once. On any
	// failure, manifestOK is false ⇒ push everything (fail-safe).
	manifest, manifestOK := resolveManifestSet(ctx, client)

	// Skip annotations the server already holds; the remainder is pushed.
	// summary.Total stays the CANDIDATE count (set above) on both the dry-run and
	// live paths — the post-skip-gate remainder is derivable as Total - Skipped.
	toSend := items
	if manifestOK {
		toSend = make([]schema.AnnotationPushItem, 0, len(items))
		for _, it := range items {
			if manifest[it.ContentHash] {
				summary.Skipped++ // client-side skip: already on the server
				continue
			}
			toSend = append(toSend, it)
		}
	}

	// Retractions are the manifest intersected with locally-superseded hashes. This requires the
	// manifest (the server side of the intersection); without it we retract
	// nothing (fail-safe: never delete on an unknown server set).
	var retractions []string
	if manifestOK {
		retractions, err = intersectSupersededHashes(superseded, manifest)
		if err != nil {
			return nil, err
		}
	}
	// Nothing to upload and nothing to retract ⇒ done (no POST at all). This is
	// the no-change steady-state path: 0 annotations sent.
	if len(toSend) == 0 && len(retractions) == 0 {
		return summary, nil
	}

	return sendAnnotationBatches(ctx, client, summary, toSend, retractions, concurrency)
}

func unresolvedAnnotationTargetError(rows []ingest.AnnotationTargetAnchorRow) error {
	first := rows[0]
	return fmt.Errorf("annotation push refused: %d unresolved annotation target(s) need repair before publication; what: annotation %s on session %s has no safely resolved transcript entry target; why: re-index repair could not prove the old target maps to one unique current entry; where: annotation_target_anchors unresolved state before Village annotation push; when: before building the annotation upload payload; user impact: Peasant did not publish annotations because doing so could mislead readers by attaching user labels to the wrong transcript entry; how to fix: inspect the session with 'peasant annotate list %s', recreate or remove the affected annotation from annotator %q, then re-run the same push", len(rows), first.AnnotationID, first.SessionID, first.SessionID, first.AnnotatorName)
}

// resolveManifestSet fetches the village annotation manifest and returns its hash
// set plus an ok flag. ok is false on ANY fetch failure — the two distinct paths
// are (1) a transport/unreachable error (statusCode 0) and (2) a non-2xx status
// (including a 404 from a village that predates the endpoint). Both disable the
// skip-gate so the caller pushes everything (fail-safe); they are logged
// distinctly for diagnosis but never cause a skip on an unknown server set.
func resolveManifestSet(ctx context.Context, client *village.VillageClient) (set map[string]bool, ok bool) {
	manifest, statusCode, err := client.GetAnnotationManifest(ctx)
	if err != nil {
		if statusCode == 0 {
			slog.Warn("annotation manifest unreachable — pushing all (fail-safe)", "error", err)
		} else {
			slog.Warn("annotation manifest fetch failed — pushing all (fail-safe)", "status", statusCode, "error", err)
		}
		return nil, false
	}
	set = make(map[string]bool, len(manifest.Hashes))
	for _, h := range manifest.Hashes {
		set[h] = true
	}
	return set, true
}

// describeUnpublishable turns a row that failed wire rendering into the report a
// user can act on: which annotation, whose it is, what it targets, and why the
// contract refused it.
func describeUnpublishable(row ingest.AnnotationPushRow, err error) UnpublishableAnnotation {
	sessionID := ""
	if row.SessionID != nil {
		sessionID = *row.SessionID
	} else if row.AssociationSessionID != nil {
		sessionID = string(*row.AssociationSessionID)
	}
	return UnpublishableAnnotation{
		ID:            row.ID,
		AnnotatorName: row.AnnotatorName,
		TypeID:        row.TypeID,
		TargetKind:    row.TargetKind,
		SessionID:     sessionID,
		Reason:        err.Error(),
	}
}

// intersectSupersededHashes returns the content-hashes to retract: the
// intersection of the server manifest with the hashes of LOCALLY-superseded
// annotations. A valid row is recomputed via the same path used for publication;
// a legacy row whose target was later lost uses its persisted content hash, the
// identity of the valid form that was published. A manifest hash that is not
// among our superseded rows is never emitted, so a retraction structurally
// cannot delete another machine's annotation.
func intersectSupersededHashes(superseded []ingest.AnnotationPushRow, manifest map[string]bool) ([]string, error) {
	seen := make(map[string]bool)
	var retractions []string
	for _, row := range superseded {
		item, err := annotationRowToPushItem(row)
		var hash string
		if err != nil {
			// The row may have been valid when it was published and only lost its
			// target during an older re-index. Its stored content hash is the
			// durable identity of that already-published form and remains safe to
			// intersect with the server manifest. Without one, there is no safe
			// remote identity to retract.
			if row.ContentHash == nil || *row.ContentHash == "" {
				slog.Warn("annotation retraction skipped: the superseded annotation cannot be rendered and has no stored content hash",
					"annotation_id", row.ID, "annotator", row.AnnotatorName, "error", err)
				continue
			}
			hash = *row.ContentHash
		} else {
			hash = item.ComputeContentHash()
		}
		if manifest[hash] && !seen[hash] {
			seen[hash] = true
			retractions = append(retractions, hash)
		}
	}
	return retractions, nil
}

// sendAnnotationBatches POSTs toSend in batches of annotationBatchSize, carrying
// retractions on the first request. The first batch is sent synchronously so a
// 404 (village without annotation push) short-circuits gracefully; the remaining
// batches run bounded-parallel at `concurrency`. Per-batch timing is recorded
// (Nop unless --timing is on). Counters are aggregated under a mutex.
func sendAnnotationBatches(
	ctx context.Context,
	client *village.VillageClient,
	summary *AnnotationPushSummary,
	toSend []schema.AnnotationPushItem,
	retractions []string,
	concurrency int,
) (*AnnotationPushSummary, error) {
	if concurrency < 1 {
		concurrency = 1
	}
	rec := perf.RecorderFromContext(ctx)

	// Partition the items into batch slices. When there are no items but there
	// ARE retractions, a single retraction-only batch carries them.
	var batches [][]schema.AnnotationPushItem
	for start := 0; start < len(toSend); start += annotationBatchSize {
		end := min(start+annotationBatchSize, len(toSend))
		batches = append(batches, toSend[start:end])
	}
	if len(batches) == 0 {
		batches = append(batches, []schema.AnnotationPushItem{}) // retraction-only request
	}

	var mu sync.Mutex
	accumulate := func(resp *schema.AnnotationPushResponse) {
		if resp == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		summary.Created += resp.Created
		summary.Updated += resp.Updated
		summary.Skipped += resp.Skipped
		summary.Errors += resp.Errors
	}

	// sendBatch issues one request. retract is non-nil only for the first batch.
	sendBatch := func(ctx context.Context, items []schema.AnnotationPushItem, retract []string) (statusCode int, err error) {
		req := schema.AnnotationPushRequest{Annotations: items, Retractions: retract}
		batchStart := time.Now()
		resp, statusCode, err := client.UploadAnnotations(ctx, req)
		rec.RecordPhase(perf.PhaseAnnotation, time.Since(batchStart))
		if err != nil {
			return statusCode, err
		}
		accumulate(resp)
		return statusCode, nil
	}

	// First batch synchronously (carries retractions; lets a 404 short-circuit).
	if statusCode, err := sendBatch(ctx, batches[0], retractions); err != nil {
		if statusCode == http.StatusNotFound {
			summary.SkipReason = "village server does not support annotation push (requires v1.1.5+)"
			return summary, nil
		}
		return summary, fmt.Errorf("upload annotations (batch 0): %w", err)
	}
	// Retractions share the first request. Count them only after that request
	// succeeded; setting this before transmission reported remote deletions that
	// may never have reached the village when the request failed or timed out.
	summary.Retracted = len(retractions)

	// Remaining batches bounded-parallel at `concurrency`.
	if len(batches) > 1 {
		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(concurrency)
		for i := 1; i < len(batches); i++ {
			batchItems := batches[i]
			idx := i
			g.Go(func() error {
				if _, err := sendBatch(gctx, batchItems, nil); err != nil {
					return fmt.Errorf("upload annotations (batch %d): %w", idx, err)
				}
				return nil
			})
		}
		if err := g.Wait(); err != nil {
			return summary, err
		}
	}

	return summary, nil
}

// annotationRowToPushItem converts an AnnotationPushRow from the store into
// a schema.AnnotationPushItem ready for wire transmission.
// ContentHash is NOT set here — the caller must call item.ComputeContentHash().
func annotationRowToPushItem(row ingest.AnnotationPushRow) (schema.AnnotationPushItem, error) {
	item := schema.AnnotationPushItem{
		TargetKind:    row.TargetKind,
		TypeID:        row.TypeID,
		Value:         row.Value,
		IsPrimary:     row.IsPrimary,
		Confidence:    row.Confidence,
		Reason:        row.Reason,
		AnnotatorName: row.AnnotatorName,
		Provenance:    row.Provenance,
	}

	// Set target-specific fields based on TargetKind.
	switch row.TargetKind {
	case schema.TargetSession:
		item.SessionID = row.SessionID
	case schema.TargetEntry:
		if row.EntryIndex != nil && row.EntryEndIndex != nil {
			item.EntryTarget = &schema.AnnotationEntryTarget{
				EntryIndex: *row.EntryIndex,
				EndIndex:   *row.EntryEndIndex,
			}
			if row.SessionID != nil {
				item.EntryTarget.SessionID = *row.SessionID
			}
		}
	case schema.TargetAnnotation:
		item.AnnotationID = row.AnnotationID
	case schema.TargetProject:
		if row.ProjectHash != nil {
			projectHash, err := schema.NewProjectHash(*row.ProjectHash)
			if err != nil {
				return schema.AnnotationPushItem{}, fmt.Errorf("prepare project annotation %q for publish: stored project hash is invalid: %w; the annotation cannot be published until its project identity is repaired or the stale annotation is removed", row.ID, err)
			}
			item.ProjectHash = &projectHash
		}
	case schema.TargetAssociation:
		item.TargetAssociationID = row.TargetAssociationID
	}
	if err := item.Validate(); err != nil {
		return schema.AnnotationPushItem{}, fmt.Errorf("prepare annotation %q for publish: schema target validation failed before network transmission: %w", row.ID, err)
	}

	return item, nil
}
