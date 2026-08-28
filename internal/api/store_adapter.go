package api

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/peasant-labs/peasant/internal/codemap"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/projectlabel"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// displayProjectName prefers the row's git remote (formatted "host:owner/repo"
// by projectlabel) over its already-coalesced canonical-cwd-or-hash value, so
// every session-level Project field the API exposes matches the same naming
// preference as the Map/Review project picker.
func displayProjectName(canonicalRemote *string, fallback string) string {
	remote := ""
	if canonicalRemote != nil {
		remote = *canonicalRemote
	}
	return projectlabel.Label(remote, fallback)
}

// StoreDataProvider adapts a store.Store to the DataProvider interface.
// It maps store-native row types to the API payload types expected by
// the server and WebSocket hub.
type StoreDataProvider struct {
	store *store.Store
	// codemap serves the Map/Review surfaces; wired with the
	// production gitops repository factory and codegraph builder in
	// NewStoreDataProvider (see store_adapter_map.go).
	codemap    *codemap.Service
	visibility sessionvisibility.Policy
	// pathIdentityResolver resolves stored session worktrees into exact clone
	// identities before user-facing discovery matching.
	pathIdentityResolver ingest.PathIdentityResolver
	// fs reads a session's ORIGINAL source transcript file so SessionByID can
	// overlay full turn content over the DB's bounded content_preview (see
	// transcript.BuildContentOverlay). Defaulted to the real OS filesystem by
	// NewStoreDataProvider; NewStoreDataProviderWithFS lets tests inject a
	// MemFS-backed source file.
	fs ingest.FileSystem
}

// NewStoreDataProvider creates a StoreDataProvider backed by the given store,
// reading source transcripts from the real OS filesystem.
func NewStoreDataProvider(s *store.Store, visibility sessionvisibility.Policy) *StoreDataProvider {
	return NewStoreDataProviderWithFSAndResolver(s, visibility, &ingest.OSFileSystem{}, ingest.NewPhysicalPathResolver())
}

// NewStoreDataProviderWithFS is NewStoreDataProvider with an injectable
// FileSystem, for tests that need SessionByID's content-overlay re-index to
// read from a MemFS fixture instead of disk.
func NewStoreDataProviderWithFS(s *store.Store, visibility sessionvisibility.Policy, fs ingest.FileSystem) *StoreDataProvider {
	return NewStoreDataProviderWithFSAndResolver(s, visibility, fs, ingest.NewPhysicalPathResolver())
}

// NewStoreDataProviderWithFSAndResolver is NewStoreDataProvider with injectable
// filesystem and physical-path boundaries. Production constructors supply the
// real implementations; focused discovery tests can supply deterministic ones.
func NewStoreDataProviderWithFSAndResolver(
	s *store.Store,
	visibility sessionvisibility.Policy,
	fs ingest.FileSystem,
	resolver ingest.PathIdentityResolver,
) *StoreDataProvider {
	if resolver == nil {
		resolver = ingest.NewPhysicalPathResolver()
	}
	return &StoreDataProvider{
		store:                s,
		codemap:              newCodemapService(s, visibility, resolver),
		visibility:           visibility,
		pathIdentityResolver: resolver,
		fs:                   fs,
	}
}

// Compile-time interface guard.
var _ DataProvider = (*StoreDataProvider)(nil)

// Sessions returns all sessions mapped from store rows to ingest.Session.
func (p *StoreDataProvider) Sessions(ctx context.Context) ([]ingest.Session, error) {
	rows, err := p.store.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: sessions: %w", err)
	}

	sessions := make([]ingest.Session, 0, len(rows))
	for i := range rows {
		visible, err := p.discoverableSessionRow(&rows[i])
		if err != nil {
			return nil, fmt.Errorf("store adapter: sessions visibility: %w", err)
		}
		if visible {
			sessions = append(sessions, sessionRowToSession(&rows[i]))
		}
	}
	return sessions, nil
}

