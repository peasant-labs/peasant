package village

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/schema"
)

// --- Pull surface endpoints ---

// pullTranscriptsEndpoint lists pullable transcripts (own + group-shared).
const pullTranscriptsEndpoint = "/api/v1/pull/transcripts"

// pullTranscriptPath returns the metadata endpoint for a single transcript.
func pullTranscriptPath(id schema.TranscriptID) string {
	return pullTranscriptsEndpoint + "/" + id.String()
}

// pullTranscriptContentPath returns the blob-content endpoint for a transcript.
func pullTranscriptContentPath(id schema.TranscriptID) string {
	return pullTranscriptPath(id) + "/content"
}

// pullTranscriptAnnotationsPath returns the annotations endpoint for a transcript.
func pullTranscriptAnnotationsPath(id schema.TranscriptID) string {
	return pullTranscriptPath(id) + "/annotations"
}

// ErrNotModified is returned by GetPullTranscriptContent when the server answers
// a conditional GET (If-None-Match: <served-blob hash>) with 304 Not Modified —
// i.e. the locally-pulled blob is already current. Callers compare against it
// with errors.Is and treat it as "up-to-date, skip re-download", NEVER as a
// failure. It is a SENTINEL (no stack), distinct from any transport error.
var ErrNotModified = errors.New("village pull content not modified (304): the locally pulled blob matches the served-blob hash")

// ErrPullNotFound is wrapped (via %w) into the actionable 404 message returned by
// the pull GETs when a transcript is not pullable for the requesting account
// (does not exist, or is neither owned-by nor group-shared-with the requester;
// public transcripts are not pullable — no existence leak, 404-not-403). Callers
// classify with errors.Is; the pull pipeline maps it to PullStatus "not-found".
var ErrPullNotFound = errors.New("village pull transcript not found (404): not pullable for this account")

// ErrPullContractIncompatible is wrapped (via %w) into the actionable
// negotiation-failure messages returned by NegotiatePull when the village's
// advertised pull window is MISSING (village too old) or this CLI's pull-contract
// version falls OUTSIDE the advertised window. Callers classify with errors.Is;
// The pull pipeline maps it to PullStatus "contract-error".
var ErrPullContractIncompatible = errors.New("village pull contract incompatible: the village's pull window is absent or excludes this CLI's pull-contract version")

// --- Pull-window negotiation (stricter than push) ---
//
// The pull surface negotiates against the village's advertised pull window
// [MinPullContractVersion, PullContractVersion]. Unlike push (which fails OPEN
// when the village advertises no window), pull fails closed: a
// village that advertises no pull window is too old to serve the pull surface at
// all, so the CLI aborts with an actionable error rather than guessing.

// villageTooOldForPullError is the actionable error returned when the village
// advertises no pull window (older village predating the pull surface). It is the
// deliberate asymmetry vs push's fail-open back-compat. The
// villageURL locates WHICH village is too old (relevant for multi-village users).
// Wraps ErrPullContractIncompatible so callers can classify with errors.Is.
func villageTooOldForPullError(villageURL string) error {
	return fmt.Errorf(
		"peasant pull aborted: the village does not advertise a pull contract\n"+
			"  what: GET /api/v1/schema/version returned no pull window (pullContractVersion/minPullContractVersion absent)\n"+
			"  why:  this village predates the transcript pull surface\n"+
			"  where: pull-contract preflight (village %s)\n"+
			"  fix:  the pull surface requires an updated village; retry once the village has been upgraded (no CLI change is needed)\n"+
			"  (%w)",
		villageURL, ErrPullContractIncompatible)
}

// pullContractError is the actionable error returned when the village DOES
// advertise a pull window but this CLI's pull-envelope version
// (defaults.PullContractVersion) falls outside it. Wraps
// ErrPullContractIncompatible so callers can classify with errors.Is.
func pullContractError(cli, min, current schema.PushContractVersion) error {
	return fmt.Errorf(
		"peasant pull aborted: this CLI's pull-contract v%s is incompatible with the village's pull window [v%s, v%s]\n"+
			"  what: the village serves pull envelopes only within [v%s, v%s] and this CLI is outside it\n"+
			"  why:  the pull-envelope shapes (PullTranscriptInfo/PullListResponse/PullAnnotation) differ across the gap\n"+
			"  where: pull-contract preflight\n"+
			"  fix:  pin/upgrade the peasant CLI to a release whose pull-contract is within [v%s, v%s], then re-run the pull\n"+
			"  (%w)",
		cli, min, current, min, current, min, current, ErrPullContractIncompatible)
}

