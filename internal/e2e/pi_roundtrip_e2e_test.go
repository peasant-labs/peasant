//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/peasant-labs/peasant/internal/api"
	"github.com/peasant-labs/peasant/internal/export"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/pull"
	"github.com/peasant-labs/peasant/internal/sessionvisibility"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/transcript"
	"github.com/peasant-labs/schema"
)

// This test intentionally builds only the backend CLI via the ordinary E2E
// helper. It does not assert that the embedded web bundle renders Pi.
func TestPiRoundTripE2E(t *testing.T) {
	bins := resolveVillageBinaries(t)
	stack := provisionHarnessStack(t, bins)
	if stack.external {
		t.Fatal("Pi encrypted roundtrip requires a disposable harness-owned database and bucket; unset external-stack variables before rerunning")
	}
	binary := buildPeasant(t)
	for _, fixture := range loadPiRoundTripCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			sandbox := newDisposableSandbox(t, binary)
			proxy, requests := newPiRecordingProxy(t, stack.villageURL)
			apiKey := mintDemoCredentials(t, bins.setupDemo, stack.dsn, proxy.URL, sandbox.configHome)
			owner := readDemoUserID(t, sandbox.configHome)
			source := strings.ReplaceAll(piRoundTripNativeSource(t, fixture.NativeCase), "pi-native-fixture", fixture.SessionID)
			for _, replacement := range fixture.SourceReplacements {
				piEqual(t, 1, strings.Count(source, replacement.From), "native usage variant must modify exactly one source owner")
				source = strings.Replace(source, replacement.From, replacement.To, 1)
			}
			beforeTranscripts := villageTableCount(t, stack.db, "transcripts")
			beforeBlobs := transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket)
			sourcePath := filepath.Join(sandbox.root, "native.jsonl")
			piNoError(t, os.WriteFile(sourcePath, []byte(source), 0600))
			writeDisposableSandboxConfig(t, sandbox, fmt.Sprintf(`version: 1
sources:
  claude-code: {enabled: false}
  opencode: {enabled: false}
  codex: {enabled: false}
  cursor: {enabled: false}
  strike: {enabled: false}
  pi:
    enabled: true
    paths: [%q]
output:
  basePath: %q
push:
  visibility: private
`, sourcePath, sandbox.transcriptOutputPath()))
			runPeasantInSandbox(t, binary, sandbox, "harvest", "--include-active", "--json")
			local := piLocalDetail(t, sandbox, fixture.SessionID)
			assertPiRoundTripDetail(t, fixture, local)
			before := requests.snapshot()
			dry := runPeasantInSandbox(t, binary, sandbox, "village", "push", "--dry-run", "--json", "--non-interactive")
			var forecast pushJSON
			piNoError(t, json.Unmarshal([]byte(stdoutJSON(dry)), &forecast))
			piEqual(t, 0, forecast.Errors, dry)
			piEqual(t, 1, forecast.New, "dry run must forecast the actual ingested Pi session")
			piEqual(t, before, requests.snapshot(), "dry run must make zero requests, including capability negotiation and upload")
			piEqual(t, beforeTranscripts, villageTableCount(t, stack.db, "transcripts"))
			piEqual(t, beforeBlobs, transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket))

			result := runPeasantInSandbox(t, binary, sandbox, "village", "push", "--json", "--non-interactive")
			var pushed pushJSON
			piNoError(t, json.Unmarshal([]byte(stdoutJSON(result)), &pushed))
			piEqual(t, 1, pushed.New)
			piEqual(t, 0, pushed.Errors)
			piEqual(t, 0, pushed.Held)
			piEqual(t, 0, pushed.Annotations.Errors)
			var uploads []piRecordedRequest
			negotiated := false
			for _, request := range requests.snapshot() {
				if request.Path == "/api/v1/schema/version" {
					negotiated = true
				}
				if request.Method == http.MethodPost && request.Path == "/api/v1/transcripts/publish" {
					piCheck(t, negotiated, "capability negotiation must precede upload")
					uploads = append(uploads, request)
				}
			}
			piEqual(t, 1, len(uploads))
			metadata, content := piRecordedMultipart(t, uploads[0])
			outbound, err := schema.DecodeTranscriptContentRaw(content)
			piNoError(t, err)
			piCheck(t, outbound.SessionDetail != nil, "outbound detail required")
			assertPiRoundTripDetail(t, fixture, outbound.SessionDetail)
			piEqual(t, local.Turns, outbound.SessionDetail.Turns, "safe native content must survive actual publish redaction/projection")
			piEqual(t, local.NativeMetadata, outbound.SessionDetail.NativeMetadata)
			authoritative, err := schema.DecodeAuthoritativePublishMetadataRaw(metadata)
			piNoError(t, err)
			for _, entry := range authoritative.Entries {
				piCheck(t, entry.Extra == nil, "private native evidence must not escape in the metadata part")
			}
			for _, forbidden := range fixture.Forbidden {
				piCheck(t, !bytes.Contains(content, []byte(forbidden)), "private content escaped transcript part: "+forbidden)
				piCheck(t, !bytes.Contains(metadata, []byte(forbidden)), "private content escaped metadata part: "+forbidden)
			}
			remote := villageTranscriptByLocalID(t, listVillageTranscripts(t, stack.db, owner), fixture.SessionID)
			stored := readLegacyStorageSnapshot(t, stack, remote.ID)
			assertPiCiphertext(t, stack, stored, content)
			served := piReadVillageDetail(t, proxy.URL, apiKey, remote.ID)
			piEqual(t, outbound.SessionDetail.Turns, served.Turns)
			piEqual(t, outbound.SessionDetail.NativeMetadata, served.NativeMetadata)

			// Republish the real producer's detail in the supported bare-payload
			// compatibility shape. Village encrypts these bytes itself; its first
			// read must then rewrite them to the canonical envelope. Changing only
			// a database version marker would not exercise this shape-based path.
			bare, err := json.Marshal(outbound.SessionDetail)
			piNoError(t, err)
			authoritative.ContentHash = schema.ComputeTranscriptContentHash(bare)
			piNoError(t, schema.ValidatePublicationRequest(authoritative))
			bareMetadata, err := json.Marshal(authoritative)
			piNoError(t, err)
			status, response := directPublish(t, proxy.URL, apiKey, bareMetadata, bare)
			piCheck(t, status >= 200 && status < 300, "bare producer detail republish rejected: "+response)
			stored = readLegacyStorageSnapshot(t, stack, remote.ID)
			assertPiCiphertext(t, stack, stored, bare)
			rewritten := piReadVillageDetail(t, proxy.URL, apiKey, remote.ID)
			piEqual(t, served.Turns, rewritten.Turns)
			piEqual(t, served.NativeMetadata, rewritten.NativeMetadata)
			after := readLegacyStorageSnapshot(t, stack, remote.ID)
			piCheck(t, stored.blobKey != after.blobKey, "migration must commit a newly encrypted canonical object")
			assertPiCiphertext(t, stack, after, content)
			piEqual(t, rewritten, piReadVillageDetail(t, proxy.URL, apiKey, remote.ID))
			piEqual(t, after.blobKey, readLegacyStorageSnapshot(t, stack, remote.ID).blobKey, "canonical reads must not rewrite again")

			pulled := runPeasantPullJSON(t, binary, sandbox.environment, remote.ID)
			piEqual(t, "", pulled.Error)
			piCheck(t, pulled.PullDir != "", "pull must persist a local artifact")
			pulledRaw, err := os.ReadFile(filepath.Join(pulled.PullDir, pull.TranscriptFilename))
			piNoError(t, err)
			pulledContent, err := schema.DecodeTranscriptContentRaw(pulledRaw)
			piNoError(t, err)
			piCheck(t, pulledContent.SessionDetail != nil, "pull must retain detail envelope")
			piEqual(t, rewritten.Turns, pulledContent.SessionDetail.Turns)
			piEqual(t, rewritten.NativeMetadata, pulledContent.SessionDetail.NativeMetadata)
			for _, invalid := range fixture.InvalidContent {
				t.Run(invalid.Name, func(t *testing.T) {
					replacement := invalid.Replacement
					if invalid.Repeat > 0 {
						replacement = strings.ReplaceAll(replacement, "$REPEAT", strings.Repeat("x", invalid.Repeat))
					}
					piCheck(t, bytes.Contains(content, []byte(invalid.Find)), "invalid fixture must mutate actual producer output")
					bad := bytes.Replace(content, []byte(invalid.Find), []byte(replacement), 1)
					authoritative.ContentHash = schema.ComputeTranscriptContentHash(bad)
					badMetadata, err := json.Marshal(authoritative)
					piNoError(t, err)
					beforeRow := readLegacyStorageSnapshot(t, stack, remote.ID)
					beforeAudit := villageTableCount(t, stack.db, "transcript_governance_events_audit")
					beforeObjects := transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket)
					status, _ := directPublish(t, proxy.URL, apiKey, badMetadata, bad)
					piEqual(t, invalid.Status, status)
					piEqual(t, beforeRow, readLegacyStorageSnapshot(t, stack, remote.ID), "invalid content must not mutate the transcript or encryption descriptor")
					piEqual(t, beforeAudit, villageTableCount(t, stack.db, "transcript_governance_events_audit"))
					piEqual(t, beforeObjects, transcriptBucketObjectCount(t, stack.minioEndpoint, stack.bucket))
				})
			}
			original, err := os.ReadFile(sourcePath)
			piNoError(t, err)
			piEqual(t, source, string(original), "harvest/publish/pull must not mutate the original native fixture")
		})
	}
}

