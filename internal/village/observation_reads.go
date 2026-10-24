package village

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/peasant-labs/schema"
)

// readObservationJSON accepts exactly one complete bounded JSON response.
// Observation reads do not follow redirects: even a same-host redirect can
// select a different Village base path or forward a saved credential.
func (c *VillageClient) readObservationJSON(ctx context.Context, operation, path string, into any) error {
	if err := observationBudgetAvailable(ctx); err != nil {
		return err
	}
	req, err := c.newAuthedGet(ctx, c.baseURL+path)
	if err != nil {
		return fmt.Errorf("%s: construct metadata request: %w; no observations changed; correct the peasant online URL and retry", operation, err)
	}
	if req.URL.Host == "" || (req.URL.Scheme != "http" && req.URL.Scheme != "https") {
		return fmt.Errorf("%s: metadata request has no supported absolute HTTP URL; no peasant online request attempted; correct the selected peasant online URL before retrying", operation)
	}
	client := *c.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%s: metadata request cancelled before HTTP attempt: %w; prior observations retained; retry explicitly", operation, err)
	}
	if c.beforeRequest != nil {
		c.beforeRequest()
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: read peasant online metadata: %w; prior observations remain dated; verify connection and retry explicitly", operation, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, collectiveResponseLimit+1))
	if err != nil {
		return fmt.Errorf("%s: read complete metadata response: %w; no partial set is usable; retry explicitly", operation, err)
	}
	if len(raw) > collectiveResponseLimit {
		return fmt.Errorf("%s: metadata response exceeds the complete-response byte limit; no partial set is usable; narrow the source or upgrade peasant online and retry", operation)
	}
	if err := chargeObservationBytes(ctx, int64(len(raw))); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Operation: operation, Path: path, StatusCode: resp.StatusCode, Message: villageErrorText(raw)}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: decode complete metadata response: %w; prior observations remain unchanged; correct the peasant online response and retry", operation, err)
	}
	return nil
}

// VerifyUser obtains the stable current caller identity, not a username match.
func (c *VillageClient) VerifyUser(ctx context.Context) (schema.VillageUser, error) {
	var user schema.VillageUser
	if err := c.readObservationJSON(ctx, "verify selected peasant online account", "/api/v1/auth/me", &user); err != nil {
		return user, err
	}
	if !IsCollectiveID(user.ID) {
		return schema.VillageUser{}, fmt.Errorf("verify selected peasant online account: auth/me returned no canonical user UUID; no private metadata can be selected; repair peasant online authentication and retry")
	}
	return user, nil
}

// ReadTranscriptCollectives reads only the dedicated viewer-disclosed set. It
// never probes foreign owner history or falls back to broader detail shares.
// A nil set is malformed, not successful empty coverage.
func (c *VillageClient) ReadTranscriptCollectives(ctx context.Context, id schema.TranscriptID) ([]schema.VillageTranscriptCollective, error) {
	if _, err := schema.NewTranscriptID(id.String()); err != nil {
		return nil, fmt.Errorf("read transcript associations: invalid transcript ID: %w; no request sent; supply its canonical ID", err)
	}
	var result schema.VillageTranscriptCollectivesResponse
	if err := c.readObservationJSON(ctx, "read disclosed transcript collectives", transcriptPath(id)+"/collectives", &result); err != nil {
		return nil, err
	}
	if result.Collectives == nil {
		return nil, fmt.Errorf("read disclosed transcript collectives: missing or null complete set; prior evidence remains unchanged; peasant online must emit an explicit array, then retry")
	}
	seen := make(map[schema.VillageUUID]bool, len(result.Collectives))
	for _, row := range result.Collectives {
		invalidIdentity := !IsCollectiveID(row.ID) || seen[row.ID]
		incompleteMetadata := row.Name == "" || row.SharedAt.IsZero()
		if invalidIdentity || incompleteMetadata {
			return nil, fmt.Errorf("read disclosed transcript collectives: invalid or repeated collective identity or missing name/time; prior evidence remains unchanged; repair peasant online response and retry")
		}
		seen[row.ID] = true
	}
	return result.Collectives, nil
}

// ReadOwnSubmissions discovers all available owned pairs in one collective.
// This unpaginated contract requires the entire array before history staging.
func (c *VillageClient) ReadOwnSubmissions(ctx context.Context, collective schema.VillageUUID) ([]schema.VillageCollectiveSubmission, error) {
	if !IsCollectiveID(collective) {
		return nil, fmt.Errorf("read own submissions: invalid collective UUID; no request sent; resolve the exact collective ID and retry")
	}
	var rows []schema.VillageCollectiveSubmission
	path := "/api/v1/users/me/collectives/" + url.PathEscape(collective.String()) + "/submissions"
	if err := c.readObservationJSON(ctx, "read own collective submissions", path, &rows); err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, fmt.Errorf("read own collective submissions: missing or null complete array; no history prefix is usable; repair peasant online response and retry")
	}
	seen := make(map[schema.TranscriptID]bool, len(rows))
	for _, row := range rows {
		_, idErr := schema.NewTranscriptID(row.TranscriptID.String())
		invalidPair := idErr != nil || row.GroupID != collective || seen[row.TranscriptID]
		invalidAttempt := row.EventNum < 1 || !isShareStatus(row.Status) || row.RecordedAt.IsZero()
		if invalidPair || invalidAttempt {
			return nil, fmt.Errorf("read own collective submissions: invalid or repeated pair identity/status/time; no history prefix is usable; repair peasant online response and retry")
		}
		seen[row.TranscriptID] = true
	}
	return rows, nil
}
