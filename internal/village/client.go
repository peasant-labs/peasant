// Package village implements the peasant↔village HTTP client (both push and pull
// directions) plus the version-negotiation helpers shared by the push and pull
// pipelines. The client lives here — rather than in internal/push — so the pull
// pipeline (internal/pull) can depend on it without inverting the dependency
// (pull -> push).
package village

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/schema"
)

// traceContext attaches the perf upload-timing httptrace hooks to ctx when (and
// only when) the caller threaded an UploadTrace onto it (i.e. --timing is on).
// When timing is off the context is returned unchanged, so an un-timed request
// pays no instrumentation cost. This is the single client-side seam for the
// connection-setup / server-processing split.
func traceContext(ctx context.Context) context.Context {
	if trace := perf.UploadTraceFromContext(ctx); trace != nil {
		return httptrace.WithClientTrace(ctx, trace.ClientTrace())
	}
	return ctx
}

// DefaultClientTimeout is the HTTP client timeout for individual upload requests.
const DefaultClientTimeout = 60 * time.Second

// DefaultPoolSize is the default connection-pool size for a VillageClient built
// without an injected *http.Client. It mirrors push.DefaultConcurrency (the
// default upload parallelism) — the same value is the right default pool size —
// but is owned here so the client has no dependency on the push pipeline.
const DefaultPoolSize = 5

// publishEndpoint is the village API path for transcript uploads.
const publishEndpoint = "/api/v1/transcripts/publish"

const transcriptEndpoint = "/api/v1/transcripts/"

type PublicationStage string

const (
	PublicationStagePublishDecode PublicationStage = "publish-response-decode"
	PublicationStageOwnerDecode   PublicationStage = "owner-update-response-decode"
)

type PublicationError struct {
	Stage      PublicationStage
	StatusCode int
	Err        error
}

func (e *PublicationError) Error() string {
	return fmt.Sprintf("publication failed at %s after HTTP %d; no local applied state was changed and retry remains targeted to this session; fix the Village deployment or response contract, then retry: %v", e.Stage, e.StatusCode, e.Err)
}
func (e *PublicationError) Unwrap() error { return e.Err }

// annotationsEndpoint is the village API path for annotation uploads.
const annotationsEndpoint = "/api/v1/annotations"

// schemaVersionEndpoint is the village API path for querying supported schema versions.
const schemaVersionEndpoint = "/api/v1/schema/version"

// annotationManifestEndpoint is the village API path for the owner's annotation
// content-hash manifest, which is the server-authoritative skip-gate source.
const annotationManifestEndpoint = "/api/v1/annotations/manifest"

// VillageClient uploads legacy and authoritative transcript requests to the
// Peasant village via multipart/form-data POST.
type VillageClient struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	// beforeRequest is set before the client is used and called immediately
	// before every push-side HTTP attempt. The push CLI uses it to distinguish
	// failures that happened wholly locally from runs that may have reached the village.
	beforeRequest func()
}

// NewVillageClient creates a VillageClient targeting the given base URL with the
// provided API key. If httpClient is nil, a client backed by a shared pooled
// transport sized to DefaultPoolSize is built (see newPooledHTTPClient); pass
// a non-nil httpClient to inject one (tests). To size the pool to a specific
// upload concurrency, use NewVillageClientWithConcurrency.
func NewVillageClient(baseURL, apiKey string, httpClient *http.Client) *VillageClient {
	if httpClient == nil {
		httpClient = newPooledHTTPClient(baseURL, DefaultPoolSize)
	}
	return &VillageClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

// NewVillageClientWithConcurrency creates a VillageClient whose shared transport
// is sized to `concurrency`: MaxIdleConnsPerHost and
// MaxConnsPerHost both equal concurrency, so reruns reuse pooled connections
// (no fresh TLS handshake per upload) and in-flight uploads are not throttled
// below the requested parallelism. This is the constructor the CLI uses with the
// resolved --concurrency value.
func NewVillageClientWithConcurrency(baseURL, apiKey string, concurrency int) *VillageClient {
	return &VillageClient{
		baseURL:    baseURL,
		apiKey:     apiKey,
		httpClient: newPooledHTTPClient(baseURL, concurrency),
	}
}

// SetRequestObserver registers a callback invoked immediately before each
// push-side HTTP attempt. It must be configured before the client is used
// concurrently.
func (c *VillageClient) SetRequestObserver(observer func()) {
	c.beforeRequest = observer
}

func (c *VillageClient) do(req *http.Request, operation clientOperation) (*http.Response, error) {
	if c.beforeRequest != nil {
		c.beforeRequest()
	}
	rec := perf.RecorderFromContext(req.Context())
	if !rec.Enabled() {
		return c.httpClient.Do(req)
	}
	// A private client copy preserves configured redirects, cookies and timeouts.
	// The wrapper observes each actual RoundTrip, including redirected requests,
	// without changing the shared client used concurrently by other sessions.
	client := *c.httpClient
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	client.Transport = &profileTransport{base: transport, rec: rec, operation: operation}
	return client.Do(req)
}

// newPooledHTTPClient builds an *http.Client backed by ONE shared *http.Transport
// MaxIdleConnsPerHost = MaxConnsPerHost = concurrency (floored at
// 1), ForceAttemptHTTP2 for multiplexing, and — preserved from the original
// behaviour — TLS verification skipped only when the village is on localhost (for
// local dev against a self-signed cert).
func newPooledHTTPClient(baseURL string, concurrency int) *http.Client {
	concurrency = max(concurrency, 1)
	transport := &http.Transport{
		MaxIdleConns:        concurrency,
		MaxIdleConnsPerHost: concurrency,
		MaxConnsPerHost:     concurrency,
		ForceAttemptHTTP2:   true,
	}
	// For local development, allow insecure TLS if the village is on localhost.
	if u, err := url.Parse(baseURL); err == nil {
		if u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" {
			transport.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		}
	}
	return &http.Client{Timeout: DefaultClientTimeout, Transport: transport}
}

