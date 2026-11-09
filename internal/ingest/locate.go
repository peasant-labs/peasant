package ingest

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
)

// scopedLocate is what one session-scoped LOCATE pass produced: the cohort
// report, the located sources DIFF may classify, the duplicate refusals a full
// run would have failed, and the side effects the adapters deferred. LOCATE
// itself writes nothing; the caller applies the deferred writes only after every
// selected id was located and no miss remains.
type scopedLocate struct {
	report   ScopeReport
	sessions []DiscoveredSession
	refusals []SessionResult
	deferred []DeferredWrites
}

// applyDeferredWrites runs the adapter side effects LOCATE collected. It is
// called after the whole cohort is located, so a refused run writes nothing.
func (p *Pipeline) applyDeferredWrites(ctx context.Context, located *scopedLocate) error {
	if located == nil {
		return nil
	}
	for _, deferred := range located.deferred {
		if deferred == nil {
			continue
		}
		if err := deferred.Apply(ctx); err != nil {
			return fmt.Errorf("pipeline locate: apply deferred writes: %w", err)
		}
	}
	return nil
}

// locateScopedRun replaces discovery for a session-scoped run. A full run
// returns the nil located value and keeps the ordinary DISCOVER stage.
//
// LOCATE reads and never writes. It runs before the harvest-start sweep, before
// the deferred writes, and before the audit row, so an unusable selection is
// refused before anything is recorded.
func (p *Pipeline) locateScopedRun(ctx context.Context) (*scopedLocate, error) {
	p.scopeReport = nil
	if p.config.Scope == nil {
		return nil, nil
	}
	locator := newScopeLocator(p)
	located, err := locator.run(ctx)
	if err != nil {
		return nil, err
	}
	// The report's upkeep set is both the in-memory gate and the SQL scope, set
	// together so the two narrowings can never disagree.
	p.config.AllowedSessionIDs = make(map[SessionID]bool, len(located.report.Members))
	for _, id := range located.report.Upkeep() {
		p.config.AllowedSessionIDs[id] = true
	}
	p.scopeReport = &located.report
	return located, nil
}

// scopeLocator is the state one LOCATE pass carries while it builds the cohort.
type scopeLocator struct {
	p     *Pipeline
	scope *SessionScope

	// store answers the two stored-relative reads; nil when no store is
	// attached, and derived answers the indexed duplicate lookups.
	store   SessionSourceReader
	derived DerivedLookupReader
	// attached says whether this run had a store, for the miss message.
	attached StoreAttachment

	// stored and children hold the two batched reads.
	stored   map[SessionID]StoredSessionSource
	children map[SessionID][]StoredSessionSource

	// adapters caches one adapter per harness, with the same store-backed
	// capabilities discovery attaches, so the scoped build links exactly as the
	// full walk does.
	adapters map[Harness]SourceAdapter

	// members holds at most one member per id. A later, higher-precedence role
	// replaces an earlier one, so a session that is both selected and related is
	// processed as selected.
	members map[SessionID]LocatedMember

	// claims are the located sources the duplicate refusal inspects. Claims
	// that exist only for the duplicate check (a second root copy, a catalogued
	// rollout) are carried separately.
	claims      []DiscoveredSession
	extraClaims []DiscoveredSession

	misses   []ScopeMiss
	refusals []SessionResult
	deferred []DeferredWrites
}

func newScopeLocator(p *Pipeline) *scopeLocator {
	return &scopeLocator{
		p:        p,
		scope:    p.config.Scope,
		members:  make(map[SessionID]LocatedMember),
		adapters: make(map[Harness]SourceAdapter),
	}
}

