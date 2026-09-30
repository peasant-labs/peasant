package village

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/peasant-labs/schema"
)

// CollectiveIDPattern is the lowercase form Village emits for a collective,
// and the pattern the contract declares for schema.VillageUUID.
const CollectiveIDPattern = `^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`

var canonicalUUID = regexp.MustCompile(CollectiveIDPattern)

// IsCollectiveID reports whether id is a collective identifier in the form
// Village emits.
func IsCollectiveID(id schema.VillageUUID) bool { return canonicalUUID.MatchString(string(id)) }

// collectivesEndpoint lists the collectives the signed-in user belongs to.
const collectivesEndpoint = "/api/v1/groups"

// collectiveResponseLimit bounds how much of one collective or share answer is
// read. The answers are small rows; the limit keeps a misbehaving server from
// holding the local server's memory.
const collectiveResponseLimit = 4 << 20

func collectiveRepositoriesPath(id schema.VillageUUID) string {
	return collectivesEndpoint + "/" + url.PathEscape(id.String()) + "/repositories"
}

func transcriptPath(id schema.TranscriptID) string {
	return transcriptEndpoint + url.PathEscape(id.String())
}

func transcriptSharePath(id schema.TranscriptID) string {
	return transcriptPath(id) + "/share"
}

func transcriptUnsharePath(id schema.TranscriptID, collective schema.VillageUUID) string {
	return transcriptSharePath(id) + "/" + url.PathEscape(collective.String())
}

func shareEventsPath(id schema.TranscriptID, collective schema.VillageUUID) string {
	return "/api/v1/users/me/collectives/" + url.PathEscape(collective.String()) + "/transcripts/" + url.PathEscape(id.String()) + "/events"
}

// StatusError is a Village answer outside 2xx to a collective read or a share
// change. Message is Village's own error text when it sent one, so a caller can
// show why Village refused.
type StatusError struct {
	Operation  string
	Path       string
	StatusCode int
	Message    string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("%s: Village answered %d for %s: %s", e.Operation, e.StatusCode, e.Path, e.Message)
}

// ListCollectives returns the collectives the signed-in user belongs to, with
// their role in each (GET /api/v1/groups).
func (c *VillageClient) ListCollectives(ctx context.Context) ([]schema.VillageUserGroup, error) {
	var collectives []schema.VillageUserGroup
	if err := c.readCollectiveJSON(ctx, "list collectives", collectivesEndpoint, &collectives); err != nil {
		return nil, err
	}
	if collectives == nil {
		collectives = []schema.VillageUserGroup{}
	}
	return collectives, nil
}

// ListCollectiveRepositories returns the GitHub repositories linked to one
// collective (GET /api/v1/groups/{id}/repositories). Village shows a member
// only the rows of accounts their own GitHub account controls.
func (c *VillageClient) ListCollectiveRepositories(ctx context.Context, id schema.VillageUUID) ([]schema.VillageLinkedRepository, error) {
	var response schema.VillageLinkedRepositoriesResponse
	if err := c.readCollectiveJSON(ctx, "list the repositories of collective "+id.String(), collectiveRepositoriesPath(id), &response); err != nil {
		return nil, err
	}
	if response.Repositories == nil {
		response.Repositories = []schema.VillageLinkedRepository{}
	}
	return response.Repositories, nil
}

// TranscriptShare is one collective's current share of a transcript.
type TranscriptShare struct {
	CollectiveID schema.VillageUUID
	Name         string
	Status       schema.VillageShareStatus
}

// TranscriptShares returns every collective that holds a current share row of
// one owned transcript, with the row's status. The transcript read
// (GET /api/v1/transcripts/{id}) lists every current row in "shares" but
// reports a status only for approved rows, in "enriched_shares". The status
// of any other row, pending or rejected, is the latest event of that
// collective's share history (see LatestShareStatus). A status outside the
// closed set fails the read, so a caller never reports access it cannot name.
func (c *VillageClient) TranscriptShares(ctx context.Context, id schema.TranscriptID) ([]TranscriptShare, error) {
	var response struct {
		Shares         []schema.VillageTranscriptShare         `json:"shares"`
		EnrichedShares []schema.VillageEnrichedTranscriptShare `json:"enriched_shares"`
	}
	operation := "read the collectives of transcript " + id.String()
	if err := c.readCollectiveJSON(ctx, operation, transcriptPath(id), &response); err != nil {
		return nil, err
	}
	approved := make(map[schema.VillageUUID]bool, len(response.EnrichedShares))
	for _, share := range response.EnrichedShares {
		if share.Status != schema.VillageShareStatusApproved {
			return nil, fmt.Errorf("%s: Village listed collective %q with status %q among the approved shares; the access cannot be reported; update Peasant or check the Village version", operation, share.GroupID, share.Status)
		}
		approved[share.GroupID] = true
	}
	shares := make([]TranscriptShare, 0, len(response.Shares))
	for _, row := range response.Shares {
		if row.GroupID == "" {
			return nil, fmt.Errorf("%s: Village listed a share with no collective; the access cannot be reported; check the Village version", operation)
		}
		status := schema.VillageShareStatusApproved
		if !approved[row.GroupID] {
			latest, err := c.LatestShareStatus(ctx, id, row.GroupID)
			if err != nil {
				return nil, err
			}
			if latest == "" {
				continue
			}
			status = latest
		}
		shares = append(shares, TranscriptShare{CollectiveID: row.GroupID, Name: row.GroupName, Status: status})
	}
	return shares, nil
}