// NegotiatePull preflights the village schema-version endpoint and verifies the
// village's advertised pull window is present and compatible with this CLI's
// pull-envelope version. It returns nil to proceed, or an actionable error that
// aborts the pull. Stricter than the push negotiate(): a MISSING window is a hard
// error (villageTooOldForPullError), not a fail-open passthrough.
//
// This is the explicit NEGOTIATE stage of the pull pipeline
// (RESOLVE → AUTH-CHECK → NEGOTIATE → FETCH-META → …). The pipeline calls it
// EXACTLY ONCE per command, mirroring how push splits negotiate() from the
// client; the four pull GETs are pure data calls that do NOT re-preflight. This
// avoids the 3×/N+1 redundant /schema/version round-trips a per-GET preflight
// would incur, and removes the consistency window where the outcome could flip
// mid-pull. Contract-incompatible outcomes
// wrap ErrPullContractIncompatible (classify with errors.Is).
func (c *VillageClient) NegotiatePull(ctx context.Context) error {
	cli := defaults.PullContractVersion

	resp, _, err := c.GetSchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf(
			"peasant pull aborted: pull-contract preflight failed\n"+
				"  what: GET /api/v1/schema/version could not be reached or decoded\n"+
				"  why:  %v\n"+
				"  where: pull-contract preflight (village %s)\n"+
				"  fix:  verify the village URL and your network/credentials, then re-run the pull",
			err, c.baseURL)
	}
	if resp == nil {
		return villageTooOldForPullError(c.baseURL)
	}

	min := resp.MinPullContractVersion
	current := resp.PullContractVersion

	switch ClassifyContract(cli, min, current) {
	case NegotiationUnadvertised:
		return villageTooOldForPullError(c.baseURL)
	case NegotiationWithin:
		return nil
	default: // older-than-min or ahead-of-current ⇒ incompatible pull window
		return pullContractError(cli, min, current)
	}
}

// --- Pull GET methods consumed by the pull pipeline through a narrow interface ---
//
// These are PURE data calls: each issues exactly one authed GET against the pull
// surface and does NOT preflight the pull window. The pipeline drives
// the explicit NEGOTIATE stage via NegotiatePull(ctx) ONCE before the FETCH
// stages; `transcripts list` calls NegotiatePull once before
// ListPullableTranscripts. This keeps transport (client) separate from
// policy/stage orchestration (pipeline), matching the push split.

// ListPullableTranscripts fetches one page of the village's pullable-transcript
// listing (own + group-shared; public excluded by the village's canPullTranscript
// policy). It issues an authed GET to /api/v1/pull/transcripts with offset
// pagination (page/limit query params). A non-2xx status is returned as an
// actionable error. The caller is responsible for running NegotiatePull once
// before the FETCH stages — this method does NOT preflight the pull window.
func (c *VillageClient) ListPullableTranscripts(ctx context.Context, page, limit int) (*schema.PullListResponse, error) {
	endpoint := fmt.Sprintf("%s%s?page=%d&limit=%d", c.baseURL, pullTranscriptsEndpoint, page, limit)
	req, err := c.newAuthedGet(ctx, endpoint)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute pull-list request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, pullStatusError("list pullable transcripts", pullTranscriptsEndpoint, resp)
	}

	var result schema.PullListResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode pull-list response: %w", err)
	}
	return &result, nil
}

// GetPullTranscript fetches the village's metadata view of a single transcript
// (owner, visibility, served-blob content hash, publish contract version, …). It
// issues an authed GET to /api/v1/pull/transcripts/{id}. A non-2xx status (incl.
// 404 for a transcript the requester cannot pull — no existence leak) is returned
// as an actionable error. The caller runs NegotiatePull once before the FETCH
// stages — this method does NOT preflight the pull window.
func (c *VillageClient) GetPullTranscript(ctx context.Context, id schema.TranscriptID) (*schema.PullTranscriptInfo, error) {
	path := pullTranscriptPath(id)
	req, err := c.newAuthedGet(ctx, c.baseURL+path)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute pull-metadata request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, pullStatusError(fmt.Sprintf("get pull metadata for transcript %s", id), path, resp)
	}

	var result schema.PullTranscriptInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode pull-metadata response: %w", err)
	}
	return &result, nil
}