func piLocalDetail(t *testing.T, sandbox disposableSandbox, sessionID string) *schema.SessionDetailPayload {
	t.Helper()
	db, err := store.Open(filepath.Join(sandbox.dataHome, "peasant", "peasant.db"))
	piNoError(t, err)
	defer db.Close()
	session, err := api.NewStoreDataProvider(db, sessionvisibility.All()).SessionByID(t.Context(), sessionID)
	piNoError(t, err)
	detail, err := transcript.SessionToDetailValidated(session)
	piNoError(t, err)
	exported, err := export.ExportSession(t.Context(), db, &ingest.OSFileSystem{}, sessionID)
	piNoError(t, err)
	piEqual(t, detail.Turns, exported.Turns)
	piEqual(t, detail.NativeMetadata, exported.NativeMetadata)
	return detail
}

func assertPiRoundTripDetail(t *testing.T, fixture piRoundTripCase, detail *schema.SessionDetailPayload) {
	t.Helper()
	piEqual(t, schema.HarnessPi, detail.Harness)
	piNoError(t, schema.ValidateSessionDetailPayload(*detail))
	piEqual(t, fixture.Capabilities, schema.RequiredContentCapabilities(*detail))
	turns, err := json.Marshal(detail.Turns)
	piNoError(t, err)
	for _, text := range fixture.Contents {
		piCheck(t, bytes.Contains(turns, []byte(text)), "missing visible content: "+text)
	}
	piEqual(t, 1, strings.Count(string(turns), "think once"))
	piEqual(t, fixture.Placeholders, strings.Count(string(turns), "[image omitted]"))
	piCheck(t, !bytes.Contains(turns, []byte(fixture.MetadataOnly)), "opaque state must not become conversation")
	piEqual(t, fixture.Metadata, len(detail.NativeMetadata))
	metadata, err := json.Marshal(detail.NativeMetadata)
	piNoError(t, err)
	piCheck(t, bytes.Contains(metadata, []byte(fixture.MetadataOnly)), "opaque state must survive separately")
	owners := make(map[string]schema.UsageDetail)
	for _, turn := range detail.Turns {
		if turn.Usage != nil {
			owners[turn.Usage.SourceEntryRef] = *turn.Usage
			if turn.Usage.Scope == schema.UsageScopeAssistant {
				piCheck(t, turn.Usage.Cost != nil && turn.Usage.Cost.Total != nil, "recorded cost required")
				piEqual(t, fixture.AssistantCost, string(*turn.Usage.Cost.Total))
				raw, err := json.Marshal(turn.Usage.Tokens)
				piNoError(t, err)
				var tokens map[string]int64
				piNoError(t, json.Unmarshal(raw, &tokens))
				piEqual(t, fixture.AssistantTokens, tokens)
			}
		}
		for _, tool := range turn.ToolCalls {
			if tool.Usage != nil {
				owners[tool.Usage.SourceEntryRef] = *tool.Usage
				piEqual(t, tool.ResultEntryRef, tool.Usage.SourceEntryRef)
			}
		}
	}
	piEqual(t, len(fixture.Owners), len(owners))
	for _, expected := range fixture.Owners {
		ref := ingest.PiPublicRef(fixture.SessionID, "entry", expected.NativeID)
		owner, ok := owners[ref]
		piCheck(t, ok, "native usage owner must survive: "+expected.NativeID)
		piEqual(t, ingest.PiPublicRef(fixture.SessionID, "owner", expected.NativeID), string(owner.OwnerID))
		piEqual(t, expected.Scope, owner.Scope)
		piEqual(t, expected.Completeness, owner.Completeness)
	}
}