// SessionSummaries returns the lightweight session summaries a person may be
// OFFERED: the discovery list, scoped by both the persisted kickstart selection
// and the declared session origin.
func (p *StoreDataProvider) SessionSummaries(ctx context.Context) ([]SessionSummary, error) {
	rows, err := p.store.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: session summaries: %w", err)
	}

	visibleRows := make([]store.SessionRow, 0, len(rows))
	for i := range rows {
		// A session that has not completed an index pass has a metrics row (so a
		// turn count exists) but no entries yet, so opening it would show no
		// turns. Withhold it from every discovery list until it is viewable. This
		// is a discovery gate, not access control: SessionSummariesByID (the
		// deep-link path) deliberately does not apply it, so a held identifier
		// still resolves. Counts derive from this same visible set, so the list
		// and its "N sessions" total cannot disagree.
		if rows[i].IndexedAt == nil {
			continue
		}
		visible, err := p.discoverableSessionRow(&rows[i])
		if err != nil {
			return nil, fmt.Errorf("store adapter: session summaries visibility: %w", err)
		}
		if visible {
			visibleRows = append(visibleRows, rows[i])
		}
	}

	return p.summariesFromRows(ctx, visibleRows)
}

// SessionSummariesByID returns summaries for exactly the named sessions, in the
// order they were named. It is the link-resolution path, deliberately OUTSIDE
// the discovery machinery: it applies NEITHER origin scope NOR selection scope,
// because the caller is not browsing — it already holds the identifiers.
// Discovery scope decides what a person is OFFERED; it has never decided what
// they may OPEN, and this project already settled that rule for stored sessions
// a narrowed selection hides.
//
// An identifier that names no stored session is OMITTED rather than reported as
// an error, so one stale link inside an evidence set cannot fail the resolution
// of the sessions that do exist. An empty request returns an empty result.
//
// It shares summariesFromRows with the list path, so the two cannot drift in
// WHAT a summary contains; they differ only in WHICH rows they return.
func (p *StoreDataProvider) SessionSummariesByID(ctx context.Context, ids []string) ([]SessionSummary, error) {
	if len(ids) == 0 {
		return []SessionSummary{}, nil
	}
	// Read through the same stored-row query the list path uses, so both paths
	// see one projection of a session rather than two that can disagree. The
	// preview lookup below is already scoped to the requested identifiers.
	rows, err := p.store.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: session summaries by id: %w", err)
	}
	byID := make(map[string]store.SessionRow, len(rows))
	for i := range rows {
		byID[rows[i].SessionID] = rows[i]
	}
	requested := make([]store.SessionRow, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		row, ok := byID[id]
		if !ok {
			continue
		}
		requested = append(requested, row)
	}
	return p.summariesFromRows(ctx, requested)
}

// summariesFromRows is the ONE row-to-summary construction site. Both the
// discovery list and the by-id link resolution build their payload here, so a
// field added to one is present in the other by construction.
func (p *StoreDataProvider) summariesFromRows(ctx context.Context, rows []store.SessionRow) ([]SessionSummary, error) {
	// Collect session IDs for a single bulk preview query.
	sessionIDs := make([]string, len(rows))
	for i := range rows {
		sessionIDs[i] = rows[i].SessionID
	}

	// FirstUserMessageBulk issues one IN(...) query for all session IDs.
	// Sessions with no indexed user entry are omitted from the map (empty preview).
	previews, err := p.store.FirstUserMessageBulk(ctx, sessionIDs)
	if err != nil {
		return nil, fmt.Errorf("store adapter: session summaries preview: %w", err)
	}

	summaries := make([]SessionSummary, len(rows))
	for i := range rows {
		row := &rows[i]
		projectHash, hashErr := schema.NewProjectHash(row.ProjectHash)
		if hashErr != nil {
			return nil, fmt.Errorf("store adapter: session summary %q has invalid stored project hash %q while mapping the sessions wire payload: %w; run `peasant ingest verify` and repair the store before retrying", row.SessionID, row.ProjectHash, hashErr)
		}
		s := SessionSummary{
			ID:            row.SessionID,
			Harness:       defaults.Harness(row.ModelHarness),
			StartTime:     time.UnixMilli(row.StartMs),
			DurationMins:  row.DurationMinutes,
			TotalTokens:   row.TokensTotal,
			TurnCount:     row.TurnCount,
			ToolCallCount: row.ToolCalls,
			Project:       displayProjectName(row.CanonicalRemote, row.ProjectName),
			ProjectHash:   projectHash,
			Preview:       previews[row.SessionID],
			// The producer's declaration travels with the summary, so a consumer
			// reads a decision instead of re-deriving one from turn shapes.
			SessionOrigin: schema.SessionOrigin(row.SessionOrigin),
		}
		if row.Outcome != nil {
			s.Outcome = *row.Outcome
		}
		s.ParentSessionID = row.ParentID
		summaries[i] = s
	}
	return summaries, nil
}

