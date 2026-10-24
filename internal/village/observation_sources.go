package village

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/peasant-labs/schema"
)

// ReadObservationMemberships reads the complete membership input from the
// canonical route, without changing the existing live suggestion API.
func (c *VillageClient) ReadObservationMemberships(ctx context.Context) ([]schema.VillageUserCollective, error) {
	var rows []schema.VillageUserCollective
	if err := c.readObservationJSON(ctx, "read complete collective memberships", "/api/v1/collectives", &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("read complete collective memberships: missing or null array; no membership reasons can be replaced; peasant online must emit the complete array before retrying")
	}
	seen := make(map[schema.VillageUUID]bool, len(rows))
	for _, row := range rows {
		if !IsCollectiveID(row.ID) || seen[row.ID] {
			return nil, fmt.Errorf("read complete collective memberships: invalid or repeated collective ID; no membership reasons can be replaced; correct the peasant online response and retry")
		}
		if row.Name == "" || row.MemberSince.IsZero() || !slices.Contains(schema.AllVillageGroupRoles, row.Role) {
			return nil, fmt.Errorf("read complete collective memberships: missing name/member time or unknown role; no membership reasons can be replaced; correct the peasant online response and retry")
		}
		if err := validateObservationPolicy(row.AcceptanceMode, row.DataAccess); err != nil {
			return nil, err
		}
		seen[row.ID] = true
	}
	return rows, nil
}

// ReadObservationCollective returns only the permitted collective projection.
// Rosters, transcript lists and organization members from the detail response
// are transport-local and never become observation-cache discovery inputs.
func (c *VillageClient) ReadObservationCollective(ctx context.Context, id schema.VillageUUID) (schema.VillageCollectiveDetailRecord, error) {
	if !IsCollectiveID(id) {
		return schema.VillageCollectiveDetailRecord{}, fmt.Errorf("read collective metadata: invalid collective UUID; no request sent; resolve an exact canonical collective ID and retry")
	}
	var response schema.VillageCollectiveDetailResponse
	path := "/api/v1/collectives/" + url.PathEscape(id.String())
	if err := c.readObservationJSON(ctx, "read visible collective metadata", path, &response); err != nil {
		return schema.VillageCollectiveDetailRecord{}, err
	}
	row := response.Group
	if row.ID != id || row.Name == "" {
		return schema.VillageCollectiveDetailRecord{}, fmt.Errorf("read visible collective metadata: missing metadata or wrong collective identity; no metadata can be installed; correct the peasant online response and retry")
	}
	if err := validateObservationPolicy(row.AcceptanceMode, row.DataAccess); err != nil {
		return schema.VillageCollectiveDetailRecord{}, err
	}
	return row, nil
}

func validateObservationPolicy(acceptance schema.VillageCollectiveAcceptanceMode, access schema.VillageCollectiveDataAccess) error {
	if !slices.Contains(schema.AllVillageGroupAcceptanceModes, acceptance) || !slices.Contains(schema.AllVillageGroupDataAccessPolicies, access) {
		return fmt.Errorf("read collective metadata: unknown acceptance or data-access policy; no policy observation can be installed; update the client or correct the peasant online response before retrying")
	}
	return nil
}

// ReadOwnShareEvents reads a complete owner-only pair history. The schema owns
// individual attempt validation and all mutable-attempt transition semantics.
// A 403/404 is returned unchanged for explicit unavailable coverage, never []
// that would imply the owner's retained business history had become empty.
func (c *VillageClient) ReadOwnShareEvents(ctx context.Context, id schema.TranscriptID, collective schema.VillageUUID) ([]schema.VillageShareEvent, error) {
	if _, err := schema.NewTranscriptID(id.String()); err != nil {
		return nil, fmt.Errorf("read own pair history: invalid transcript ID: %w; no request sent; resolve its canonical ID and retry", err)
	}
	if !IsCollectiveID(collective) {
		return nil, fmt.Errorf("read own pair history: invalid collective UUID; no request sent; resolve its canonical ID and retry")
	}
	var events []schema.VillageShareEvent
	if err := c.readObservationJSON(ctx, "read complete own contribution attempts", shareEventsPath(id, collective), &events); err != nil {
		return nil, err
	}
	if events == nil {
		return nil, fmt.Errorf("read own pair history: missing or null complete array; retained history is unchanged; peasant online must emit an explicit complete array before retrying")
	}
	seen := make(map[int32]bool, len(events))
	for _, event := range events {
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("read own pair history: %w", err)
		}
		if seen[event.EventNum] {
			return nil, fmt.Errorf("read own pair history: repeated attempt ordinal in one complete pair response; no prefix can be installed; correct the peasant online history response and retry")
		}
		seen[event.EventNum] = true
	}
	return events, nil
}