func piReadVillageDetail(t *testing.T, base, key, id string) *schema.SessionDetailPayload {
	t.Helper()
	status, raw := villageAPIRequest(t, http.MethodGet, base, "/api/v1/transcripts/"+id+"/content", key, nil)
	piEqual(t, http.StatusOK, status, "Village content read: "+string(raw))
	detail, err := schema.DecodeSessionDetailPayloadRaw(raw)
	piNoError(t, err)
	return &detail
}

func assertPiCiphertext(t *testing.T, stack harnessStack, stored legacyStorageSnapshot, plaintext []byte) {
	t.Helper()
	piCheck(t, len(stored.wrappedDataKey) > 0 && stored.keyVersion > 0, "encryption descriptor required")
	piEqual(t, "aes-256-gcm-random-nonce-v1", stored.encryptionAlgorithm)
	client, err := newMinioClient(stack.minioEndpoint)
	piNoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), s3OpTimeout)
	defer cancel()
	object, err := client.GetObject(ctx, stack.bucket, stored.blobKey, minio.GetObjectOptions{})
	piNoError(t, err)
	defer object.Close()
	ciphertext, err := io.ReadAll(object)
	piNoError(t, err)
	piCheck(t, len(ciphertext) > 0 && !bytes.Equal(plaintext, ciphertext), "encrypted object required")
	piCheck(t, !bytes.Contains(ciphertext, []byte("think once")), "plaintext probe must not appear in object")
	var value any
	piCheck(t, json.Unmarshal(ciphertext, &value) != nil, "persisted object must not be plaintext JSON")
}