// sessionCandidate projects a stored row onto the visibility package's input.
func (p *StoreDataProvider) sessionCandidate(row *store.SessionRow) sessionvisibility.Candidate {
	remote := ""
	if row.CanonicalRemote != nil {
		remote = *row.CanonicalRemote
	}
	branch := ""
	if row.GitBranch != nil {
		branch = *row.GitBranch
	}
	parent := ""
	if row.ParentID != nil {
		parent = *row.ParentID
	}
	return sessionvisibility.Candidate{
		SessionID:       ingest.SessionID(row.SessionID),
		Harness:         defaults.Harness(row.ModelHarness),
		GitRemote:       remote,
		ProjectName:     row.ProjectName,
		ClonePath:       p.resolveSessionClonePath(row),
		GitBranch:       branch,
		Origin:          sessionorigin.Origin(row.SessionOrigin),
		ParentSessionID: ingest.SessionID(parent),
	}
}

// visibleSessionRow applies SELECTION scope only. It backs the aggregate
// surfaces (dashboard totals, trends), which count the work a store holds for
// the selected projects rather than listing sessions to choose from. Origin
// scope is a discovery-list boundary and would silently drop agent-driven work
// out of those totals.
func (p *StoreDataProvider) visibleSessionRow(row *store.SessionRow) (bool, error) {
	return p.visibility.Visible(p.sessionCandidate(row))
}

// discoverableSessionRow applies BOTH scopes. It backs the surfaces a person
// picks from: the REST sessions list, the WebSocket sessions channel, and
// therefore the /share chooser.
func (p *StoreDataProvider) discoverableSessionRow(row *store.SessionRow) (bool, error) {
	return p.visibility.VisibleForDiscovery(p.sessionCandidate(row))
}

func (p *StoreDataProvider) resolveSessionClonePath(row *store.SessionRow) ingest.ClonePath {
	raw := row.GitWorktree
	// SessionRow.ProjectName is the canonical cwd when one exists and otherwise
	// the project hash. A hash is display fallback, not path evidence.
	if raw == "" && row.ProjectName != row.ProjectHash {
		raw = row.ProjectName
	}
	if raw == "" || p.pathIdentityResolver == nil {
		return ""
	}
	resolved, err := p.pathIdentityResolver.Resolve(raw)
	if err != nil {
		// Stored paths can disappear. Keep discovery available, but do not cast
		// unavailable text into exact identity or fall back from a recorded
		// worktree to a possibly different project path.
		return ""
	}
	return resolved
}

func (p *StoreDataProvider) visibleSessionRows(ctx context.Context) ([]store.SessionRow, error) {
	rows, err := p.store.AllSessions(ctx)
	if err != nil {
		return nil, err
	}
	visibleRows := make([]store.SessionRow, 0, len(rows))
	for i := range rows {
		visible, visibilityErr := p.visibleSessionRow(&rows[i])
		if visibilityErr != nil {
			return nil, visibilityErr
		}
		if visible {
			visibleRows = append(visibleRows, rows[i])
		}
	}
	return visibleRows, nil
}