// ReadObservationOrganizations reads the caller's complete organization input.
// The separate permitted linked-collective projection is still required before
// this input can establish complete organization-derived catalog relevance.
func (c *VillageClient) ReadObservationOrganizations(ctx context.Context) ([]schema.VillageMetadataUserOrganization, error) {
	var rows []schema.VillageMetadataUserOrganization
	if err := c.readObservationJSON(ctx, "read selected account organizations", "/api/v1/auth/orgs", &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("read selected account organizations: missing or null complete array; organization relevance cannot be replaced; peasant online must emit the complete array before retrying")
	}
	seen := make(map[string]bool, len(rows))
	for _, row := range rows {
		login := strings.ToLower(row.OrgLogin)
		invalidIdentity := login == "" || row.OrgID < 1 || seen[login]
		if invalidIdentity || row.FetchedAt.IsZero() {
			return nil, fmt.Errorf("read selected account organizations: missing or repeated organization identity or observation time; organization relevance cannot be replaced; correct the peasant online response and retry")
		}
		seen[login] = true
	}
	return rows, nil
}

// ReadOwnContributionIdentities projects complete validated owner discovery,
// including historical identities that do not confer current relevance.
func (c *VillageClient) ReadOwnContributionIdentities(ctx context.Context) ([]schema.VillageUUID, error) {
	rows, err := c.ReadOwnContributionDiscovery(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]schema.VillageUUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids, nil
}

func (c *VillageClient) ReadOwnContributionDiscovery(ctx context.Context) ([]schema.VillageContributedCollective, error) {
	var raw json.RawMessage
	if err := c.readObservationJSON(ctx, "discover owned contribution collective identities", "/api/v1/users/me/collectives/contributions", &raw); err != nil {
		return nil, err
	}
	response, err := decodeContributionDiscovery(raw)
	if err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, err
	}
	if len(response.Collectives) > schema.MaxCollectiveFetchItems {
		return nil, fmt.Errorf("discover owned contributions: complete source exceeds the metadata invocation item bound; no prefix installed; narrow source inventory before retrying")
	}
	for _, row := range response.Collectives {
		if row.Name == "" {
			return nil, fmt.Errorf("read contribution discovery: empty collective label; no relevance installed; correct the peasant online source response")
		}
	}
	return response.Collectives, nil
}

// SearchObservationCollectives resolves an explicit exact-name request, never
// populates a world-directory cache. The existing search route caps at 50; a
// full cap cannot prove absence of another exact match, so callers must use UUID.
func (c *VillageClient) SearchObservationCollectives(ctx context.Context, name string) ([]schema.VillageCollectiveSearchResult, error) {
	var response schema.VillageCollectiveSearchResponse
	if err := c.readObservationJSON(ctx, "resolve an explicitly selected collective name", "/api/v1/collectives/search?q="+url.QueryEscape(name)+"&limit=50", &response); err != nil {
		return nil, err
	}
	if response.Collectives == nil {
		return nil, fmt.Errorf("resolve collective name: missing complete search array; no selection made; use a UUID or retry validated search")
	}
	if len(response.Collectives) >= 50 {
		return nil, fmt.Errorf("resolve collective name: search reached the server result cap, so exact-name uniqueness is unknown; no selection made; use the collective's canonical UUID")
	}
	seen := map[schema.VillageUUID]bool{}
	for _, row := range response.Collectives {
		if !IsCollectiveID(row.ID) || row.Name == "" || seen[row.ID] {
			return nil, fmt.Errorf("resolve collective name: invalid or repeated search identity; no selection made; correct the peasant online response or use UUID")
		}
		seen[row.ID] = true
	}
	return response.Collectives, nil
}

func (c *VillageClient) ReadOrganizationCollectives(ctx context.Context, login string) ([]schema.VillageOrganizationCollective, error) {
	if strings.TrimSpace(login) == "" {
		return nil, fmt.Errorf("read organization collectives: login is empty; no request sent; use a selected-account organization login")
	}
	var response schema.VillageOrganizationCollectivesResponse
	if err := c.readObservationJSON(ctx, "read permitted organization collective projection", "/api/v1/orgs/"+url.PathEscape(login), &response); err != nil {
		return nil, err
	}
	if err := response.Validate(); err != nil {
		return nil, err
	}
	return response.Collectives, nil
}
