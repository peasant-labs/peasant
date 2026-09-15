package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/sessionorigin"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/session_graph_publish.yaml
var sessionGraphPublishFixtureYAML []byte

//go:embed testdata/session_graph_publish.manifest.yaml
var sessionGraphPublishManifestYAML []byte

// Closed sets for the fixture. A new arm, payload shape, or transport must be
// added here and implemented in the test, so an unknown value fails loudly
// instead of being silently skipped.
var (
	sessionGraphPublishArms       = []string{"pipeline", "preflight"}
	sessionGraphPublishPayloads   = []string{"graph", "plain", "count-only", "root-only", "purpose-only", "provenance", "source-ref-only"}
	sessionGraphPublishTransports = []string{"stub", "unavailable", "null-advertisement"}
)

type sessionGraphPublishCase struct {
	Name             string                     `yaml:"name"`
	Arm              string                     `yaml:"arm"`
	Payload          string                     `yaml:"payload"`
	DryRun           bool                       `yaml:"dryRun"`
	ScanFirst        bool                       `yaml:"scanFirst"`
	Transport        string                     `yaml:"transport"`
	Advertisement    []schema.ContentCapability `yaml:"advertisement"`
	WantRequired     []schema.ContentCapability `yaml:"wantRequired"`
	WantMissing      []schema.ContentCapability `yaml:"wantMissing"`
	WantUploads      int                        `yaml:"wantUploads"`
	WantError        bool                       `yaml:"wantError"`
	WantNetworkCalls int                        `yaml:"wantNetworkCalls"`
	WantSaved        int                        `yaml:"wantSavedPublications"`
	WantAttempts     int                        `yaml:"wantAttempts"`
	// WantLegacyEnvelope pins that a store without the durable snapshot surface
	// publishes the preserved entries-built envelope: the graph token is derived
	// from the entry provenance alone, with no snapshot-derived session members.
	WantLegacyEnvelope bool `yaml:"wantLegacyEnvelope"`
}

type sessionGraphPublishFixture struct {
	Cases []sessionGraphPublishCase `yaml:"cases"`
}