// SessionByID returns a single session by ID, or an error if not found.
// Populates Turns from session_entries for the trajectory view.
// Uses SessionDetailByID to include extra fields (git_remote, pushed_at, project_path).
func (p *StoreDataProvider) SessionByID(ctx context.Context, id string) (*ingest.Session, error) {
	detailRow, err := p.store.SessionDetailByID(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("store adapter: session by id: %w", err)
	}
	if detailRow == nil {
		return nil, fmt.Errorf("session not found: %s", id)
	}
	s := sessionRowToSession(&detailRow.SessionRow)

	// Populate detail-specific fields from the extended row.
	s.Project = detailRow.ProjectName
	s.Model = detailRow.ModelID
	if detailRow.GitBranch != nil {
		s.GitBranch = *detailRow.GitBranch
	}
	if detailRow.GitRemote != nil {
		s.GitRemote = *detailRow.GitRemote
	}
	s.ProjectPath = detailRow.ProjectPath
	s.PushedAt = detailRow.PushedAt

	// Populate turns from session_entries.
	sid, sidErr := ingest.NewSessionID(id)
	if sidErr != nil {
		// Invalid session ID format; return session without turns.
		return &s, nil
	}

	// Enrich quality metrics with the full session_metrics row. The detail row
	// only carries the v1 quality columns; the M-series and cost signals needed
	// by the Highlights scorecard live only in session_metrics. Non-fatal: if
	// the lookup fails we keep the v1 metrics derived from the detail row.
	if metrics, mErr := p.store.GetMetrics(ctx, sid); mErr == nil && metrics != nil {
		full := metrics.QualityMetrics
		s.Metadata.Quality = &full
	}

	entries, err := p.store.ListEntries(ctx, sid)
	if err != nil {
		// Non-fatal: return session without turns rather than failing entirely.
		return &s, nil
	}
	turns, validationErr := transcript.EntriesToTurnsValidated(entries)
	if validationErr != nil {
		return nil, fmt.Errorf("store adapter: session %q observed model evidence is invalid after ListEntries and before session-detail emission: %w", id, validationErr)
	}
	s.Turns = turns

	// Overlay full turn content from the source transcript, re-indexed with
	// truncation disabled (see transcript.BuildContentOverlay) — otherwise
	// every turn's Content stops at the DB's bounded content_preview
	// (defaults.ContentPreviewLimit), which is what the session_detail WS
	// channel was silently doing before: main turn bodies
	// cut off mid-word around 2000 chars).
	//
	// GATED on transcript.AnyContentTruncated: BuildContentOverlay does a
	// full re-parse of the source transcript from disk, which is real cost
	// this fix would otherwise pay on EVERY session view regardless of size.
	// The common case — nothing in this session hit the preview limit — has
	// nothing to recover, so it skips the re-parse entirely.
	//
	// Best-effort even when gated in: if source info can't be looked up, the
	// file is missing, or the harness has no full-content indexer wired (see
	// BuildContentOverlay's doc comment), turns simply keep their existing
	// (possibly truncated) content rather than failing the whole session view.
	if transcript.AnyContentTruncated(entries) {
		if info, infoErr := p.store.SessionSourceInfo(ctx, id); infoErr == nil && info != nil {
			if overlay, overlayErr := transcript.BuildContentOverlay(ctx, p.fs, defaults.Harness(info.Harness), ingest.ResolvedPath(info.SourcePath), schema.SessionID(id)); overlayErr == nil {
				for i := range s.Turns {
					if content, ok := overlay[s.Turns[i].Index]; ok {
						s.Turns[i].Content = content
					}
				}
			}
		}
	}
	return &s, nil
}

// ChildSessionsForParent returns child (subagent) session references for a parent session.
func (p *StoreDataProvider) ChildSessionsForParent(ctx context.Context, parentID string) ([]ChildSessionRef, error) {
	rows, err := p.store.ChildSessionsForParent(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("store adapter: child sessions: %w", err)
	}
	refs := make([]ChildSessionRef, len(rows))
	for i, r := range rows {
		remote := r.CanonicalRemote
		refs[i] = ChildSessionRef{
			ID:        r.SessionID,
			StartTime: time.UnixMilli(r.StartMs),
			Project:   projectlabel.Label(remote, r.ProjectName),
		}
	}
	return refs, nil
}

// DashboardMetrics returns dashboard aggregates from the store.
func (p *StoreDataProvider) DashboardMetrics(ctx context.Context) (*DashboardPayload, error) {
	rows, err := p.visibleSessionRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: dashboard visibility: %w", err)
	}
	harnessBreakdown := make(map[ingest.Harness]int)
	var totalTokens, totalTurns int
	var totalDuration float64
	for i := range rows {
		row := &rows[i]
		totalTokens += row.TokensTotal
		totalTurns += row.TurnCount
		totalDuration += row.DurationMinutes
		harnessBreakdown[defaults.Harness(row.ModelHarness)]++
	}
	avgDuration, avgTurns := 0.0, 0.0
	if len(rows) > 0 {
		avgDuration = totalDuration / float64(len(rows))
		avgTurns = float64(totalTurns) / float64(len(rows))
	}

	return &DashboardPayload{
		TotalSessions:      len(rows),
		TotalTokens:        totalTokens,
		AvgDurationMins:    avgDuration,
		HarnessBreakdown:   harnessBreakdown,
		AvgTurnsPerSession: avgTurns,
		AcceptanceRate:     0.0, // v1: requires outcome inference from v2
	}, nil
}