type piRecordedRequest struct {
	Method, Path, ContentType string
	Body                      []byte
}

type piRequestRecorder struct {
	mu       sync.Mutex
	requests []piRecordedRequest
}

func (recorder *piRequestRecorder) snapshot() []piRecordedRequest {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]piRecordedRequest(nil), recorder.requests...)
}

// The proxy records transport but forwards to real Village unchanged. It never
// supplies a synthetic capability response, validation result, or persistence.
func newPiRecordingProxy(t *testing.T, target string) (*httptest.Server, *piRequestRecorder) {
	t.Helper()
	u, err := url.Parse(target)
	piNoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(u)
	recorder := &piRequestRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request := piRecordedRequest{Method: r.Method, Path: r.URL.Path, ContentType: r.Header.Get("Content-Type")}
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/transcripts/publish" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "E2E proxy could not record upload; retry with a readable request body", http.StatusBadGateway)
				return
			}
			r.Body.Close()
			r.Body = io.NopCloser(bytes.NewReader(body))
			request.Body = body
		}
		recorder.mu.Lock()
		recorder.requests = append(recorder.requests, request)
		recorder.mu.Unlock()
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, recorder
}

func piRecordedMultipart(t *testing.T, request piRecordedRequest) ([]byte, []byte) {
	t.Helper()
	mediaType, params, err := mime.ParseMediaType(request.ContentType)
	piNoError(t, err)
	piEqual(t, "multipart/form-data", mediaType)
	reader := multipart.NewReader(bytes.NewReader(request.Body), params["boundary"])
	parts := make(map[string][]byte)
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		piNoError(t, err)
		_, duplicate := parts[part.FormName()]
		piCheck(t, !duplicate, "multipart parts must be unique")
		parts[part.FormName()], err = io.ReadAll(part)
		piNoError(t, err)
		piNoError(t, part.Close())
	}
	piCheck(t, len(parts["metadata"]) > 0 && len(parts["transcript_file"]) > 0, "both multipart surfaces required")
	return parts["metadata"], parts["transcript_file"]
}