func loadSessionGraphPublishFixture(t *testing.T) sessionGraphPublishFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(sessionGraphPublishFixtureYAML))
	decoder.KnownFields(true)
	var fixture sessionGraphPublishFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode session graph publish fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("session graph publish fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionGraphPublishManifestYAML, "session graph publish")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(fixture.Cases))
	for index, fixtureCase := range fixture.Cases {
		names[index] = fixtureCase.Name
		if fixtureCase.Name == "" {
			t.Fatalf("session graph publish fixture case %d has no name", index)
		}
		if !containsString(sessionGraphPublishArms, fixtureCase.Arm) {
			t.Fatalf("case %q has unknown arm %q", fixtureCase.Name, fixtureCase.Arm)
		}
		if !containsString(sessionGraphPublishPayloads, fixtureCase.Payload) {
			t.Fatalf("case %q has unknown payload shape %q", fixtureCase.Name, fixtureCase.Payload)
		}
		if fixtureCase.Arm == "pipeline" && !containsString(sessionGraphPublishTransports, fixtureCase.Transport) {
			t.Fatalf("case %q has unknown transport %q", fixtureCase.Name, fixtureCase.Transport)
		}
		if fixtureCase.WantUploads < 0 || fixtureCase.WantNetworkCalls < 0 {
			t.Fatalf("case %q declares a negative count", fixtureCase.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "session graph publish"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

func containsString(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

// TestSessionGraphPublishOfflineScanAndFreshNegotiation drives the REAL shared
// publish service for every pipeline-arm case: an offline dry-run scan that must
// make no network call, and a real publish that must fetch a fresh advertisement
// immediately before upload and refuse a requirement-bearing payload the
// receiver cannot preserve, with no upload, receipt, or attempt side effect.
func TestSessionGraphPublishOfflineScanAndFreshNegotiation(t *testing.T) {
	for _, fixtureCase := range loadSessionGraphPublishFixture(t).Cases {
		if fixtureCase.Arm != "pipeline" {
			continue
		}
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			store, fs := sessionGraphPublishStore(t, fixtureCase.Payload)

			if fixtureCase.ScanFirst {
				scanTransport := &testutil.StubPublisher{SchemaVersionErr: errors.New("offline scan must not reach the network")}
				scanStderr := &bytes.Buffer{}
				scanPipeline := newSessionGraphPipeline(t, store, scanTransport, fs, baseTestConfig(), push.PipelineConfig{DryRun: true, Concurrency: 1}, scanStderr)
				scanResult, err := scanPipeline.Run(context.Background())
				if err != nil {
					t.Fatalf("offline scan Run: %v", err)
				}
				if scanTransport.SchemaVersionCalls != 0 {
					t.Fatalf("offline scan made %d network call(s)", scanTransport.SchemaVersionCalls)
				}
				if len(scanTransport.Calls) != 0 {
					t.Fatalf("offline scan uploaded %d body(ies)", len(scanTransport.Calls))
				}
				requireCapabilities(t, scanResult.Sessions, fixtureCase.WantRequired)
			}

			transport, networkCalls := sessionGraphPublishTransport(t, fixtureCase)
			stderr := &bytes.Buffer{}
			pipeline := newSessionGraphPipeline(t, store, transport, fs, baseTestConfig(), push.PipelineConfig{DryRun: fixtureCase.DryRun, Concurrency: 1}, stderr)
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}

			if got := networkCalls(); got != fixtureCase.WantNetworkCalls {
				t.Fatalf("network calls=%d, want %d; result=%+v", got, fixtureCase.WantNetworkCalls, result)
			}
			requireCapabilities(t, result.Sessions, fixtureCase.WantRequired)
			requireUploads(t, transport, fixtureCase.WantUploads)
			if gotError := result.Errors > 0; gotError != fixtureCase.WantError {
				t.Fatalf("error=%t, want %t; result=%+v sessions=%+v", gotError, fixtureCase.WantError, result, result.Sessions)
			}
			if got := len(store.SavedPublicationIDs); got != fixtureCase.WantSaved {
				t.Fatalf("saved publications=%d, want %d", got, fixtureCase.WantSaved)
			}
			if got := len(store.PublicationAttempts); got != fixtureCase.WantAttempts {
				t.Fatalf("publication attempts=%d, want %d", got, fixtureCase.WantAttempts)
			}
			if fixtureCase.WantLegacyEnvelope {
				requireLegacyEnvelope(t, transport)
			}
		})
	}
}

// requireLegacyEnvelope pins the preserved entries-built envelope shape: the
// last upload carries no session-level snapshot members, and its turn count is
// the folded turn count rather than a durable generation mirror.
func requireLegacyEnvelope(t *testing.T, transport push.Transport) {
	t.Helper()
	pub, ok := transport.(*testutil.StubPublisher)
	if !ok || len(pub.Calls) == 0 {
		t.Fatalf("legacy envelope assertion needs a stub upload; transport=%T", transport)
	}
	var envelope schema.TranscriptContent
	if err := json.Unmarshal(pub.Calls[len(pub.Calls)-1].TranscriptBody, &envelope); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	detail := envelope.SessionDetail
	if detail == nil {
		t.Fatal("legacy envelope has no sessionDetail")
	}
	if detail.RootSessionID != nil || detail.Purpose != "" || len(detail.Relationships) != 0 ||
		detail.InputSubmissionCount != nil || len(detail.EarlierHistory) != 0 {
		t.Fatalf("legacy envelope carries snapshot-derived members: root=%v purpose=%q relationships=%d count=%v earlier=%d",
			detail.RootSessionID, detail.Purpose, len(detail.Relationships), detail.InputSubmissionCount, len(detail.EarlierHistory))
	}
	if detail.TurnCount != len(detail.Turns) {
		t.Fatalf("legacy turnCount=%d, want the folded turn count %d", detail.TurnCount, len(detail.Turns))
	}
}

// TestSessionGraphPublishCapabilityDerivation drives the REAL offline scan
// against durable payload shapes the pipeline cannot currently produce from
// indexed entries: a payload that only carries a count, a root, or a purpose
// still requires the graph token, while a lone source reference does not.
func TestSessionGraphPublishCapabilityDerivation(t *testing.T) {
	for _, fixtureCase := range loadSessionGraphPublishFixture(t).Cases {
		if fixtureCase.Arm != "preflight" {
			continue
		}
		fixtureCase := fixtureCase
		t.Run(fixtureCase.Name, func(t *testing.T) {
			content := push.BuildTranscriptContentFromDetail(sessionGraphPublishDetail(t, fixtureCase.Payload), defaults.PublishSchemaVersion, sessionorigin.Unknown)
			scan, err := push.ScanPublication(content)
			if err != nil {
				t.Fatalf("ScanPublication: %v", err)
			}
			if got, want := capabilityStrings(scan.Required), capabilityStrings(fixtureCase.WantRequired); !reflect.DeepEqual(got, want) {
				t.Fatalf("required=%v, want %v", got, want)
			}
			missing := push.CapabilityShortfall(fixtureCase.Advertisement, scan.Required)
			if got, want := capabilityStrings(missing), capabilityStrings(fixtureCase.WantMissing); !reflect.DeepEqual(got, want) {
				t.Fatalf("missing=%v, want %v", got, want)
			}
		})
	}
}

// TestSessionGraphPublishNullAdvertisementRefusedByRealTransport exercises the
// production HTTP client: a receiver that answers JSON null for
// contentCapabilities is malformed, so a graph-bearing payload is refused
// before any upload. The only request the receiver sees is the negotiation.
func TestSessionGraphPublishNullAdvertisementRefusedByRealTransport(t *testing.T) {
	var negotiationRequests, otherRequests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/schema/version" {
			otherRequests++
			http.NotFound(w, r)
			return
		}
		negotiationRequests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"annotationSchemaVersion":"1","pushContractVersion":%q,"minPushContractVersion":%q,"contentCapabilities":null}`, defaults.PublishSchemaVersion, defaults.PublishSchemaVersion)
	}))
	defer server.Close()

	store, fs := sessionGraphPublishStore(t, "graph")
	transport := village.NewVillageClient(server.URL, "test-key", server.Client())
	pipeline := newSessionGraphPipeline(t, store, transport, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &bytes.Buffer{})
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Errors != 1 || len(result.Sessions) != 1 || result.Sessions[0].Error == nil {
		t.Fatalf("null advertisement result=%+v, want one refusal", result)
	}
	if negotiationRequests != 1 {
		t.Fatalf("negotiation requests=%d, want exactly one fresh preflight", negotiationRequests)
	}
	if otherRequests != 0 {
		t.Fatalf("receiver saw %d non-negotiation request(s); a refusal must reach no upload", otherRequests)
	}
	if len(store.SavedPublicationIDs) != 0 || len(store.PublicationAttempts) != 0 {
		t.Fatalf("refusal persisted side effects: saved=%d attempts=%d", len(store.SavedPublicationIDs), len(store.PublicationAttempts))
	}
}

// TestSessionGraphPublishSupportedRepublishPreservesIdentity pins that an
// explicit republish and a forced retry of a supported graph payload keep ONE
// owner/local identity: the terminal receipt is keyed by the same
// owner/project/session tuple and reused rather than duplicated.
func TestSessionGraphPublishSupportedRepublishPreservesIdentity(t *testing.T) {
	store, fs := sessionGraphPublishStore(t, "graph")
	publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.0.1"),
		PushContractVersion:    defaults.PublishSchemaVersion,
		ContentCapabilities:    []schema.ContentCapability{schema.ContentCapabilitySessionGraphProvenanceV1},
	}}

	run := func(runCfg push.PipelineConfig) *push.PushResult {
		t.Helper()
		pipeline := newSessionGraphPipeline(t, store, publisher, fs, baseTestConfig(), runCfg, &bytes.Buffer{})
		result, err := pipeline.Run(context.Background())
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		return result
	}

	first := run(push.PipelineConfig{Concurrency: 1})
	if first.New != 1 || len(publisher.Calls) != 1 {
		t.Fatalf("first publish result=%+v uploads=%d, want one new upload", first, len(publisher.Calls))
	}
	pushedAt := int64(1740312500000)
	store.Sessions[0].PushedAt = &pushedAt

	second := run(push.PipelineConfig{Concurrency: 1})
	if second.Skipped != 1 || len(publisher.Calls) != 1 {
		t.Fatalf("unchanged republish result=%+v uploads=%d, want one skip without a second upload", second, len(publisher.Calls))
	}

	third := run(push.PipelineConfig{Concurrency: 1, Force: true})
	if len(publisher.Calls) != 2 || third.Errors != 0 {
		t.Fatalf("forced retry result=%+v uploads=%d, want one retry upload", third, len(publisher.Calls))
	}

	for _, id := range store.SavedPublicationIDs {
		if string(id) != testutil.TestSessionUUID {
			t.Fatalf("saved publication identity=%q, want the one owner/local id %q", id, testutil.TestSessionUUID)
		}
	}
	if len(store.Publications) != 1 {
		t.Fatalf("publication records=%d, want exactly one owner/local identity", len(store.Publications))
	}
}

// TestSessionGraphPublishSupportedUploadPreservesEvidence proves there is no
// provenance-stripping fallback: when the receiver advertises the required
// token, the uploaded body still carries the exact durable evidence. A refusal
// (covered by the fixture) is the only alternative to an evidence-preserving
// upload.
func TestSessionGraphPublishSupportedUploadPreservesEvidence(t *testing.T) {
	store, fs := sessionGraphPublishStore(t, "graph")
	publisher := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
		MinPushContractVersion: schema.PushContractVersion("0.0.1"),
		PushContractVersion:    defaults.PublishSchemaVersion,
		ContentCapabilities:    []schema.ContentCapability{schema.ContentCapabilitySessionGraphProvenanceV1},
	}}
	pipeline := newSessionGraphPipeline(t, store, publisher, fs, baseTestConfig(), push.PipelineConfig{Concurrency: 1}, &bytes.Buffer{})
	result, err := pipeline.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.New != 1 || len(publisher.Calls) != 1 {
		t.Fatalf("result=%+v uploads=%d, want one supported upload", result, len(publisher.Calls))
	}
	var envelope schema.TranscriptContent
	if err := json.Unmarshal(publisher.Calls[0].TranscriptBody, &envelope); err != nil {
		t.Fatalf("decode uploaded body: %v", err)
	}
	if envelope.SessionDetail == nil || len(envelope.SessionDetail.Turns) != 1 {
		t.Fatalf("uploaded detail=%+v, want one turn", envelope.SessionDetail)
	}
	turn := envelope.SessionDetail.Turns[0]
	if turn.SourceEntryRef != "e_u1" {
		t.Fatalf("uploaded sourceEntryRef=%q, want the exact durable ref e_u1", turn.SourceEntryRef)
	}
	if turn.Provenance == nil {
		t.Fatal("uploaded turn lost its provenance; evidence was stripped instead of uploaded whole")
	}
	if turn.Provenance.SubmissionRef != "s_u1" ||
		turn.Provenance.Origin != schema.ContentOriginSubmittedInput ||
		turn.Provenance.Delivery != schema.DeliveryOriginSessionAdmission ||
		turn.Provenance.Ownership != schema.ContentOwnershipLocal {
		t.Fatalf("uploaded provenance=%+v, want the exact durable evidence", turn.Provenance)
	}
}

// sessionGraphPublishStore seeds one publishable session whose indexed entries
// carry either graph provenance or a plain legacy shape.
func sessionGraphPublishStore(t *testing.T, payload string) (*testutil.StubPushStore, *testutil.MemFS) {
	t.Helper()
	fs := testutil.NewMemFS()
	seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
	content := "recorded input"
	entry := schema.SessionEntry{
		SessionID:      schema.SessionID(testutil.TestSessionUUID),
		EntryIndex:     0,
		Harness:        defaults.HarnessClaudeCode,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleUser,
		Depth:          0,
		ContentPreview: &content,
	}
	if payload == "graph" {
		entry.SourceEntryRef = schema.SourceEntryRef("e_u1")
		entry.Provenance = &schema.ContentProvenance{
			Origin:        schema.ContentOriginSubmittedInput,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      schema.DeliveryOriginSessionAdmission,
			Ownership:     schema.ContentOwnershipLocal,
			Evidence:      schema.EvidenceNativeTyped,
			InputModality: schema.InputModalityText,
			SubmissionRef: schema.SubmissionRef("s_u1"),
		}
	}
	return &testutil.StubPushStore{
		Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)},
		Entries:  map[ingest.SessionID][]schema.SessionEntry{ingest.SessionID(testutil.TestSessionUUID): {entry}},
	}, fs
}

// sessionGraphPublishTransport builds the fresh-negotiation transport for a
// case and returns a function reporting how many network calls it observed.
func sessionGraphPublishTransport(t *testing.T, fixtureCase sessionGraphPublishCase) (push.Transport, func() int) {
	t.Helper()
	switch fixtureCase.Transport {
	case "stub":
		pub := &testutil.StubPublisher{SchemaVersionResp: &schema.SchemaVersionResponse{
			MinPushContractVersion: schema.PushContractVersion("0.0.1"),
			PushContractVersion:    defaults.PublishSchemaVersion,
			ContentCapabilities:    fixtureCase.Advertisement,
		}}
		return pub, func() int { return pub.SchemaVersionCalls }
	case "unavailable":
		pub := &testutil.StubPublisher{SchemaVersionErr: errors.New("dial tcp: network is unreachable")}
		return pub, func() int { return pub.SchemaVersionCalls }
	case "null-advertisement":
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"annotationSchemaVersion":"1","pushContractVersion":%q,"minPushContractVersion":%q,"contentCapabilities":null}`, defaults.PublishSchemaVersion, defaults.PublishSchemaVersion)
		}))
		t.Cleanup(server.Close)
		return village.NewVillageClient(server.URL, "test-key", server.Client()), func() int { return 1 }
	default:
		t.Fatalf("case %q has unknown transport %q", fixtureCase.Name, fixtureCase.Transport)
		return nil, nil
	}
}