// TrendsData returns daily summary data mapped to the trends payload.
func (p *StoreDataProvider) TrendsData(ctx context.Context) (*TrendsPayload, error) {
	rows, err := p.visibleSessionRows(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: trends visibility: %w", err)
	}
	type dayAggregate struct{ tokens, sessions int }
	byDay := make(map[string]dayAggregate)
	var totalTokens, totalSessions int
	for i := range rows {
		date := time.UnixMilli(rows[i].StartMs).UTC().Format("2006-01-02")
		agg := byDay[date]
		agg.tokens += rows[i].TokensTotal
		agg.sessions++
		byDay[date] = agg
		totalTokens += rows[i].TokensTotal
		totalSessions++
	}
	dates := make([]string, 0, len(byDay))
	for date := range byDay {
		dates = append(dates, date)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(dates)))
	days := make([]DayStats, 0, len(dates))
	for _, date := range dates {
		agg := byDay[date]
		days = append(days, DayStats{Date: date, Tokens: agg.tokens, Sessions: agg.sessions})
	}

	return &TrendsPayload{
		Days:          days,
		TotalTokens:   totalTokens,
		TotalSessions: totalSessions,
	}, nil
}

// QualitySessions returns sessions mapped to quality analysis payloads,
// optionally filtered by project and date range.
func (p *StoreDataProvider) QualitySessions(ctx context.Context, f QualityFilter) ([]QualitySession, error) {
	// Map QualityFilter to store.SessionFilter.
	sf := store.SessionFilter{}

	if f.DateRange != nil {
		startMs := f.DateRange.Start.UnixMilli()
		sf.StartFrom = &startMs
		endMs := f.DateRange.End.UnixMilli()
		sf.StartBefore = &endMs
	}

	// Note: QualityFilter.Projects maps to project names, but SessionFilter
	// operates on project hashes. For v1 we pass through without project
	// filtering when project names are provided (store-level project name
	// filtering would require a schema extension or post-filter).
	// An empty Projects slice means "all projects" which maps to nil ProjectHash.

	rows, err := p.store.FilteredSessions(ctx, sf)
	if err != nil {
		return nil, fmt.Errorf("store adapter: quality sessions: %w", err)
	}

	// Apply the selection boundary before request-specific filtering.
	var filtered []store.SessionRow
	for i := range rows {
		visible, visibilityErr := p.visibleSessionRow(&rows[i])
		if visibilityErr != nil {
			return nil, fmt.Errorf("store adapter: quality sessions visibility: %w", visibilityErr)
		}
		if visible {
			filtered = append(filtered, rows[i])
		}
	}

	// Post-filter by project name if specified.
	if len(f.Projects) > 0 {
		projectSet := make(map[string]bool, len(f.Projects))
		for _, proj := range f.Projects {
			projectSet[proj] = true
		}
		projectFiltered := make([]store.SessionRow, 0, len(filtered))
		for i := range filtered {
			if projectSet[filtered[i].ProjectName] {
				projectFiltered = append(projectFiltered, filtered[i])
			}
		}
		filtered = projectFiltered
	}

	// One bulk annotation query for the whole snapshot. Querying per session
	// meant one serial SQLite round-trip per recorded session — minutes on a
	// live store with thousands of sessions, re-paid on every annotation
	// mutation via RefreshQuality. Non-fatal if it fails (same semantics as
	// the old per-session lookups: sessions ship without annotations).
	annsBySession, annErr := p.store.GetSessionAnnotationsBulk(ctx)

	result := make([]QualitySession, len(filtered))
	for i := range filtered {
		result[i] = sessionRowToQuality(&filtered[i])
		if annErr == nil {
			if anns := annsBySession[filtered[i].SessionID]; len(anns) > 0 {
				result[i].EffectiveAnnotations = annotationRowsToSummaries(anns)
			}
		}
	}
	return result, nil
}

// AnnotationsForSession returns every non-superseded annotation belonging to a
// session, mapped from store rows to schema summaries. Association targets are
// resolved through the durable local ledger so REST and WebSocket consumers see
// the same complete annotation surface.
func (p *StoreDataProvider) AnnotationsForSession(ctx context.Context, sessionID string) ([]schema.AnnotationSummary, error) {
	rows, err := p.store.GetAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store adapter: annotations for session: %w", err)
	}
	entryRows, err := p.store.GetEntryAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store adapter: entry annotations for session: %w", err)
	}
	associationRows, err := p.store.GetAssociationAnnotationsForSession(ctx, sessionID)
	if err != nil {
		return nil, fmt.Errorf("store adapter: association annotations for session: %w", err)
	}
	rows = append(rows, entryRows...)
	rows = append(rows, associationRows...)
	return annotationRowsToSummaries(rows), nil
}

