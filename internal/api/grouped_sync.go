package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
)

// syncSessionEntry is one flat sync row together with the status the flat sync
// route renders for it. The grouped sync view is opt-in over exactly this set,
// so it can never list a session the flat route withholds.
type syncSessionEntry struct {
	row    ingest.PushSessionRow
	status string
}

// loadSyncEntries applies the EXISTING flat sync predicate and status policy:
// every pushable session (one with metrics) is listed, and a session whose
// publication metadata is not a verified capture is held. Both the flat route
// and the grouped view read through this ONE function, so the two views of the
// same route cannot disagree about which sessions are offered.
func loadSyncEntries(ctx context.Context, db syncSessionReader) ([]syncSessionEntry, error) {
	sessions, err := db.AllPushableSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("api.loadSyncEntries: query pushable sessions for the local sync list: %w; no sync rows were returned; retry, and if the failure repeats inspect the Peasant store", err)
	}

	heldMap := make(map[string]bool)
	held, heldErr := db.SessionsWithoutMetrics(ctx)
	if heldErr == nil {
		for _, session := range held {
			heldMap[session.SessionID] = true
		}
	}

	metadata, metadataErr := push.LoadPublicationMetadata(ctx, db, sessions)
	entries := make([]syncSessionEntry, 0, len(sessions))
	for _, session := range sessions {
		input := metadata[session.SessionID]
		if metadataErr != nil || !push.PublicationMetadataReady(input) {
			heldMap[session.SessionID] = true
		}
		entries = append(entries, syncSessionEntry{row: session, status: computeSyncStatus(session, heldMap, input.Readiness)})
	}
	return entries, nil
}

// syncGroupedReader is the store surface the grouped sync view needs beyond the
// flat predicate: the durable grouping evidence (purpose and immediate owner)
// and the redaction-safe preview each SessionSummary carries. *store.Store is
// the production implementation.
type syncGroupedReader interface {
	syncSessionReader
	GroupingEvidenceForSessions(ctx context.Context, sessionIDs []string) (map[string]store.GroupingEvidenceRow, error)
	FirstUserMessageBulk(ctx context.Context, sessionIDs []string) (map[string]string, error)
}