func newSessionGraphPipeline(t *testing.T, store *testutil.StubPushStore, transport push.Transport, fs *testutil.MemFS, cfg *config.Config, runCfg push.PipelineConfig, stderr io.Writer) *push.Pipeline {
	t.Helper()
	testutil.SeedPublicationInputs(store, fs, cfg.Output.BasePath)
	pipeline, err := push.NewPipeline(store, transport, baseCreds(), cfg, fs, runCfg, &testutil.NoopRedactor{}, stderr)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return pipeline
}

func sessionGraphPublishDetail(t *testing.T, payload string) *schema.SessionDetailPayload {
	t.Helper()
	detail := &schema.SessionDetailPayload{
		ID:        "45454545-4545-4545-4545-454545454590",
		Harness:   schema.HarnessCodex,
		TurnCount: 1,
		Turns:     []schema.TurnDetail{{Index: 0, Role: schema.RoleUser, Content: "recorded input"}},
	}
	switch payload {
	case "count-only":
		count := int64(0)
		detail.InputSubmissionCount = &count
	case "root-only":
		root := schema.SessionID("45454545-4545-4545-4545-454545454591")
		detail.RootSessionID = &root
	case "purpose-only":
		detail.Purpose = schema.SessionPurposeInteraction
	case "provenance":
		detail.Turns[0].SourceEntryRef = schema.SourceEntryRef("e_u1")
		detail.Turns[0].Provenance = &schema.ContentProvenance{
			Origin:        schema.ContentOriginSubmittedInput,
			Actor:         schema.ActorOriginUnknown,
			Delivery:      schema.DeliveryOriginSessionAdmission,
			Ownership:     schema.ContentOwnershipLocal,
			Evidence:      schema.EvidenceNativeTyped,
			InputModality: schema.InputModalityText,
			SubmissionRef: schema.SubmissionRef("s_u1"),
		}
	case "source-ref-only":
		detail.Turns[0].SourceEntryRef = schema.SourceEntryRef("e_u1")
	case "graph", "plain":
		// The preflight arm builds payload shapes the pipeline arm cannot; these
		// two shapes are exercised there through real indexed entries.
	default:
		t.Fatalf("unknown payload shape %q", payload)
	}
	return detail
}

func requireCapabilities(t *testing.T, sessions []push.SessionPushResult, want []schema.ContentCapability) {
	t.Helper()
	if len(sessions) == 0 {
		if len(want) != 0 {
			t.Fatalf("no sessions reported, want required %v", capabilityStrings(want))
		}
		return
	}
	for _, session := range sessions {
		if got, wantStrings := capabilityStrings(session.RequiredCapabilities), capabilityStrings(want); !reflect.DeepEqual(got, wantStrings) {
			t.Fatalf("session %s required=%v, want %v; status=%s err=%v", session.SessionID, got, wantStrings, session.Status, session.Error)
		}
	}
}

func requireUploads(t *testing.T, transport push.Transport, want int) {
	t.Helper()
	pub, ok := transport.(*testutil.StubPublisher)
	if !ok {
		// The real transport case counts its own requests.
		return
	}
	if got := len(pub.Calls); got != want {
		t.Fatalf("uploads=%d, want %d", got, want)
	}
}

func capabilityStrings(capabilities []schema.ContentCapability) []string {
	out := make([]string, len(capabilities))
	for index, capability := range capabilities {
		out[index] = string(capability)
	}
	return out
}