func (l *scopeLocator) run(ctx context.Context) (*scopedLocate, error) {
	if err := l.readStored(ctx); err != nil {
		return nil, err
	}
	for _, id := range l.scope.IDs() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := l.locateSelected(ctx, id); err != nil {
			return nil, err
		}
	}
	if err := l.joinUnreadableChildren(ctx); err != nil {
		return nil, err
	}
	if l.scope.Related() == RelatedInclude {
		if err := l.walkAncestors(ctx); err != nil {
			return nil, err
		}
	}
	if err := l.refuseDuplicates(ctx); err != nil {
		return nil, err
	}
	if len(l.misses) > 0 {
		return nil, l.scopeError()
	}

	report := l.buildReport()
	located := &scopedLocate{
		report:   report,
		sessions: l.processedSources(report),
		refusals: l.refusals,
		deferred: l.deferred,
	}
	return located, nil
}

// scopeError builds the one miss error a run returns. Every unusable selected
// id is reported in one run, in id order, so a run never stops at the first.
func (l *scopeLocator) scopeError() error {
	return &SessionScopeError{
		Misses:  l.misses,
		Command: l.p.config.Command,
		Prefix:  l.p.config.CommandPrefix,
		Store:   l.attached,
	}
}

// readStored performs the two batched stored-relative reads. Both are skipped
// when no store is attached; a store that cannot answer either is a plain run
// error, never a miss.
func (l *scopeLocator) readStored(ctx context.Context) error {
	if l.p.store == nil {
		l.attached = StoreNotAttached
		return nil
	}
	l.attached = StoreAttached
	reader, ok := l.p.store.(SessionSourceReader)
	if !ok {
		return fmt.Errorf(
			"ingest: the attached store cannot answer StoredSessionSources; a session-scoped run must know which of its named ids are already recorded, and a store that cannot say would report a stored id as a miss; where: ingest.Pipeline.locateScopedRun in internal/ingest/locate.go; when: before the first session was located; impact: no source was read and no session or store row was touched; fix: attach a store that implements SessionSourceReader, or run without a store for a logs-only run")
	}
	l.store = reader
	if derived, ok := l.p.store.(DerivedLookupReader); ok {
		l.derived = derived
	}

	ids := l.scope.IDs()
	stored, err := reader.StoredSessionSources(ctx, ids)
	if err != nil {
		return fmt.Errorf("pipeline locate stored session sources: %w", err)
	}
	children, err := reader.StoredChildren(ctx, ids)
	if err != nil {
		return fmt.Errorf("pipeline locate stored children: %w", err)
	}
	l.stored = stored
	l.children = children
	return nil
}

// adapterFor builds the adapter for one harness with the store-backed
// capabilities discovery attaches. A harness with no registered adapter is not
// enabled in this build.
func (l *scopeLocator) adapterFor(harness Harness) (SourceAdapter, bool) {
	if adapter, ok := l.adapters[harness]; ok {
		return adapter, true
	}
	factory, ok := l.p.adapters[harness]
	if !ok {
		return nil, false
	}
	adapter := factory(l.p.fs, l.p.git, l.p.salt)
	if cache, ok := l.p.store.(ClaudeEvidenceCache); ok && !l.p.config.DryRun {
		AttachClaudeEvidenceCache(adapter, cache)
	}
	if l.p.store != nil {
		AttachSessionLocationLookup(adapter, l.p.store)
		if parentReader, ok := l.p.store.(LogicalParentReader); ok {
			AttachLogicalParentLookup(adapter, parentReader)
		}
	}
	l.adapters[harness] = adapter
	return adapter, true
}

// locatorFor reports the session locator of an enabled harness.
func (l *scopeLocator) locatorFor(harness Harness) (SessionLocator, bool) {
	adapter, ok := l.adapterFor(harness)
	if !ok {
		return nil, false
	}
	locator, ok := adapter.(SessionLocator)
	return locator, ok
}

// enabledConfig reports whether a harness is enabled and returns its source
// configuration.
func (l *scopeLocator) enabledConfig(harness Harness) (SourceConfig, bool) {
	cfg, ok := l.p.config.Sources[harness]
	if !ok || !cfg.Enabled {
		return SourceConfig{}, false
	}
	return cfg, true
}