// GetPullTranscriptContent streams the transcript's blob bytes as served by the
// village. It is the conditional-GET path: when ifNoneMatch is non-empty it is
// sent verbatim as the If-None-Match header (the caller passes the locally-stored
// served-blob hash), and a 304 Not Modified response is reported as the
// ErrNotModified sentinel (body drained+closed, nil reader) so the caller skips
// re-download. On 200 the caller OWNS the returned io.ReadCloser and MUST Close
// it; the second return value is the server's ETag (served-blob hash, may be
// empty when the village has not computed one ⇒ caller falls back to a local hash
// compare). The caller runs NegotiatePull once before the FETCH stages — this
// method does NOT preflight the pull window.
func (c *VillageClient) GetPullTranscriptContent(ctx context.Context, id schema.TranscriptID, ifNoneMatch string) (io.ReadCloser, string, error) {
	path := pullTranscriptContentPath(id)
	req, err := c.newAuthedGet(ctx, c.baseURL+path)
	if err != nil {
		return nil, "", err
	}
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("execute pull-content request: %w", err)
	}

	switch resp.StatusCode {
	case http.StatusNotModified:
		// Conditional GET hit: blob is current. Drain+close; signal via sentinel.
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, resp.Header.Get("ETag"), ErrNotModified
	case http.StatusOK:
		// Caller owns and must Close resp.Body.
		return resp.Body, resp.Header.Get("ETag"), nil
	default:
		err := pullStatusError(fmt.Sprintf("get pull content for transcript %s", id), path, resp)
		_ = resp.Body.Close()
		return nil, "", err
	}
}

// GetPullTranscriptAnnotations fetches the authored annotations for a transcript
// from the pull surface — PullAnnotation rows carrying the village account
// identity (AuthorUserID/AuthorUsername) the bare AnnotationSummary lacks, so the
// caller can foreign-mark them and exclude its own authored rows during refresh.
// A non-2xx status is an actionable error. The caller runs NegotiatePull once
// before the FETCH stages — this method does NOT preflight the pull window.
func (c *VillageClient) GetPullTranscriptAnnotations(ctx context.Context, id schema.TranscriptID) ([]schema.PullAnnotation, error) {
	path := pullTranscriptAnnotationsPath(id)
	req, err := c.newAuthedGet(ctx, c.baseURL+path)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("execute pull-annotations request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, pullStatusError(fmt.Sprintf("get pull annotations for transcript %s", id), path, resp)
	}

	var result []schema.PullAnnotation
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode pull-annotations response: %w", err)
	}
	return result, nil
}

// newAuthedGet builds an authed GET request (Bearer apiKey) against the given
// full URL. Shared by all four pull GETs to keep auth/header handling in one place.
func (c *VillageClient) newAuthedGet(ctx context.Context, url string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("create pull request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	return req, nil
}

// pullStatusError formats an actionable error for a non-2xx pull response. It
// names the operation, the endpoint path, the status code, and the (truncated)
// server body, and gives status-specific remediation for the common 401/404
// cases so the caller can act without inspecting the raw response.
func pullStatusError(op, path string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		return fmt.Errorf(
			"%s: village returned 401 Unauthorized\n"+
				"  what: the pull request to %s was rejected as unauthenticated\n"+
				"  why:  your credentials are missing, expired, or invalid for this village\n"+
				"  fix:  re-authenticate with `peasant village login`, then re-run the pull",
			op, path)
	case http.StatusNotFound:
		return fmt.Errorf(
			"%s: village returned 404 Not Found\n"+
				"  what: %s is not pullable for your account\n"+
				"  why:  the transcript does not exist, or is not owned by or group-shared with you (public transcripts are not pullable)\n"+
				"  fix:  verify the transcript ID/URL, or ask the owner to share it with a group you belong to\n"+
				"  (%w)",
			op, path, ErrPullNotFound)
	default:
		return fmt.Errorf("%s: village returned %d for %s: %s", op, resp.StatusCode, path, string(body))
	}
}
