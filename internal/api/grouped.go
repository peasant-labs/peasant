package api

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/peasant-labs/schema"
)

// GroupedRouteVariant identifies the mounted route that produced a grouped
// list. The member scope records it so a member fetch replays the exact
// originating predicate instead of guessing from the group key.
type GroupedRouteVariant string

const (
	// GroupedRouteSessions is the local session list route (view=grouped).
	GroupedRouteSessions GroupedRouteVariant = "sessions"
	// GroupedRouteSearch is the local full-text search route (view=grouped).
	GroupedRouteSearch GroupedRouteVariant = "search"
	// GroupedRouteSync is the local /share chooser sync route (view=grouped).
	// Its predicate and row callback are registered by the sync route owner;
	// this package owns the member route and cache they replay through.
	GroupedRouteSync GroupedRouteVariant = "sync"
)

// AllGroupedRouteVariants is the closed set of originating routes.
var AllGroupedRouteVariants = []GroupedRouteVariant{GroupedRouteSessions, GroupedRouteSearch, GroupedRouteSync}

// IsValid reports whether v is a registered originating route.
func (v GroupedRouteVariant) IsValid() bool {
	for _, candidate := range AllGroupedRouteVariants {
		if v == candidate {
			return true
		}
	}
	return false
}

// GroupedFilters is the canonical parsed existing-filter set of an originating
// grouped route. It is what the member scope records and replays, so it carries
// ONLY values the route already declares; unknown member query parameters are
// refused at the trust boundary rather than folded in here.
type GroupedFilters struct {
	Variant GroupedRouteVariant
	// SearchQuery and SearchLimit are the parsed values of the search route's
	// existing q and limit parameters. They are empty on every other variant.
	SearchQuery string
	SearchLimit int
	// ProjectHash is the parsed value of the sessions route's project
	// parameter: the opaque project identity a caller scoped the grouped list
	// to. It is empty when the caller asked for every project, which keeps the
	// legacy cross-project grouped list. The member scope records it, so a
	// member fetch replays exactly the project predicate the list used and
	// never widens to a sibling project.
	ProjectHash schema.ProjectHash
}

// GroupedCandidate is one route-predicate-applied session plus the durable
// evidence the grouped view needs. The route owner's Gather callback produces
// candidates ONLY after applying the existing route predicate, so grouping
// never widens or replaces it.
type GroupedCandidate struct {
	// Summary is the ordinary session row projection for the transcript item.
	Summary schema.SessionSummary
	// StartMs orders the item. It is the stored session start, never a page or
	// query artifact.
	StartMs int64
	// StableID is the stable local session identifier used as the deterministic
	// tie-break after start time.
	StableID string
	// HostSlug and Harness form the local owner-identity domain of the group key.
	HostSlug string
	Harness  string
	// Purpose is the durable session purpose. helper_review candidates become
	// members of an owner group; every other value stays an ordinary item.
	Purpose schema.SessionPurpose
	// OwnerState and OwnerTargetLocalID are the immediate started_by evidence.
	// An empty state means no started_by evidence; a known owner with a missing
	// stored row still names OwnerTargetLocalID and keeps a stable group key.
	OwnerState         schema.RelationshipTargetState
	OwnerTargetLocalID string
	// Matches is the originating search evidence for this exact session. It is
	// empty on the session-list route and preserved on the search route.
	Matches []schema.SearchResult
	// Sync is the route-specific sync summary. The local sessions and search
	// routes leave it nil; the sync route owner sets it.
	Sync *schema.LocalSyncSummary
}

// GroupedVariantSource is the fixed seam a route owner registers so the shared
// member route and cache can replay its predicate. Gather MUST apply the
// originating route predicate with CURRENT authorization and selection before
// returning, and MUST NOT widen beyond the recorded filters.
type GroupedVariantSource struct {
	Variant GroupedRouteVariant
	Gather  func(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error)
}

// groupedCandidateProvider is implemented by the production store provider. It
// supplies the session-list and search candidate sets plus the selection
// revision the member scope compares against.
type groupedCandidateProvider interface {
	GroupedCandidates(ctx context.Context, filters GroupedFilters) ([]GroupedCandidate, error)
	GroupedScopeRevision() string
}