// locateSelected resolves one id the user named. A stored id re-reads its
// recorded source path first; only an unstored id, or a stored one whose source
// no longer holds it, runs the by-name lookup.
func (l *scopeLocator) locateSelected(ctx context.Context, id SessionID) error {
	row, stored := l.stored[id]
	if !stored {
		return l.locateByName(ctx, id, nil)
	}
	if row.Harness == "" {
		l.addMember(LocatedMember{
			ID: id, Harness: row.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
			Unreadable: UnreadableSourceFailed,
			Detail:     "the store records no harness for this session, so its recorded source cannot be attributed to an adapter",
		})
		return nil
	}
	if _, enabled := l.enabledConfig(row.Harness); !enabled {
		l.addMember(LocatedMember{
			ID: id, Harness: row.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
			Unreadable: UnreadableHarnessDisabled,
		})
		return nil
	}
	if l.p.config.Harness != nil && *l.p.config.Harness != row.Harness {
		l.misses = append(l.misses, ScopeMiss{ID: id, Reason: MissHarnessFiltered, StoredHarness: row.Harness})
		return nil
	}

	cfg, _ := l.enabledConfig(row.Harness)
	locator, ok := l.locatorFor(row.Harness)
	if !ok {
		l.addMember(LocatedMember{
			ID: id, Harness: row.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
			Unreadable: UnreadableSourceFailed,
			Detail:     "the " + string(row.Harness) + " adapter cannot locate one session by name",
		})
		return nil
	}

	build, err := locator.DiscoverSessionAt(ctx, cfg, LocatedSource{ID: id, Path: row.Path, Stored: l.relatives(id, row)})
	if err != nil {
		return l.handleStoredBuildError(ctx, id, row, err)
	}
	l.deferred = append(l.deferred, build.Deferred)

	root, found := memberForID(build.Members, id)
	if !found || root.Source == nil {
		// A source that reads cleanly but yields no member is gone for this
		// purpose: a full run stores nothing for it either.
		l.addMember(LocatedMember{
			ID: id, Harness: row.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
			Unreadable: UnreadableSourceGone,
		})
		return nil
	}
	if !l.withinRunAgeOf(*root.Source) {
		l.misses = append(l.misses, ScopeMiss{ID: id, Reason: MissSinceFiltered})
		return nil
	}
	l.adopt(id, row, build.Members, LocateStoredPath, CohortSelected)
	return nil
}

// handleStoredBuildError turns the build error of a stored selected id into the
// next step: a gone source retries by name, a read failure is unreadable, and a
// cancellation ends the run.
func (l *scopeLocator) handleStoredBuildError(ctx context.Context, id SessionID, row StoredSessionSource, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, ErrSourceGone) {
		return l.locateByName(ctx, id, &row)
	}
	l.addMember(LocatedMember{
		ID: id, Harness: row.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
		Unreadable: UnreadableSourceFailed,
		Detail:     firstLineOr(err.Error(), "the recorded source could not be read"),
	})
	return nil
}

// relatives returns what the store records about a located source's family.
func (l *scopeLocator) relatives(id SessionID, row StoredSessionSource) StoredRelatives {
	return StoredRelatives{Parent: row.Parent, Children: l.children[id]}
}