// LatestShareStatus returns the status of the latest event of one owned
// transcript's share history in one collective
// (GET /api/v1/users/me/collectives/{groupId}/transcripts/{transcriptId}/events),
// or "" when the transcript was never offered there. Village records every
// share, approval, rejection, and withdrawal as an event, so the latest event
// says what the collective did with the last share. A status outside the
// closed set fails the read.
func (c *VillageClient) LatestShareStatus(ctx context.Context, id schema.TranscriptID, collective schema.VillageUUID) (schema.VillageShareStatus, error) {
	var events []schema.VillageShareEvent
	operation := "read the share history of transcript " + id.String() + " in collective " + collective.String()
	if err := c.readCollectiveJSON(ctx, operation, shareEventsPath(id, collective), &events); err != nil {
		return "", err
	}
	var latest *schema.VillageShareEvent
	for i := range events {
		if !isShareStatus(events[i].Status) {
			return "", fmt.Errorf("%s: Village returned status %q, which is outside the closed set, so the access cannot be reported; update Peasant or check the Village version", operation, events[i].Status)
		}
		if latest == nil || events[i].EventNum > latest.EventNum {
			latest = &events[i]
		}
	}
	if latest == nil {
		return "", nil
	}
	return latest.Status, nil
}

// ShareTranscript offers one owned transcript to one collective
// (POST /api/v1/transcripts/{id}/share). A 2xx answer means Village took the
// request; it does not say whether the collective accepted the share, holds it
// for its owner's approval, or skipped it, so a caller reads LatestShareStatus
// to learn that. Village answers 409 when the collective already holds a live
// share of the transcript, and also when another request shared it at the same
// moment.
func (c *VillageClient) ShareTranscript(ctx context.Context, id schema.TranscriptID, collective schema.VillageUUID) error {
	body, err := json.Marshal(schema.VillageShareTranscriptRequest{GroupIDs: []schema.VillageUUID{collective}})
	if err != nil {
		return fmt.Errorf("share transcript %s with collective %s: encode request: %w", id, collective, err)
	}
	return c.changeShare(ctx, http.MethodPost, "share transcript "+id.String()+" with collective "+collective.String(), transcriptSharePath(id), body)
}

// UnshareTranscript takes one owned transcript back from one collective
// (DELETE /api/v1/transcripts/{id}/share/{groupID}). Its members lose access.
// Village answers 2xx also when the collective held no live share.
func (c *VillageClient) UnshareTranscript(ctx context.Context, id schema.TranscriptID, collective schema.VillageUUID) error {
	return c.changeShare(ctx, http.MethodDelete, "take transcript "+id.String()+" back from collective "+collective.String(), transcriptUnsharePath(id, collective), nil)
}

// These reads and share changes do not go through c.do: they are not stages of
// the push pipeline, so they are neither profiled as one nor reported to the
// run's request observer.
func (c *VillageClient) readCollectiveJSON(ctx context.Context, operation, path string, into any) error {
	req, err := c.newAuthedGet(ctx, c.baseURL+path)
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: reach Village: %w", operation, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, collectiveResponseLimit))
	if err != nil {
		return fmt.Errorf("%s: read Village's answer: %w", operation, err)
	}
	if resp.StatusCode != http.StatusOK {
		return &StatusError{Operation: operation, Path: path, StatusCode: resp.StatusCode, Message: villageErrorText(raw)}
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return fmt.Errorf("%s: decode Village's answer: %w", operation, err)
	}
	return nil
}

func (c *VillageClient) changeShare(ctx context.Context, method, operation, path string, body []byte) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
	if err != nil {
		return fmt.Errorf("%s: create request: %w", operation, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s: reach Village: %w", operation, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, collectiveResponseLimit))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &StatusError{Operation: operation, Path: path, StatusCode: resp.StatusCode, Message: villageErrorText(raw)}
	}
	return nil
}

// villageErrorText returns the error text of Village's {"error": ...} envelope,
// or the start of the body when it sent another shape.
func villageErrorText(raw []byte) string {
	var envelope schema.VillageErrorResponse
	if err := json.Unmarshal(raw, &envelope); err == nil && strings.TrimSpace(envelope.Error) != "" {
		return envelope.Error
	}
	text := strings.TrimSpace(string(raw))
	if len(text) > 512 {
		text = text[:512] + "..."
	}
	if text == "" {
		return "no error text"
	}
	return text
}

func isShareStatus(status schema.VillageShareStatus) bool {
	for _, known := range schema.AllVillageShareStatuses {
		if status == known {
			return true
		}
	}
	return false
}