// errGroupedScopeNotMatched reports that a member scope no longer matches the
// replayed candidate set. The handler maps it to an actionable 409 so the
// caller refreshes the originating list instead of receiving a broadened or
// silently empty page.
var errGroupedScopeNotMatched = errors.New("grouped member scope no longer matches the originating list")

// errGroupedGroupUnknown reports that the requested group key names no group in
// the replayed candidate set.
var errGroupedGroupUnknown = errors.New("grouped member scope names no matching helper group")

// helperGroup is one resolved owner-and-purpose group.
type helperGroup struct {
	groupID     string
	ownerID     string // empty for an unresolved (independent) group
	resolved    bool
	ownerStatus schema.RelationshipNavigationStatus
	members     []GroupedCandidate
}

// knownOwnerState reports whether the immediate started_by evidence names a
// specific owner session, including one that is retained but not stored.
func knownOwnerState(state schema.RelationshipTargetState) bool {
	return state == schema.RelationshipTargetKnown || state == schema.RelationshipTargetKnownRetained
}

// ownerStatusFor maps durable owner evidence to the read-only availability the
// context container carries. It never guesses a target identifier or title.
func ownerStatusFor(state schema.RelationshipTargetState) schema.RelationshipNavigationStatus {
	switch state {
	case schema.RelationshipTargetKnown, schema.RelationshipTargetKnownRetained:
		return schema.RelationshipNavigationKnownUnavailable
	case schema.RelationshipTargetConflictingCurrentNativeEvidence:
		return schema.RelationshipNavigationConflicting
	case schema.RelationshipTargetUnknown, schema.RelationshipTargetExplicitNone, "":
		return schema.RelationshipNavigationUnknown
	default:
		return schema.RelationshipNavigationUnknown
	}
}

// helperGroupFor derives one candidate's stable group identity. Known owners key
// on the installation domain plus the immediate started_by target; unknown,
// conflicting and ownerless helpers key on their own stable identity so two
// independent helpers never coalesce.
func helperGroupFor(candidate GroupedCandidate) helperGroup {
	purpose := "helper_review"
	if knownOwnerState(candidate.OwnerState) && candidate.OwnerTargetLocalID != "" {
		domain := candidate.HostSlug + "\x00" + candidate.Harness
		return helperGroup{
			groupID:     groupedGroupID("1", domain, candidate.OwnerTargetLocalID, purpose),
			ownerID:     candidate.OwnerTargetLocalID,
			resolved:    true,
			ownerStatus: ownerStatusFor(candidate.OwnerState),
		}
	}
	return helperGroup{
		groupID:     groupedGroupID("1", "unresolved", candidate.StableID, purpose),
		ownerStatus: ownerStatusFor(candidate.OwnerState),
	}
}

// groupedGroupID hashes a canonical length-prefixed tuple. Length prefixes make
// the tuple unambiguous, and the digest keeps mutable fields (title, content,
// counts, timestamps) out of the identity entirely.
func groupedGroupID(parts ...string) string {
	var buffer bytes.Buffer
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		buffer.Write(length[:])
		buffer.WriteString(part)
	}
	sum := sha256.Sum256(buffer.Bytes())
	return "hg_" + base64.RawURLEncoding.EncodeToString(sum[:])
}

// groupedScopeIssuer mints the opaque member scope a rendered helper group
// carries. The production implementation is the bounded cache on the server.
type groupedScopeIssuer interface {
	issueMemberScope(groupID string, variant GroupedRouteVariant, filters GroupedFilters) (string, error)
}

// groupedView is the deterministic partition of one candidate set.
type groupedView struct {
	ordinary []GroupedCandidate
	helpers  []GroupedCandidate
	groups   []*helperGroup
	// groupsByOwner indexes resolved groups by their owner's stable identity.
	groupsByOwner map[string][]*helperGroup
	// nested marks the groups that render under a candidate row instead of as
	// their own top-level context container. A group nests under its owner when
	// the owner is an ordinary candidate, or when the owner is a helper whose
	// own group is reachable. A group whose owner is outside the candidate set
	// is a top-level container; a group whose owner edge points back into a
	// cycle is a top-level container too, so no saved helper and no owner
	// context ever disappears.
	nested map[string]bool
}