// locateByName asks every enabled harness for an id the user named, in harness
// order, and builds every hit. It never stops at the first hit, so the searched
// record covers every source the id could be in.
//
// storedRow is non-nil when a stored id reached this step through ErrSourceGone:
// if nothing is found, that stored row becomes an unreadable member rather than
// a miss, because the store's copy is still the session's record.
func (l *scopeLocator) locateByName(ctx context.Context, id SessionID, storedRow *StoredSessionSource) error {
	var searched []SearchedSource
	var built []LocatedMember
	seenPaths := make(map[ResolvedPath]struct{})
	found := false

	for _, harness := range l.harnessOrder() {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if l.p.config.Harness != nil && *l.p.config.Harness != harness {
			continue
		}
		cfg, enabled := l.enabledConfig(harness)
		if !enabled {
			searched = append(searched, SearchedSource{Harness: harness, Method: SearchHarnessDisabled})
			continue
		}
		locator, ok := l.locatorFor(harness)
		if !ok {
			// A harness whose adapter carries no locator cannot search by id.
			// Every production adapter implements SessionLocator, so this only
			// guards a partial registry.
			continue
		}
		lookup, err := locator.LookupSessionSources(ctx, cfg, id)
		searched = append(searched, lookup.Searched...)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			searched = append(searched, SearchedSource{
				Harness:  harness,
				Method:   SearchFailed,
				Location: l.failureLocation(harness, cfg, lookup),
				Detail:   firstLineOr(err.Error(), "the lookup failed"),
			})
			continue
		}
		for _, path := range lookup.Sources {
			if _, duplicate := seenPaths[path]; duplicate {
				continue
			}
			seenPaths[path] = struct{}{}
			located := LocatedSource{ID: id, Path: path}
			if storedRow != nil {
				located.Stored = l.relatives(id, *storedRow)
			}
			build, buildErr := locator.DiscoverSessionAt(ctx, cfg, located)
			if buildErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				if errors.Is(buildErr, ErrSourceGone) {
					continue
				}
				searched = append(searched, SearchedSource{
					Harness:  harness,
					Method:   SearchFailed,
					Location: string(path),
					Detail:   firstLineOr(buildErr.Error(), "the located source could not be built"),
				})
				continue
			}
			root, ok := memberForID(build.Members, id)
			if !ok || root.Source == nil {
				continue
			}
			l.deferred = append(l.deferred, build.Deferred)
			built = append(built, build.Members...)
			found = true
		}
	}

	if !found {
		if storedRow != nil {
			l.addMember(LocatedMember{
				ID: id, Harness: storedRow.Harness, Outcome: LocateStoredUnreadable, Role: CohortSelected,
				Unreadable: UnreadableSourceGone,
			})
			return nil
		}
		l.misses = append(l.misses, ScopeMiss{ID: id, Reason: MissNotFound, Searched: searched})
		return nil
	}

	root, _ := memberForID(built, id)
	if root.Source != nil && !l.withinRunAgeOf(*root.Source) {
		l.misses = append(l.misses, ScopeMiss{ID: id, Reason: MissSinceFiltered})
		return nil
	}
	storedRelatives := StoredSessionSource{}
	if storedRow != nil {
		storedRelatives = *storedRow
	}
	l.adopt(id, storedRelatives, built, LocateDirectLookup, CohortSelected)
	return nil
}

// failureLocation names the source a failed lookup could not read. The adapter's
// returned entries describe the roots it completed; when none names the failure,
// the configured location is used and the message says so.
func (l *scopeLocator) failureLocation(harness Harness, cfg SourceConfig, lookup SourceLookup) string {
	for index := len(lookup.Searched) - 1; index >= 0; index-- {
		if lookup.Searched[index].Harness == harness && lookup.Searched[index].Location != "" {
			return lookup.Searched[index].Location
		}
	}
	if len(cfg.Paths) > 0 {
		return displaySourcePath(cfg.Paths[0])
	}
	return ""
}

// displaySourcePath renders a configured source path with the home directory
// shortened, the way the searched block shows every location.
func displaySourcePath(path ResolvedPath) string {
	raw := string(path)
	if home, err := os.UserHomeDir(); err == nil && home != "" && strings.HasPrefix(raw, home+string(filepath.Separator)) {
		return "~" + strings.TrimPrefix(raw, home)
	}
	return raw
}