// Publish uploads metadataJSON and transcriptBody to the village.
//
// It builds a multipart/form-data request with two parts:
//   - "metadata": the JSON payload as a plain text field
//   - "transcript_file": the binary transcript body with the given filename
//
// Returns the server's PublishResult, the HTTP status code, and any error.
// A non-2xx status code causes an error.
func (c *VillageClient) Publish(
	ctx context.Context,
	metadataJSON []byte,
	transcriptBody io.Reader,
	filename string,
) (*ingest.PublishResult, int, error) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)

	// Part 1: metadata JSON as a plain field.
	metaWriter, err := mw.CreateFormField("metadata")
	if err != nil {
		return nil, 0, fmt.Errorf("create metadata field: %w", err)
	}
	if _, err := metaWriter.Write(metadataJSON); err != nil {
		return nil, 0, fmt.Errorf("write metadata field: %w", err)
	}

	// Part 2: transcript file as a binary file upload.
	fileWriter, err := mw.CreateFormFile("transcript_file", filename)
	if err != nil {
		return nil, 0, fmt.Errorf("create transcript_file field: %w", err)
	}
	if _, err := io.Copy(fileWriter, transcriptBody); err != nil {
		return nil, 0, fmt.Errorf("write transcript_file field: %w", err)
	}

	if err := mw.Close(); err != nil {
		return nil, 0, fmt.Errorf("close multipart writer: %w", err)
	}

	endpoint := c.baseURL + publishEndpoint
	req, err := http.NewRequestWithContext(traceContext(ctx), http.MethodPost, endpoint, &body)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.do(req, operationPublish)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, statusCode, fmt.Errorf("village returned %d: %s", statusCode, string(respBody))
	}

	var result ingest.PublishResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		// Non-fatal: server did not return a parseable body.
		profileResponseFailure(ctx, operationPublish)
		// Return the status code but no result.
		return nil, statusCode, nil
	}

	return &result, statusCode, nil
}

// PublishAuthoritative uploads a successor publication request and accepts only
// a complete, schema-validated terminal receipt. A 2xx response with missing,
// duplicate, unknown, or inconsistent fields is an error, not success.
func (c *VillageClient) PublishAuthoritative(ctx context.Context, request schema.AuthoritativePublishRequest, transcriptBody io.Reader, filename string) (schema.AuthoritativePublishResponse, int, error) {
	metadataJSON, err := json.Marshal(request)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: encode schema request before transport: %w", err)
	}
	if _, err = schema.DecodeAuthoritativePublishRequest(metadataJSON); err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: validate schema request before transport: %w", err)
	}
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	metaWriter, err := mw.CreateFormField("metadata")
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: create metadata part: %w", err)
	}
	if _, err = metaWriter.Write(metadataJSON); err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: write metadata part: %w", err)
	}
	fileWriter, err := mw.CreateFormFile("transcript_file", filename)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: create transcript part: %w", err)
	}
	if _, err = io.Copy(fileWriter, transcriptBody); err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: write transcript part: %w", err)
	}
	if err = mw.Close(); err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: finalize multipart request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(traceContext(ctx), http.MethodPost, c.baseURL+publishEndpoint, &body)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", mw.FormDataContentType())
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq, operationPublish)
	if err != nil {
		return schema.AuthoritativePublishResponse{}, 0, fmt.Errorf("authoritative publish: execute HTTP request: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		profileResponseFailure(ctx, operationPublish)
		return schema.AuthoritativePublishResponse{}, resp.StatusCode, fmt.Errorf("authoritative publish: read terminal response: %w", readErr)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return schema.AuthoritativePublishResponse{}, resp.StatusCode, fmt.Errorf("authoritative publish: village returned %d: %s", resp.StatusCode, string(raw))
	}
	receipt, err := schema.DecodePublishResponse(raw)
	if err != nil {
		profileResponseFailure(ctx, operationPublish)
		return schema.AuthoritativePublishResponse{}, resp.StatusCode, &PublicationError{Stage: PublicationStagePublishDecode, StatusCode: resp.StatusCode, Err: fmt.Errorf("authoritative publish: village returned malformed 2xx receipt: %w", err)}
	}
	return receipt, resp.StatusCode, nil
}