// buildGroupedView partitions candidates into ordinary items and owner groups.
// helper_review candidates become members; every other candidate stays ordinary
// and keeps its existing route fields. No candidate is dropped here: the route
// predicate already decided membership.
func buildGroupedView(candidates []GroupedCandidate) *groupedView {
	view := &groupedView{groupsByOwner: map[string][]*helperGroup{}}
	groupIndex := map[string]*helperGroup{}
	for _, candidate := range candidates {
		if candidate.Purpose == schema.SessionPurposeHelperReview {
			view.helpers = append(view.helpers, candidate)
			group := helperGroupFor(candidate)
			existing, ok := groupIndex[group.groupID]
			if !ok {
				existing = &helperGroup{groupID: group.groupID, ownerID: group.ownerID, resolved: group.resolved, ownerStatus: group.ownerStatus}
				groupIndex[group.groupID] = existing
				view.groups = append(view.groups, existing)
			}
			existing.members = append(existing.members, candidate)
			continue
		}
		view.ordinary = append(view.ordinary, candidate)
	}
	sort.SliceStable(view.groups, func(i, j int) bool { return view.groups[i].groupID < view.groups[j].groupID })
	for _, group := range view.groups {
		sort.SliceStable(group.members, func(i, j int) bool { return candidateLess(group.members[i], group.members[j]) })
		if group.resolved {
			view.groupsByOwner[group.ownerID] = append(view.groupsByOwner[group.ownerID], group)
		}
	}
	sort.SliceStable(view.ordinary, func(i, j int) bool {
		return itemOrderLess(view.ordinary[i].StartMs, view.ordinary[i].StableID, view.ordinary[j].StartMs, view.ordinary[j].StableID)
	})
	for owner := range view.groupsByOwner {
		groups := view.groupsByOwner[owner]
		sort.SliceStable(groups, func(i, j int) bool { return groups[i].groupID < groups[j].groupID })
		view.groupsByOwner[owner] = groups
	}
	view.nested = nestedGroups(view.groups, view.ordinary, candidates)
	return view
}

// nestedGroups reports which helper groups render under a candidate row rather
// than as their own top-level context container. A group nests under its owner
// when the owner is an ordinary candidate, or when the owner is a helper member
// of a group that is itself reachable. A group whose owner is absent is a
// top-level container in its own right. Whatever stays unreachable is an owner
// cycle: the owner edge points only at helpers that point back, so no ordinary
// root exists. Those groups are emitted as top-level context containers with a
// conflicting owner status, so a stored cycle yields honest, expandable
// contexts instead of no visible items at all.
func nestedGroups(groups []*helperGroup, ordinary []GroupedCandidate, candidates []GroupedCandidate) map[string]bool {
	ordinaryIDs := make(map[string]bool, len(ordinary))
	for _, candidate := range ordinary {
		ordinaryIDs[candidate.StableID] = true
	}
	present := candidateIndex(candidates)
	// memberGroup names the group a helper member belongs to, so an owner that
	// is itself a helper can be resolved to the group that renders it.
	memberGroup := make(map[string]*helperGroup, len(groups))
	for _, group := range groups {
		for _, member := range group.members {
			memberGroup[member.StableID] = group
		}
	}

	// reachable marks every group a caller can open, whether nested or emitted
	// as its own top-level container.
	reachable := make(map[string]bool, len(groups))
	for _, group := range groups {
		if !group.resolved || !present[group.ownerID] {
			reachable[group.groupID] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for _, group := range groups {
			if reachable[group.groupID] || !group.resolved || !present[group.ownerID] {
				continue
			}
			if ordinaryIDs[group.ownerID] {
				reachable[group.groupID] = true
				changed = true
				continue
			}
			// The owner is itself a helper. It is only open when the group that
			// contains it is open; that propagates reachability down the owner
			// chain and leaves a cycle unreachable.
			if ownerGroup, ok := memberGroup[group.ownerID]; ok && reachable[ownerGroup.groupID] {
				reachable[group.groupID] = true
				changed = true
			}
		}
	}

	nested := make(map[string]bool, len(groups))
	for _, group := range groups {
		if !reachable[group.groupID] {
			continue
		}
		if !group.resolved || !present[group.ownerID] {
			// A root group is reachable through its own top-level container.
			continue
		}
		nested[group.groupID] = true
	}
	return nested
}

// groupsNestedUnder returns the groups owned by one candidate that render under
// that candidate's row. A group emitted as its own top-level container is
// excluded, so a cyclic owner edge never appears twice and the member projection
// cannot recurse into itself.
func (v *groupedView) groupsNestedUnder(ownerID string) []*helperGroup {
	groups := v.groupsByOwner[ownerID]
	if len(groups) == 0 {
		return nil
	}
	nested := make([]*helperGroup, 0, len(groups))
	for _, group := range groups {
		if v.nested[group.groupID] {
			nested = append(nested, group)
		}
	}
	return nested
}

// candidateLess orders helper members ascending session start, then stable ID.
func candidateLess(a, b GroupedCandidate) bool {
	if a.StartMs != b.StartMs {
		return a.StartMs < b.StartMs
	}
	return a.StableID < b.StableID
}

// itemOrderLess orders top-level items descending start, then ascending stable
// identity. A context container passes its latest visible helper's start and
// its group ID, so it never borrows a hidden owner's time.
func itemOrderLess(aStart int64, aID string, bStart int64, bID string) bool {
	if aStart != bStart {
		return aStart > bStart
	}
	return aID < bID
}

// helperCountByID indexes candidate identity presence so the grouping can tell
// an owner that is present (ordinary or helper) from one that is absent.
func candidateIndex(candidates []GroupedCandidate) map[string]bool {
	index := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		index[candidate.StableID] = true
	}
	return index
}