// harnessOrder lists the registered adapters in the canonical harness order, so
// the searches are reported deterministically.
func (l *scopeLocator) harnessOrder() []Harness {
	order := make([]Harness, 0, len(l.p.adapters))
	seen := make(map[Harness]struct{}, len(l.p.adapters))
	for _, harness := range AllHarnesses {
		if _, ok := l.p.adapters[harness]; !ok {
			continue
		}
		seen[harness] = struct{}{}
		order = append(order, harness)
	}
	var extra []Harness
	for harness := range l.p.adapters {
		if _, ok := seen[harness]; !ok {
			extra = append(extra, harness)
		}
	}
	slices.SortFunc(extra, func(a, b Harness) int { return strings.Compare(string(a), string(b)) })
	return append(order, extra...)
}

// withinRunAgeOf applies the run's own --since to a found source, the way the
// FILTER stage does.
func (l *scopeLocator) withinRunAgeOf(source DiscoveredSession) bool {
	return l.p.withinRunAge(discoveredSessionStart(source))
}

// memberForID returns the member that carries the located source for id.
func memberForID(members []LocatedMember, id SessionID) (LocatedMember, bool) {
	for _, member := range members {
		if member.ID == id {
			return member, true
		}
	}
	return LocatedMember{}, false
}

// adopt assigns each built member its role and outcome and records the claim.
// rootID is the id this build was rooted at, rootRole is the role that root
// takes (selected for a named id, child or context for a stored child), and
// stored carries the store's family facts for that root (zero for a by-name
// build).
func (l *scopeLocator) adopt(rootID SessionID, stored StoredSessionSource, members []LocatedMember, rootOutcome LocateOutcome, rootRole CohortRole) {
	storedChildren := make(map[SessionID]struct{}, len(l.children[rootID]))
	for _, child := range l.children[rootID] {
		storedChildren[child.ID] = struct{}{}
	}
	for _, member := range members {
		switch {
		case member.ID == rootID:
			member.Role = rootRole
		case stored.Parent != nil && member.ID == *stored.Parent:
			member.Role = CohortContext
		case l.scope.Related() == RelatedExclude:
			member.Role = CohortContext
		default:
			member.Role = CohortChild
		}

		if member.Source == nil {
			if member.Role == CohortContext {
				continue
			}
			member.Outcome = LocateStoredUnreadable
			if !member.Unreadable.IsValid() {
				member.Unreadable = UnreadableSourceFailed
				member.Detail = "the adapter returned no source for this stored session and no reason"
			}
		} else {
			_, isStoredChild := storedChildren[member.ID]
			switch {
			case member.ID == rootID:
				member.Outcome = rootOutcome
			case member.Role == CohortContext:
				member.Outcome = LocateStoredPath
			case isStoredChild:
				member.Outcome = LocateStoredPath
			default:
				member.Outcome = LocateDirectLookup
			}
		}

		l.addMember(member)
	}
}

// rolePrecedence orders the cohort roles so a selected member can replace a
// related record of the same id.
func rolePrecedence(role CohortRole) int {
	switch role {
	case CohortSelected:
		return 4
	case CohortChild:
		return 3
	case CohortAncestor:
		return 2
	case CohortContext:
		return 1
	default:
		return 0
	}
}

// addMember records one member, dropping a related member the run's --since
// excludes and letting a higher-precedence role replace a lower one.
func (l *scopeLocator) addMember(member LocatedMember) {
	if !member.Role.IsValid() || !member.Outcome.IsValid() {
		return
	}
	if member.Role != CohortSelected && member.Role != CohortContext && member.Source != nil {
		// A related member the run's own --since excludes is dropped.
		if !l.withinRunAgeOf(*member.Source) {
			return
		}
	}
	if member.Role == CohortContext && member.Source == nil {
		return
	}
	if existing, ok := l.members[member.ID]; ok && rolePrecedence(existing.Role) >= rolePrecedence(member.Role) {
		return
	}
	l.members[member.ID] = member
}