// ProjectFamiliarity computes familiarity data for a project from the store.
func (p *StoreDataProvider) ProjectFamiliarity(ctx context.Context, projectHash schema.ProjectHash) (*FamiliarityPayload, error) {
	now := time.Now().UTC()

	allSessions, err := p.store.AllSessions(ctx)
	if err != nil {
		return nil, fmt.Errorf("store adapter: project familiarity sessions for %q: %w", projectHash, err)
	}
	allowedSessions := make(map[string]store.SessionRow)
	for i := range allSessions {
		row := &allSessions[i]
		if row.ProjectHash != projectHash.String() {
			continue
		}
		visible, visibilityErr := p.visibleSessionRow(row)
		if visibilityErr != nil {
			return nil, fmt.Errorf(
				"store adapter: project familiarity visibility failed for project %q session %q while filtering stored sessions before aggregation; no familiarity payload was returned because hidden contributions cannot be safely excluded; repair the persisted selection with `peasant kickstart` and retry: %w",
				projectHash, row.SessionID, visibilityErr,
			)
		}
		if visible {
			allowedSessions[row.SessionID] = *row
		}
	}

	rawSessionFiles, err := p.store.GetSessionFilesForProject(ctx, projectHash.String())
	if err != nil {
		return nil, fmt.Errorf("store adapter: project familiarity session files for %q after visibility filtering: %w", projectHash, err)
	}
	visibleSessionFiles := make([]store.SessionFileRow, 0, len(rawSessionFiles))
	for _, row := range rawSessionFiles {
		if _, visible := allowedSessions[row.SessionID]; visible {
			visibleSessionFiles = append(visibleSessionFiles, row)
		}
	}

	files := aggregateVisibleFamiliarity(visibleSessionFiles, allowedSessions, now)
	trails := buildTrailsFromSessionFiles(visibleSessionFiles)
	suggestions := buildReviewSuggestions(files)
	if trails == nil {
		trails = make([]WalkthroughTrail, 0)
	}
	if suggestions == nil {
		suggestions = make([]ReviewSuggestion, 0)
	}

	payload := &FamiliarityPayload{
		ProjectHash:     projectHash,
		FamiliarityPct:  computeFamiliarityPct(files),
		UnexploredCount: computeUnexploredCount(files),
		FreshnessDays:   computeFreshnessDays(files, now),
		Files:           files,
		Trails:          trails,
		Suggestions:     suggestions,
	}

	return payload, nil
}

type visibleFileAggregate struct {
	sessions      map[string]struct{}
	totalTurns    int
	humanTurns    int
	lastEngagedMs int64
	interaction   schema.InteractionType
}

func aggregateVisibleFamiliarity(rows []store.SessionFileRow, sessions map[string]store.SessionRow, now time.Time) []FileFamiliarity {
	byPath := make(map[string]*visibleFileAggregate)
	for _, row := range rows {
		session, ok := sessions[row.SessionID]
		if !ok {
			continue
		}
		aggregate, exists := byPath[row.FilePath]
		if !exists {
			aggregate = &visibleFileAggregate{sessions: make(map[string]struct{}), interaction: schema.InteractionMentioned}
			byPath[row.FilePath] = aggregate
		}
		aggregate.sessions[row.SessionID] = struct{}{}
		aggregate.totalTurns += row.TurnCount
		aggregate.humanTurns += row.HumanTurns
		if session.EndMs > aggregate.lastEngagedMs {
			aggregate.lastEngagedMs = session.EndMs
		}
		candidate := schema.InteractionType(row.Interaction)
		if interactionStrength(candidate) > interactionStrength(aggregate.interaction) {
			aggregate.interaction = candidate
		}
	}

	files := make([]FileFamiliarity, 0, len(byPath))
	for path, aggregate := range byPath {
		lastEngagedValue := time.UnixMilli(aggregate.lastEngagedMs).UTC().Format(time.RFC3339)
		lastEngaged := &lastEngagedValue
		files = append(files, FileFamiliarity{
			Path:          path,
			Depth:         computeFamiliarityDepth(len(aggregate.sessions), aggregate.totalTurns, aggregate.humanTurns, aggregate.interaction),
			SessionCount:  len(aggregate.sessions),
			TotalTurns:    aggregate.totalTurns,
			HumanTurns:    aggregate.humanTurns,
			LastEngagedAt: lastEngaged,
			DaysSince:     daysSince(lastEngaged, now),
			DecayLevel:    computeDecayLevel(lastEngaged, now),
			IsSourceFile:  isSourceFile(path),
		})
	}
	sort.Slice(files, func(i, j int) bool {
		if *files[i].LastEngagedAt != *files[j].LastEngagedAt {
			return *files[i].LastEngagedAt > *files[j].LastEngagedAt
		}
		return files[i].Path < files[j].Path
	})
	return files
}