// buildGroupedListPayload assembles the opt-in grouped list response. It orders
// and counts exactly the route candidates, attaches each rendered group's opaque
// member scope, and pages the TOP-LEVEL units only. Helper expansion pages
// through the member operation.
func buildGroupedListPayload(filters GroupedFilters, candidates []GroupedCandidate, page, limit int, issuer groupedScopeIssuer) (*schema.LocalSessionListPayload, error) {
	if !filters.Variant.IsValid() {
		return nil, fmt.Errorf("api.buildGroupedListPayload: originating route %q is outside the closed set; no grouped list was assembled and the caller cannot replay a member scope; request view=grouped from a registered route", filters.Variant)
	}
	view := buildGroupedView(candidates)
	present := candidateIndex(candidates)

	type topLevelItem struct {
		startMs int64
		stable  string
		item    schema.LocalSessionListItem
	}
	items := make([]topLevelItem, 0, len(view.ordinary)+len(view.groups))
	for _, candidate := range view.ordinary {
		groups, err := view.helperGroupSummaries(view.groupsNestedUnder(candidate.StableID), filters, issuer)
		if err != nil {
			return nil, err
		}
		items = append(items, topLevelItem{
			startMs: candidate.StartMs,
			stable:  candidate.StableID,
			item: schema.LocalSessionListItem{
				Kind:         schema.SessionListItemTranscript,
				Transcript:   &schema.LocalSessionRow{Session: candidate.Summary, Sync: candidate.Sync, Matches: candidate.Matches},
				HelperGroups: groups,
			},
		})
	}
	for _, group := range view.groups {
		if view.nested[group.groupID] {
			// The owner is reachable, so the group renders under its owner's
			// row (ordinary item or helper member expansion), never as a
			// top-level container.
			continue
		}
		ownerStatus := group.ownerStatus
		if group.resolved && present[group.ownerID] {
			// The owner is a candidate, yet the group is unreachable: its owner
			// edge points back into a cycle. Report the contradiction honestly
			// instead of claiming the owner is merely unavailable.
			ownerStatus = schema.RelationshipNavigationConflicting
		}
		groups, err := view.helperGroupSummaries([]*helperGroup{group}, filters, issuer)
		if err != nil {
			return nil, err
		}
		items = append(items, topLevelItem{
			startMs: group.latestStart(),
			stable:  group.groupID,
			item: schema.LocalSessionListItem{
				Kind: schema.SessionListItemContextContainer,
				Context: &schema.HelperContextSummary{
					GroupID:     group.groupID,
					OwnerStatus: ownerStatus,
				},
				HelperGroups: groups,
			},
		})
	}
	sort.SliceStable(items, func(i, j int) bool {
		return itemOrderLess(items[i].startMs, items[i].stable, items[j].startMs, items[j].stable)
	})

	totalItems := len(items)
	if limit <= 0 {
		limit = totalItems
		if limit < 1 {
			limit = 1
		}
	}
	if page < 1 {
		page = 1
	}
	start := (page - 1) * limit
	if start > len(items) {
		start = len(items)
	}
	end := start + limit
	if end > len(items) {
		end = len(items)
	}
	pageItems := make([]schema.LocalSessionListItem, 0, end-start)
	for _, item := range items[start:end] {
		pageItems = append(pageItems, item.item)
	}

	payload := &schema.LocalSessionListPayload{
		Items:                pageItems,
		Page:                 page,
		Limit:                limit,
		TotalItems:           totalItems,
		OrdinarySessionTotal: len(view.ordinary),
		HelperThreadTotal:    len(view.helpers),
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("api.buildGroupedListPayload: validate grouped %s list: %w; no grouped response was emitted", filters.Variant, err)
	}
	return payload, nil
}