// joinUnreadableChildren re-reads the stored children of every related member
// that could not be read, so a changed child of an unreadable parent still
// updates as it does in a full run. One extra StoredChildren read covers every
// such parent.
func (l *scopeLocator) joinUnreadableChildren(ctx context.Context) error {
	if l.store == nil || l.scope.Related() != RelatedInclude {
		return nil
	}
	var parents []SessionID
	for id, member := range l.members {
		if member.Outcome != LocateStoredUnreadable || member.Role == CohortContext || member.Role == CohortAncestor {
			continue
		}
		parents = append(parents, id)
	}
	if len(parents) == 0 {
		return nil
	}
	// The first read covered the selected ids' children. An unreadable related
	// member was not one of those parents, so its children need one more batch.
	// Asking for every unreadable parent keeps it to one extra read.
	children, err := l.store.StoredChildren(ctx, parents)
	if err != nil {
		return fmt.Errorf("pipeline locate stored children: %w", err)
	}
	for id, rows := range children {
		if len(rows) > 0 {
			l.children[id] = rows
		}
	}
	sortSessionIDs(parents)
	for _, id := range parents {
		if err := l.joinChildrenOf(ctx, id); err != nil {
			return err
		}
	}
	return nil
}

// joinChildrenOf locates every stored child of one unreadable parent.
func (l *scopeLocator) joinChildrenOf(ctx context.Context, parentID SessionID) error {
	for _, child := range l.children[parentID] {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, exists := l.members[child.ID]; exists {
			continue
		}
		if err := l.locateStoredChild(ctx, parentID, child); err != nil {
			return err
		}
	}
	return nil
}

// locateStoredChild re-reads one stored child of an unreadable parent.
func (l *scopeLocator) locateStoredChild(ctx context.Context, parentID SessionID, child StoredSessionSource) error {
	role := CohortChild
	if l.scope.Related() == RelatedExclude {
		role = CohortContext
	}
	if _, enabled := l.enabledConfig(child.Harness); !enabled {
		l.addMember(LocatedMember{ID: child.ID, Harness: child.Harness, Outcome: LocateStoredUnreadable, Role: role, Unreadable: UnreadableHarnessDisabled})
		return nil
	}
	cfg, _ := l.enabledConfig(child.Harness)
	locator, ok := l.locatorFor(child.Harness)
	if !ok {
		l.addMember(LocatedMember{ID: child.ID, Harness: child.Harness, Outcome: LocateStoredUnreadable, Role: role, Unreadable: UnreadableSourceFailed, Detail: "the " + string(child.Harness) + " adapter cannot locate one session by name"})
		return nil
	}
	parent := parentID
	build, err := locator.DiscoverSessionAt(ctx, cfg, LocatedSource{ID: child.ID, Path: child.Path, Stored: StoredRelatives{Parent: &parent, Children: l.children[child.ID]}})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if errors.Is(err, ErrSourceGone) {
			l.addMember(LocatedMember{ID: child.ID, Harness: child.Harness, Outcome: LocateStoredUnreadable, Role: role, Unreadable: UnreadableSourceGone})
			return nil
		}
		l.addMember(LocatedMember{ID: child.ID, Harness: child.Harness, Outcome: LocateStoredUnreadable, Role: role, Unreadable: UnreadableSourceFailed, Detail: firstLineOr(err.Error(), "the recorded source could not be read")})
		return nil
	}
	l.deferred = append(l.deferred, build.Deferred)
	root, found := memberForID(build.Members, child.ID)
	if !found || root.Source == nil {
		l.addMember(LocatedMember{ID: child.ID, Harness: child.Harness, Outcome: LocateStoredUnreadable, Role: role, Unreadable: UnreadableSourceGone})
		return nil
	}
	stored := StoredSessionSource{ID: child.ID, Harness: child.Harness, Parent: &parent}
	l.adopt(child.ID, stored, build.Members, LocateStoredPath, role)
	return nil
}