// UpdateOwner applies an owner PATCH and accepts only the complete authoritative response.
func (c *VillageClient) UpdateOwner(ctx context.Context, id schema.TranscriptID, request schema.OwnerTranscriptUpdateRequest) (schema.OwnerTranscriptUpdateResponse, int, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return schema.OwnerTranscriptUpdateResponse{}, 0, fmt.Errorf("owner update: encode schema request: %w", err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPatch, c.baseURL+transcriptEndpoint+url.PathEscape(id.String()), bytes.NewReader(body))
	if err != nil {
		return schema.OwnerTranscriptUpdateResponse{}, 0, fmt.Errorf("owner update: create HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.do(httpReq, operationVisibility)
	if err != nil {
		return schema.OwnerTranscriptUpdateResponse{}, 0, fmt.Errorf("owner update: execute HTTP request: %w", err)
	}
	defer resp.Body.Close()
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if readErr != nil {
		profileResponseFailure(ctx, operationVisibility)
		return schema.OwnerTranscriptUpdateResponse{}, resp.StatusCode, fmt.Errorf("owner update: read terminal response: %w", readErr)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return schema.OwnerTranscriptUpdateResponse{}, resp.StatusCode, fmt.Errorf("owner update: village returned %d: %s", resp.StatusCode, string(raw))
	}
	var result schema.OwnerTranscriptUpdateResponse
	if err = json.Unmarshal(raw, &result); err != nil {
		profileResponseFailure(ctx, operationVisibility)
		return schema.OwnerTranscriptUpdateResponse{}, resp.StatusCode, &PublicationError{Stage: PublicationStageOwnerDecode, StatusCode: resp.StatusCode, Err: fmt.Errorf("owner update: village returned malformed 2xx response: %w", err)}
	}
	return result, resp.StatusCode, nil
}

// UploadAnnotations sends an AnnotationPushRequest to the village and returns
// the server's AnnotationPushResponse and HTTP status code.
// A non-2xx status code causes an error.
func (c *VillageClient) UploadAnnotations(
	ctx context.Context,
	req schema.AnnotationPushRequest,
) (*schema.AnnotationPushResponse, int, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, 0, fmt.Errorf("marshal annotation push request: %w", err)
	}
	if err := schema.ValidateAnnotationPushRequest(body); err != nil {
		return nil, 0, fmt.Errorf("validate annotation push request against the published schema before transmission: %w", err)
	}

	endpoint := c.baseURL + annotationsEndpoint
	httpReq, err := http.NewRequestWithContext(traceContext(ctx), http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.do(httpReq, operationAnnotations)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode

	if statusCode != http.StatusOK && statusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, statusCode, fmt.Errorf("village returned %d: %s", statusCode, string(respBody))
	}

	var result schema.AnnotationPushResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		profileResponseFailure(ctx, operationAnnotations)
		// Non-fatal: server did not return a parseable body.
		return nil, statusCode, nil
	}

	return &result, statusCode, nil
}

// GetSchemaVersion queries the village for the current annotation schema version
// and returns the response. A non-2xx status code causes an error.
func (c *VillageClient) GetSchemaVersion(ctx context.Context) (*schema.SchemaVersionResponse, int, error) {
	endpoint := c.baseURL + schemaVersionEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.do(httpReq, operationNegotiate)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode

	if statusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, statusCode, fmt.Errorf("village returned %d: %s", statusCode, string(respBody))
	}

	var result schema.SchemaVersionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		profileResponseFailure(ctx, operationNegotiate)
		return nil, statusCode, fmt.Errorf("decode schema version response: %w", err)
	}

	return &result, statusCode, nil
}

// GetAnnotationManifest fetches the owner's annotation content-hash manifest from
// the village - the set of annotation hashes the server already
// holds, used as the server-authoritative skip-gate. It mirrors GetSchemaVersion:
// an authed GET, returning the decoded response, the HTTP status code, and any
// error. A non-2xx status (including a 404 from a village predating the endpoint)
// returns an error WITH the status code so the caller can fail safe (push all);
// a transport error returns status 0. The caller MUST treat any error as
// "manifest unavailable ⇒ push everything", never as "skip everything".
func (c *VillageClient) GetAnnotationManifest(ctx context.Context) (*schema.AnnotationManifestResponse, int, error) {
	endpoint := c.baseURL + annotationManifestEndpoint
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.do(httpReq, operationAnnotationManifest)
	if err != nil {
		return nil, 0, fmt.Errorf("execute request: %w", err)
	}
	defer resp.Body.Close()

	statusCode := resp.StatusCode

	if statusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, statusCode, fmt.Errorf("village returned %d: %s", statusCode, string(respBody))
	}

	var result schema.AnnotationManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		profileResponseFailure(ctx, operationAnnotationManifest)
		return nil, statusCode, fmt.Errorf("decode annotation manifest response: %w", err)
	}

	return &result, statusCode, nil
}