func interactionStrength(interaction schema.InteractionType) int {
	switch interaction {
	case schema.InteractionQuestioned:
		return 4
	case schema.InteractionDiscussed:
		return 3
	case schema.InteractionRead:
		return 2
	case schema.InteractionMentioned:
		return 1
	default:
		return 0
	}
}

// buildTrailsFromSessionFiles groups session_files by session to build trails.
func buildTrailsFromSessionFiles(rows []store.SessionFileRow) []WalkthroughTrail {
	if len(rows) == 0 {
		return nil
	}

	// Group by session_id.
	type sessionGroup struct {
		files []store.SessionFileRow
	}
	groups := make(map[string]*sessionGroup)
	var sessionOrder []string

	for _, r := range rows {
		g, ok := groups[r.SessionID]
		if !ok {
			g = &sessionGroup{}
			groups[r.SessionID] = g
			sessionOrder = append(sessionOrder, r.SessionID)
		}
		g.files = append(g.files, r)
	}

	var trails []WalkthroughTrail
	for _, sid := range sessionOrder {
		g := groups[sid]
		steps := make([]WalkthroughStep, 0, len(g.files))
		uniqueFiles := make(map[string]bool)
		totalRefs := 0

		for _, f := range g.files {
			steps = append(steps, WalkthroughStep{
				File:    f.FilePath,
				Excerpt: f.Interaction,
			})
			uniqueFiles[f.FilePath] = true
			totalRefs += f.TurnCount
			if totalRefs == 0 {
				totalRefs = 1 // avoid div by zero
			}
		}

		coherent := float64(len(uniqueFiles))/float64(totalRefs) > 0.5

		trails = append(trails, WalkthroughTrail{
			SessionID:  sid,
			TurnCount:  totalRefs,
			Steps:      steps,
			IsCoherent: coherent,
		})

		// Cap trails at 10 most recent sessions.
		if len(trails) >= 10 {
			break
		}
	}
	return trails
}

// sessionRowToSession maps a store.SessionRow to an ingest.Session.
func sessionRowToSession(row *store.SessionRow) ingest.Session {
	s := ingest.Session{
		ID:        schema.SessionID(row.SessionID),
		Project:   displayProjectName(row.CanonicalRemote, row.ProjectName),
		Harness:   defaults.Harness(row.ModelHarness),
		StartTime: time.UnixMilli(row.StartMs),
		EndTime:   time.UnixMilli(row.EndMs),
		Turns:     nil, // populated separately by SessionByID via ListEntries
		Metadata: ingest.SessionMetadata{
			TokensIn:      row.InputTokens,
			TokensOut:     row.OutputTokens,
			TotalTokens:   row.TokensTotal,
			Duration:      time.Duration(row.DurationMinutes * float64(time.Minute)),
			TurnCount:     row.TurnCount,
			ToolCallCount: row.ToolCalls,
		},
	}
	s.GitBranch = ""
	if row.GitBranch != nil {
		s.GitBranch = *row.GitBranch
	}
	if row.CanonicalRemote != nil {
		s.GitRemote = *row.CanonicalRemote
	}
	if q := rowToQualityMetrics(row); q != nil {
		s.Metadata.Quality = q
	}
	return s
}