// walkAncestors follows each member's declared ancestor probe until the walk
// reaches a stored ancestor, a root, a nil probe, a miss, or an id already in
// the cohort.
func (l *scopeLocator) walkAncestors(ctx context.Context) error {
	startIDs := make([]SessionID, 0, len(l.members))
	for id, member := range l.members {
		if member.Role != CohortSelected && member.Role != CohortChild {
			continue
		}
		if member.ParentProbe == nil || member.Source == nil {
			continue
		}
		startIDs = append(startIDs, id)
	}
	sortSessionIDs(startIDs)
	for _, id := range startIDs {
		current := l.members[id]
		for {
			if err := ctx.Err(); err != nil {
				return err
			}
			probe := current.ParentProbe
			if probe == nil {
				break
			}
			if !probe.Kind.IsValid() {
				return fmt.Errorf(
					"ingest: the ancestor probe for session %s declares probe kind %d, which this build cannot execute; the core runs exactly the declared kind of a harness probe so the walk cannot list a directory that kind does not allow; where: ingest.Pipeline.locateScopedRun in internal/ingest/locate.go; when: walking an unstored ancestor; impact: the ancestor was not looked for and no directory was listed; fix: declare ProbePath, ProbeRow, or ProbeCatalogWindow in the adapter that built the probe",
					current.ID, probe.Kind)
			}
			path, ancestorID, found, err := l.executeProbe(ctx, current, probe)
			if err != nil {
				return err
			}
			if !found {
				break
			}
			if _, exists := l.members[ancestorID]; exists {
				break
			}
			cfg, enabled := l.enabledConfig(probe.Harness)
			if !enabled {
				break
			}
			locator, ok := l.locatorFor(probe.Harness)
			if !ok {
				break
			}
			build, buildErr := locator.DiscoverSessionAt(ctx, cfg, LocatedSource{ID: ancestorID, Path: path})
			if buildErr != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				break
			}
			root, ok := memberForID(build.Members, ancestorID)
			if !ok || root.Source == nil {
				break
			}
			l.deferred = append(l.deferred, build.Deferred)
			// Only the ancestor joins the cohort: its own children are siblings
			// of the child that named it, and siblings stay out of a scoped run.
			root.Role = CohortAncestor
			root.Outcome = LocateDirectLookup
			l.addMember(root)
			current = root
		}
	}
	return nil
}

// executeProbe runs exactly the declared probe kind and returns the path it
// found, if any.
func (l *scopeLocator) executeProbe(ctx context.Context, current LocatedMember, probe *AncestorProbe) (ResolvedPath, SessionID, bool, error) {
	switch probe.Kind {
	case ProbePath:
		info, err := l.p.fs.Stat(string(probe.Path))
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", false, ctxErr
			}
			// A path that cannot be observed is a miss for the walk, exactly
			// as a path that is not there is.
			return "", "", false, nil
		}
		if info.IsDir() {
			return "", "", false, nil
		}
		id, err := NewSessionID(strings.TrimSuffix(filepath.Base(string(probe.Path)), defaults.ExtJSONL.String()))
		if err != nil {
			return "", "", false, nil
		}
		return probe.Path, id, true, nil
	case ProbeRow:
		if probe.ID == "" {
			return "", "", false, nil
		}
		cfg, enabled := l.enabledConfig(probe.Harness)
		if !enabled {
			return "", "", false, nil
		}
		locator, ok := l.locatorFor(probe.Harness)
		if !ok {
			return "", "", false, nil
		}
		lookup, err := locator.LookupSessionSources(ctx, cfg, probe.ID)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", false, ctxErr
			}
			return "", "", false, nil
		}
		if len(lookup.Sources) == 0 {
			return "", "", false, nil
		}
		return lookup.Sources[0], probe.ID, true, nil
	case ProbeCatalogWindow:
		if probe.ID == "" || current.Source == nil {
			return "", "", false, nil
		}
		// The declared window is one catalog row, then the child's own date
		// folder and the previous day: at most two listings, never a walk.
		if l.derived != nil {
			if _, err := l.derived.CountCodexRolloutsBySession(ctx, probe.ID); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return "", "", false, ctxErr
				}
				return "", "", false, nil
			}
		}
		paths, err := l.codexWindowPaths(ctx, probe.ID, current.Source.SourcePath)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return "", "", false, ctxErr
			}
			return "", "", false, nil
		}
		if len(paths) == 0 {
			return "", "", false, nil
		}
		return paths[0], probe.ID, true, nil
	default:
		return "", "", false, nil
	}
}

