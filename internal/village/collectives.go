package village

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/peasant-labs/schema"
)

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

// TranscriptShares returns the current share of one owned transcript with each
// collective, with its status, from the transcript read
// (GET /api/v1/transcripts/{id}). A status outside the closed set is refused,
// so a caller never reports access it cannot name.
func (c *VillageClient) TranscriptShares(ctx context.Context, id schema.TranscriptID) ([]schema.VillageEnrichedTranscriptShare, error) {
	var response struct {
		EnrichedShares []schema.VillageEnrichedTranscriptShare `json:"enriched_shares"`
	}
	operation := "read the collectives of transcript " + id.String()
	if err := c.readCollectiveJSON(ctx, operation, transcriptPath(id), &response); err != nil {
		return nil, err
	}
	for _, share := range response.EnrichedShares {
		if share.GroupID == "" || !isShareStatus(share.Status) {
			return nil, fmt.Errorf("%s: Village returned a share with collective %q and status %q; the status is outside the closed set, so the access cannot be reported; update Peasant or check the Village version", operation, share.GroupID, share.Status)
		}
	}
	if response.EnrichedShares == nil {
		response.EnrichedShares = []schema.VillageEnrichedTranscriptShare{}
	}
	return response.EnrichedShares, nil
}

// ShareTranscript offers one owned transcript to one collective
// (POST /api/v1/transcripts/{id}/share). A 2xx answer means Village took the
// request; it does not say whether the collective accepted the share, holds it
// for its owner's approval, or skipped it, so a caller reads TranscriptShares to
// learn that. Village answers 409 when the collective already holds a live
// share of the transcript.
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