// latestStart returns the newest start time among a group's visible members.
func (g *helperGroup) latestStart() int64 {
	var latest int64
	for i, member := range g.members {
		if i == 0 || member.StartMs > latest {
			latest = member.StartMs
		}
	}
	return latest
}

// helperGroupSummaries renders the collapsed group summaries for one owner with
// a freshly issued member scope per group.
func (v *groupedView) helperGroupSummaries(groups []*helperGroup, filters GroupedFilters, issuer groupedScopeIssuer) ([]schema.HelperGroupSummary, error) {
	if len(groups) == 0 {
		return nil, nil
	}
	summaries := make([]schema.HelperGroupSummary, 0, len(groups))
	for _, group := range groups {
		if len(group.members) == 0 {
			continue
		}
		scope, err := issuer.issueMemberScope(group.groupID, filters.Variant, filters)
		if err != nil {
			return nil, err
		}
		summaries = append(summaries, schema.HelperGroupSummary{
			GroupID:           group.groupID,
			Purpose:           schema.SessionPurposeHelperReview,
			HelperThreadCount: len(group.members),
			MemberScope:       scope,
		})
	}
	if len(summaries) == 0 {
		return nil, nil
	}
	return summaries, nil
}

// buildGroupedMembersPayload replays one group's members from the already
// predicate-applied candidates and pages them. It returns errGroupedGroupUnknown
// when the group key no longer matches, so the handler refuses with an
// actionable refresh instead of an empty lie.
func buildGroupedMembersPayload(groupID string, filters GroupedFilters, candidates []GroupedCandidate, page, limit int, issuer groupedScopeIssuer) (*schema.LocalHelperMembersPayload, error) {
	if !filters.Variant.IsValid() {
		return nil, fmt.Errorf("api.buildGroupedMembersPayload: originating route %q is outside the closed set; member paging cannot replay the predicate; refresh the grouped list", filters.Variant)
	}
	view := buildGroupedView(candidates)
	var group *helperGroup
	for _, candidate := range view.groups {
		if candidate.groupID == groupID {
			group = candidate
			break
		}
	}
	if group == nil {
		return nil, fmt.Errorf("api.buildGroupedMembersPayload: group %q has no members under the replayed %s predicate: %w", groupID, filters.Variant, errGroupedGroupUnknown)
	}

	if limit <= 0 {
		limit = groupedMembersDefaultLimit
	}
	if limit > groupedMembersMaxLimit {
		limit = groupedMembersMaxLimit
	}
	if page < 1 {
		page = 1
	}
	total := len(group.members)
	start := (page - 1) * limit
	if start > total {
		start = total
	}
	end := start + limit
	if end > total {
		end = total
	}

	members := make([]schema.LocalSessionListItem, 0, end-start)
	for _, member := range group.members[start:end] {
		groups, err := view.helperGroupSummaries(view.groupsNestedUnder(member.StableID), filters, issuer)
		if err != nil {
			return nil, err
		}
		members = append(members, schema.LocalSessionListItem{
			Kind:         schema.SessionListItemTranscript,
			Transcript:   &schema.LocalSessionRow{Session: member.Summary, Sync: member.Sync, Matches: member.Matches},
			HelperGroups: groups,
		})
	}

	payload := &schema.LocalHelperMembersPayload{
		Members: members,
		Page:    page,
		Limit:   limit,
		Total:   total,
	}
	if err := payload.Validate(); err != nil {
		return nil, fmt.Errorf("api.buildGroupedMembersPayload: validate members of group %q: %w; no member page was emitted", groupID, err)
	}
	return payload, nil
}