// codexWindowPaths lists the date folder of the given rollout and the day before
// it, returning every rollout file whose name carries id. It never lists a third
// folder and never walks the tree.
func (l *scopeLocator) codexWindowPaths(ctx context.Context, id SessionID, sourcePath ResolvedPath) ([]ResolvedPath, error) {
	folders, ok := codexFolderWindow(sourcePath)
	if !ok {
		return nil, nil
	}
	var paths []ResolvedPath
	for _, folder := range folders {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entries, err := l.p.fs.ReadDir(string(folder))
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			nameID, ok := codexSessionIDFromFilename(entry.Name())
			if !ok || nameID != string(id) {
				continue
			}
			paths = append(paths, ResolvedPath(filepath.Join(string(folder), entry.Name())))
		}
	}
	slices.Sort(paths)
	return paths, nil
}

// codexFolderWindow derives the two date folders a Codex rollout beside path can
// be in. A path outside the YYYY/MM/DD layout has no window.
func codexFolderWindow(path ResolvedPath) ([2]ResolvedPath, bool) {
	dayDir := filepath.Dir(string(path))
	day := filepath.Base(dayDir)
	month := filepath.Base(filepath.Dir(dayDir))
	year := filepath.Base(filepath.Dir(filepath.Dir(dayDir)))
	if len(year) != 4 || len(month) != 2 || len(day) != 2 {
		return [2]ResolvedPath{}, false
	}
	parsed, err := time.ParseInLocation("2006/01/02", year+"/"+month+"/"+day, time.Local)
	if err != nil {
		return [2]ResolvedPath{}, false
	}
	root := ResolvedPath(filepath.Dir(filepath.Dir(filepath.Dir(dayDir))))
	return [2]ResolvedPath{
		codexDayFolder(root, parsed),
		codexDayFolder(root, parsed.AddDate(0, 0, -1)),
	}, true
}

// buildReport orders the members selected first, then children, ancestors, and
// context, and keeps one member per id.
func (l *scopeLocator) buildReport() ScopeReport {
	byRole := map[CohortRole][]SessionID{}
	for id, member := range l.members {
		byRole[member.Role] = append(byRole[member.Role], id)
	}
	for _, ids := range byRole {
		sortSessionIDs(ids)
	}
	order := []CohortRole{CohortSelected, CohortChild, CohortAncestor, CohortContext}
	members := make([]LocatedMember, 0, len(l.members))
	for _, role := range order {
		for _, id := range byRole[role] {
			members = append(members, l.members[id])
		}
	}
	return ScopeReport{Members: members}
}

// processedSources returns the sources DIFF through INDEX may work on, in report
// order and one per id.
func (l *scopeLocator) processedSources(report ScopeReport) []DiscoveredSession {
	sessions := make([]DiscoveredSession, 0, len(report.Members))
	seen := make(map[SessionID]struct{}, len(report.Members))
	for _, member := range report.Members {
		if member.Role == CohortContext || !member.Outcome.located() || member.Source == nil {
			continue
		}
		if _, duplicate := seen[member.ID]; duplicate {
			continue
		}
		seen[member.ID] = struct{}{}
		sessions = append(sessions, *member.Source)
	}
	return sessions
}

// sortSessionIDs orders ids the way a report reads them.
func sortSessionIDs(ids []SessionID) {
	slices.SortFunc(ids, func(a, b SessionID) int { return strings.Compare(string(a), string(b)) })
}