// rowToQualityMetrics converts quality columns from a SessionRow to a schema.QualityMetrics.
// Returns nil when all quality fields are zero/nil (i.e. not yet computed).
func rowToQualityMetrics(row *store.SessionRow) *schema.QualityMetrics {
	hasQuality := row.TurnCount != 0 || row.TokensTotal != 0 ||
		row.FilesTouched != 0 || row.LinesChanged != 0 ||
		row.RetryLoops != 0 || row.SignalDensity != 0 ||
		row.SpecQualityScore != 0 || row.Outcome != nil || row.Title != nil
	if !hasQuality {
		return nil
	}
	q := &schema.QualityMetrics{
		TurnCount:            &row.TurnCount,
		TotalTokens:          &row.TokensTotal,
		InputTokens:          &row.InputTokens,
		OutputTokens:         &row.OutputTokens,
		ToolCalls:            &row.ToolCalls,
		DurationMinutes:      &row.DurationMinutes,
		FilesTouched:         &row.FilesTouched,
		LinesChanged:         &row.LinesChanged,
		RetryLoops:           &row.RetryLoops,
		RetryTokensWasted:    &row.RetryTokensWasted,
		WithinSessionReverts: &row.WithinSessionReverts,
		SignalDensity:        &row.SignalDensity,
		SpecQualityScore:     &row.SpecQualityScore,
		ExplorationRatio:     &row.ExplorationRatio,
		ScopeBreadth:         &row.ScopeBreadth,
		DiscoveryTurns:       &row.DiscoveryTurns,
	}
	if row.Outcome != nil {
		o := schema.SessionOutcome(*row.Outcome)
		q.Outcome = &o
	}
	if row.Title != nil {
		q.TitleGenerated = row.Title
	}
	return q
}

// sessionRowToQuality maps a store.SessionRow to a QualitySession.
func sessionRowToQuality(row *store.SessionRow) QualitySession {
	qs := QualitySession{
		ID:                   row.SessionID,
		Date:                 time.UnixMilli(row.StartMs).UTC().Format("2006-01-02"),
		Project:              displayProjectName(row.CanonicalRemote, row.ProjectName),
		TotalTokens:          row.TokensTotal,
		InputTokens:          row.InputTokens,
		OutputTokens:         row.OutputTokens,
		TurnCount:            row.TurnCount,
		ToolCalls:            row.ToolCalls,
		DurationMinutes:      row.DurationMinutes,
		FilesTouched:         row.FilesTouched,
		LinesChanged:         row.LinesChanged,
		RetryLoops:           row.RetryLoops,
		RetryTokensWasted:    row.RetryTokensWasted,
		WithinSessionReverts: row.WithinSessionReverts,
		SignalDensity:        row.SignalDensity,
		SpecQualityScore:     row.SpecQualityScore,
		ExplorationRatio:     row.ExplorationRatio,
		ScopeBreadth:         row.ScopeBreadth,
		DiscoveryTurns:       row.DiscoveryTurns,
	}
	if row.Title != nil {
		qs.Title = *row.Title
	}
	if row.Outcome != nil {
		qs.Outcome = *row.Outcome
	}
	if row.Scope != nil {
		qs.Scope = *row.Scope
	}
	return qs
}

// annotationRowsToSummaries maps store.AnnotationRow slices to schema.AnnotationSummary.
func annotationRowsToSummaries(rows []store.AnnotationRow) []schema.AnnotationSummary {
	if len(rows) == 0 {
		return nil
	}
	out := make([]schema.AnnotationSummary, len(rows))
	for i, r := range rows {
		out[i] = schema.AnnotationSummary{
			ID:                  r.ID,
			TargetKind:          r.TargetKind,
			TargetSessionID:     r.TargetSessionID,
			TargetEntryIndex:    r.TargetEntryIndex,
			TargetEntryEndIndex: r.TargetEntryEndIndex,
			TargetAnnotID:       r.TargetAnnotID,
			TargetProjectHash:   r.TargetProjectHash,
			TargetAssociationID: r.TargetAssociationID,
			IsPrimary:           r.IsPrimary,
			AnnotatorKind:       r.AnnotatorKind,
			AnnotatorName:       r.AnnotatorName,
			TypeID:              r.TypeID,
			TypeName:            r.TypeName,
			Value:               r.Value,
			Confidence:          r.Confidence,
			Reason:              r.Reason,
			Provenance:          r.Provenance,
			ContentHash:         r.ContentHash,
			CreatedAt:           r.CreatedAt,
			SupersededBy:        r.SupersededBy,
		}
	}
	return out
}

// EntriesToTurns folds flat session_entries into the rendered Turn model. It
// forwards to the canonical implementation in internal/transcript so the local
// dashboard, export, and push share one conversion path. Retained as an api-level
// name for existing callers (tui, export, tests).
func EntriesToTurns(entries []schema.SessionEntry) []ingest.Turn {
	return transcript.EntriesToTurns(entries)
}

// SessionToDetail builds a SessionDetailPayload from a full Session. It forwards
// to internal/transcript (single conversion path).
func SessionToDetail(s *ingest.Session) *SessionDetailPayload {
	return transcript.SessionToDetail(s)
}