// gatherGroupedSyncCandidates builds the registered GroupedRouteSync predicate.
// The candidate set is the flat sync set, projected into the shared grouped row
// shape with its Sync mirror. Helper grouping then applies the SAME partition
// the sessions route applies: helper_review candidates become members of their
// owner group, every other session stays an ordinary transcript row. A helper
// that is not pushable never enters the flat set, so it contributes no row and
// no count.
func (h *syncHandler) gatherGroupedSyncCandidates(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error) {
	if filters.Variant != GroupedRouteSync {
		return nil, fmt.Errorf("api.syncHandler.gatherGroupedSyncCandidates: route variant %q is not the sync chooser route; the grouped sync list cannot replay another route's predicate; request view=grouped from /api/v1/sync/sessions", filters.Variant)
	}
	reader, ok := any(h.store).(syncGroupedReader)
	if !ok || h.store == nil {
		return nil, fmt.Errorf("api.syncHandler.gatherGroupedSyncCandidates: the local store is unavailable while the grouped sync chooser list was requested; no sessions were returned because push eligibility and grouping evidence cannot be read; start Peasant with its normal store and retry")
	}

	entries, err := loadSyncEntries(ctx, reader)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return []GroupedCandidate{}, nil
	}

	// Every grouped route applies the persisted selection scope to its
	// candidates (the grouped list contract). The legacy flat sync route is
	// unchanged; the opt-in grouped view is where the chooser's selection
	// boundary is enforced. Origin scope is deliberately NOT applied here: the
	// sync route's predicate is the pushable set, and helper sessions are
	// agent-driven by definition.
	policy, err := syncSelectionPolicy(h.config)
	if err != nil {
		return nil, err
	}
	visibleEntries := make([]syncSessionEntry, 0, len(entries))
	for _, entry := range entries {
		visible, visErr := policy.Visible(syncVisibilityCandidate(entry.row))
		if visErr != nil {
			return nil, fmt.Errorf("api.syncHandler.gatherGroupedSyncCandidates: apply the persisted selection to pushable session %q: %w; no grouped rows were returned because a partial chooser list would expose an unverified set; run `peasant kickstart` to repair the selection, then retry", entry.row.SessionID, visErr)
		}
		if visible {
			visibleEntries = append(visibleEntries, entry)
		}
	}
	entries = visibleEntries
	if len(entries) == 0 {
		return []GroupedCandidate{}, nil
	}

	ids := make([]string, len(entries))
	for i := range entries {
		ids[i] = entries[i].row.SessionID
	}
	evidence, err := reader.GroupingEvidenceForSessions(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("api.syncHandler.gatherGroupedSyncCandidates: read durable grouping evidence for the sync chooser: %w; no grouped rows were returned; retry, and if the failure repeats inspect the Peasant store", err)
	}
	previews, err := reader.FirstUserMessageBulk(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("api.syncHandler.gatherGroupedSyncCandidates: read session previews for the sync chooser: %w; no grouped rows were returned; retry, and if the failure repeats inspect the Peasant store", err)
	}

	candidates := make([]GroupedCandidate, 0, len(entries))
	for _, entry := range entries {
		row := entry.row
		ev := evidence[row.SessionID]
		summary, syncSummary, buildErr := buildSyncGroupedRow(row, entry.status, ev, previews[row.SessionID])
		if buildErr != nil {
			return nil, buildErr
		}
		candidate := GroupedCandidate{
			Summary:    summary,
			StartMs:    row.StartMs,
			StableID:   row.SessionID,
			HostSlug:   row.HostSlug,
			Harness:    row.ModelHarness,
			Purpose:    ev.Purpose,
			OwnerState: ev.OwnerState,
			Sync:       &syncSummary,
		}
		if ev.OwnerTargetLocalID != nil {
			candidate.OwnerTargetLocalID = *ev.OwnerTargetLocalID
		}
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// buildSyncGroupedRow derives the matching SessionSummary/LocalSyncSummary pair
// for one pushable row from the SAME database read, so the schema's mirror check
// (identity, harness, project, tokens, turn count and optional input count) can
// only hold or fail closed here rather than downstream.
func buildSyncGroupedRow(row ingest.PushSessionRow, status string, ev store.GroupingEvidenceRow, preview string) (schema.SessionSummary, schema.LocalSyncSummary, error) {
	projectHash, hashErr := schema.NewProjectHash(row.ProjectHash)
	if hashErr != nil {
		return schema.SessionSummary{}, schema.LocalSyncSummary{}, fmt.Errorf("api.buildSyncGroupedRow: session %q has an invalid stored project hash %q while building the sync chooser row: %w; run `peasant ingest verify` and repair the store, then retry", row.SessionID, row.ProjectHash, hashErr)
	}
	harness := schema.Harness(row.ModelHarness)
	start := time.UnixMilli(row.StartMs)
	summary := schema.SessionSummary{
		ID:                   row.SessionID,
		Harness:              harness,
		StartTime:            start.UTC(),
		DurationMins:         float64(row.DurationMs) / 60000,
		TotalTokens:          row.TokensTotal,
		TurnCount:            row.TurnCount,
		InputSubmissionCount: ev.InputSubmissionCount,
		ToolCallCount:        row.ToolCalls,
		// The sync arm validates Project against Sync.ProjectName, so both carry
		// the stored project name verbatim rather than a display transform.
		Project:       row.ProjectName,
		ProjectHash:   projectHash,
		Preview:       preview,
		SessionOrigin: schema.SessionOrigin(row.SessionOrigin),
		Purpose:       ev.Purpose,
	}
	syncSummary := schema.LocalSyncSummary{
		ID:                   row.SessionID,
		Harness:              harness,
		ProjectName:          row.ProjectName,
		ProjectHash:          projectHash,
		HostSlug:             row.HostSlug,
		StartTime:            start.UTC().Format(time.RFC3339),
		DurationMs:           row.DurationMs,
		TotalTokens:          row.TokensTotal,
		TurnCount:            row.TurnCount,
		Model:                row.ModelID,
		InputSubmissionCount: ev.InputSubmissionCount,
		SyncStatus:           status,
	}
	return summary, syncSummary, nil
}

// syncSelectionPolicy returns the persisted selection projection the grouped
// sync view applies. A nil configuration means no persisted selection, which is
// the explicit all-data policy rather than a refusal.
func syncSelectionPolicy(cfg *config.Config) (sessionvisibility.Policy, error) {
	selection := config.SelectionConfig{Mode: config.SelectionModeAll}
	if cfg != nil {
		selection = cfg.Selection
	}
	policy, err := sessionvisibility.New(selection)
	if err != nil {
		return sessionvisibility.Policy{}, fmt.Errorf("api.syncSelectionPolicy: build the selection projection for the grouped sync chooser: %w; no grouped rows were returned because the saved selection cannot be applied; run `peasant kickstart` to repair it, then retry", err)
	}
	return policy, nil
}

// syncVisibilityCandidate projects one pushable sync row onto the selection
// input. ClonePath uses the recorded session worktree, falling back to the
// canonical project path the push reader already resolved.
func syncVisibilityCandidate(row ingest.PushSessionRow) sessionvisibility.Candidate {
	branch := ""
	if row.GitBranch != nil {
		branch = *row.GitBranch
	}
	return sessionvisibility.Candidate{
		SessionID:       ingest.SessionID(row.SessionID),
		Harness:         defaults.Harness(row.ModelHarness),
		GitRemote:       row.GitRemote,
		ProjectName:     row.ProjectName,
		ClonePath:       ingest.ClonePath(row.ProjectPath),
		GitBranch:       branch,
		Origin:          sessionorigin.Origin(row.SessionOrigin),
		ParentSessionID: ingest.SessionID(row.ParentID),
	}
}

// serveGroupedSyncSessions answers GET /api/v1/sync/sessions?view=grouped. It
// assembles the same opt-in grouped payload the sessions route serves, over the
// exact sync predicate, and attaches each rendered group's opaque member scope
// so the registered member operation can replay the sync route rather than the
// sessions route.
func (h *syncHandler) serveGroupedSyncSessions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set(defaults.HeaderContentType, defaults.ContentJSON.String())
	if h.scopeIssuer == nil {
		writeAPIError(w, http.StatusServiceUnavailable,
			"Grouped sync sessions could not be listed because no member scope issuer is wired in api.syncHandler.serveGroupedSyncSessions, which ran while view=grouped was requested. No rows were returned, because a group without a replayable scope cannot be expanded safely. Omit view=grouped for the flat sync list, or restart Peasant with its normal server wiring, then retry.",
			"grouped_unavailable")
		return
	}
	filters := GroupedFilters{Variant: GroupedRouteSync}
	candidates, err := h.gatherGroupedSyncCandidates(r.Context(), filters)
	if err != nil {
		writeDiscoveryError(w, "failed to fetch grouped sync sessions", err)
		return
	}
	payload, err := buildGroupedListPayload(filters, candidates, 1, 0, h.scopeIssuer)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError,
			"Grouped sync sessions could not be assembled because the grouped projection failed in internal/api.serveGroupedSyncSessions after the sync rows were read. No grouped response was returned, because a partial list would misstate the counts. Refresh the share chooser; if the failure repeats, inspect the Peasant server logs.",
			"grouped_projection_failure")
		return
	}
	_ = json.NewEncoder(w).Encode(payload)
}
