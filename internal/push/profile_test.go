package push_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/internal/village"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_push/cases.yaml
var pushProfileCases []byte

//go:embed testdata/profile_push/manifest.yaml
var pushProfileManifest []byte

type pushProfileCase struct {
	Name                       string            `yaml:"name"`
	Sessions                   []string          `yaml:"sessions"`
	Concurrency                int               `yaml:"concurrency"`
	PublishStatus              int               `yaml:"publishStatus"`
	NegotiateStatus            int               `yaml:"negotiateStatus"`
	FailSession                string            `yaml:"failSession"`
	Visibility                 schema.Visibility `yaml:"visibility"`
	PatchStatus                int               `yaml:"patchStatus"`
	Annotations                bool              `yaml:"annotations"`
	AnnotationErrors           int               `yaml:"annotationErrors"`
	PersistFailure             bool              `yaml:"persistFailure"`
	ReceiptMismatch            bool              `yaml:"receiptMismatch"`
	MalformedReceipt           bool              `yaml:"malformedReceipt"`
	Barrier                    bool              `yaml:"barrier"`
	Disabled                   bool              `yaml:"disabled"`
	ConfigRecorder             bool              `yaml:"configRecorder"`
	DryRun                     bool              `yaml:"dryRun"`
	Sources                    []string          `yaml:"sources"`
	ExcludeAll                 bool              `yaml:"excludeAll"`
	EntryReadFailOnCall        int               `yaml:"entryReadFailOnCall"`
	ExpectedPublished          int               `yaml:"expectedPublished"`
	ExpectedFailed             int               `yaml:"expectedFailed"`
	ExpectedSkipped            int               `yaml:"expectedSkipped"`
	ExpectedSaved              int               `yaml:"expectedSaved"`
	ExpectedRequests           int64             `yaml:"expectedRequests"`
	ExpectedDBReads            int64             `yaml:"expectedDBReads"`
	ExpectedHighWater          int64             `yaml:"expectedHighWater"`
	ExpectedPatches            int64             `yaml:"expectedPatches"`
	ExpectedAnnotationRequests int64             `yaml:"expectedAnnotationRequests"`
	Retryable                  bool              `yaml:"retryable"`
	FailedStage                perf.StageID      `yaml:"failedStage"`
	SkippedStage               perf.StageID      `yaml:"skippedStage"`
	Stages                     []perf.StageID    `yaml:"stages"`
}

// A constant injected clock gives deterministic structural profiles even when
// requests overlap. Concurrency is proven by a barrier, not elapsed thresholds.
type profileClock struct{}

var _ perf.Clock = profileClock{}

func (profileClock) Now() time.Time { return time.Unix(1, 0).UTC() }

func loadPushProfileFixtures(t *testing.T) []pushProfileCase {
	t.Helper()
	var doc struct {
		Cases []pushProfileCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pushProfileCases))
	decoder.KnownFields(true)
	if err := decoder.Decode(&doc); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("fixture must contain exactly one document: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(pushProfileManifest, "push profiles")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(doc.Cases))
	for _, c := range doc.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push profiles"); err != nil {
		t.Fatal(err)
	}
	shared, err := testutil.LoadProfilePushFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, required := range shared.Names() {
		found := false
		for _, name := range names {
			found = found || name == required
		}
		if !found {
			t.Fatalf("shared requirement %s has no production fixture", required)
		}
	}
	return doc.Cases
}

func TestPipelineProfileFixtures(t *testing.T) {
	for _, tc := range loadPushProfileFixtures(t) {
		t.Run(tc.Name, func(t *testing.T) {
			first := runPushProfileFixture(t, tc)
			if tc.Disabled {
				tc.Disabled = false
				enabled := runPushProfileFixture(t, tc)
				if first.output != enabled.output || !reflect.DeepEqual(first.statuses, enabled.statuses) {
					t.Fatalf("profiling changed ordinary output/results: %#v versus %#v", first, enabled)
				}
			} else if tc.Barrier {
				second := runPushProfileFixture(t, tc)
				if !reflect.DeepEqual(first.subjects, second.subjects) || !reflect.DeepEqual(first.counters, second.counters) || !reflect.DeepEqual(first.summaries, second.summaries) {
					t.Fatal("repeated concurrent runs changed reduced subjects, counters or summaries")
				}
			}
		})
	}
}

type pushProfileEvidence struct {
	output    string
	statuses  map[string]push.PushStatus
	subjects  []string
	counters  map[perf.CounterName]int64
	summaries []perf.StageSummary
}

// A barrier inside the real pipeline's redaction dependency proves that profile
// attribution does not serialize engine calls across independent sessions.
type concurrentProfileRedactor struct {
	ingest.TextRedactor
	arrivals atomic.Int64
	expected int64
	release  chan struct{}
	ctx      context.Context
	t        *testing.T
}

var _ ingest.TextRedactor = (*concurrentProfileRedactor)(nil)

func (r *concurrentProfileRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	if r.arrivals.Add(1) == r.expected {
		close(r.release)
	}
	select {
	case <-r.release:
	case <-r.ctx.Done():
		r.t.Error("concurrent sessions did not reach the redaction barrier before cancellation")
	}
	return r.TextRedactor.RedactMetadata(meta)
}

func runPushProfileFixture(t *testing.T, tc pushProfileCase) pushProfileEvidence {
	t.Helper()
	var requests, requestBytes, responseBytes, arrivals atomic.Int64
	barrier := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		requestBytes.Add(int64(len(body)))
		write := func(status int, data []byte) {
			w.WriteHeader(status)
			n, err := w.Write(data)
			responseBytes.Add(int64(n))
			if err != nil {
				t.Error(err)
			}
		}
		encode := func(value any) []byte {
			data, err := json.Marshal(value)
			if err != nil {
				t.Error(err)
			}
			return data
		}
		switch r.URL.Path {
		case "/api/v1/schema/version":
			if tc.NegotiateStatus != 0 {
				write(tc.NegotiateStatus, []byte("PRIVATE_TRANSCRIPT_SENTINEL"))
				return
			}
			write(http.StatusOK, encode(schema.SchemaVersionResponse{PushContractVersion: defaults.PublishSchemaVersion, MinPushContractVersion: defaults.PublishSchemaVersion}))
		case "/api/v1/transcripts/publish":
			r.Body = io.NopCloser(bytes.NewReader(body))
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				return
			}
			defer r.MultipartForm.RemoveAll()
			req, err := schema.DecodeAuthoritativePublishRequest([]byte(r.FormValue("metadata")))
			if err != nil {
				t.Error(err)
				return
			}
			if req.VisibilityIntent != schema.VisibilityIntentPrivate {
				t.Error("publish must remain private before widening")
			}
			if tc.Barrier {
				if arrivals.Add(1) == int64(len(tc.Sessions)) {
					close(barrier)
				}
				select {
				case <-barrier:
				case <-r.Context().Done():
					return
				}
			}
			status := tc.PublishStatus
			if tc.FailSession != "" && req.Identity.SessionID.String() != tc.FailSession {
				status = http.StatusCreated
			}
			if status >= http.StatusBadRequest {
				write(status, []byte("PRIVATE_TRANSCRIPT_SENTINEL"))
				return
			}
			raw, err := testutil.AuthoritativePublishReceiptFromRequest(req, status == http.StatusCreated)
			if err != nil {
				t.Error(err)
				return
			}
			if tc.ReceiptMismatch {
				receipt, err := schema.DecodePublishResponse(raw)
				if err != nil {
					t.Error(err)
					return
				}
				receipt.ContentHash = schema.ComputeTranscriptContentHash([]byte("different"))
				raw = encode(receipt)
			}
			if tc.MalformedReceipt {
				raw = []byte(`{"private":"PRIVATE_TRANSCRIPT_SENTINEL"}`)
			}
			write(status, raw)
		case "/api/v1/annotations/manifest":
			write(http.StatusOK, encode(schema.AnnotationManifestResponse{}))
		case "/api/v1/annotations":
			write(http.StatusOK, encode(schema.AnnotationPushResponse{Created: 1 - tc.AnnotationErrors, Errors: tc.AnnotationErrors}))
		default:
			if r.Method != http.MethodPatch {
				t.Errorf("unexpected request %s", r.URL.Path)
				return
			}
			if tc.PatchStatus != 0 {
				write(tc.PatchStatus, []byte("PRIVATE_PROJECT_HISTORY_SENTINEL"))
				return
			}
			var update schema.OwnerTranscriptUpdateRequest
			if err := json.Unmarshal(body, &update); err != nil {
				t.Error(err)
				return
			}
			id, err := schema.NewTranscriptID(testutil.TestSessionUUID)
			if err != nil {
				t.Error(err)
				return
			}
			write(http.StatusOK, encode(schema.OwnerTranscriptUpdateResponse{TranscriptID: id, TranscriptURL: "https://village.example/transcripts/" + id.String(), Visibility: *update.Visibility, Tags: []string{}, UpdatedAt: 1}))
		}
	}))
	defer server.Close()
	fs := testutil.NewMemFS()
	st := &testutil.StubPushStore{}
	for _, id := range tc.Sessions {
		st.Sessions = append(st.Sessions, makeSession(id, "host", string(defaults.HarnessClaudeCode), nil))
		seedMemFS(t, fs, "host", id, defaults.HarnessClaudeCode)
	}
	if tc.PersistFailure {
		st.SavePublicationErr = errors.New("PRIVATE_TRANSCRIPT_SENTINEL")
	}
	if tc.EntryReadFailOnCall > 0 {
		st.ListEntriesErr = errors.New("PRIVATE_TRANSCRIPT_SENTINEL")
		st.ListEntriesFailOnCall = tc.EntryReadFailOnCall
	}
	cfg := baseTestConfig()
	if len(tc.Sources) > 0 {
		cfg.Push.Method = config.PushMethodBySource
		cfg.Push.Sources = tc.Sources
	}
	runCfg := push.PipelineConfig{Concurrency: tc.Concurrency, DryRun: tc.DryRun}
	if tc.Visibility != "" {
		runCfg.Visibility = tc.Visibility
	}
	if tc.ExcludeAll {
		runCfg.Selection = push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{})
	}
	creds := baseCreds()
	creds.VillageURL = server.URL
	client := village.NewVillageClient(server.URL, testAPIKey, server.Client())
	var output, trace bytes.Buffer
	collector := perf.NewCollectorWithOptions(profileClock{}, perf.NewJSONLTraceSink(&trace), perf.Options{Enabled: true})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if tc.ConfigRecorder {
		runCfg.Recorder = collector
	} else if !tc.Disabled {
		ctx = perf.ContextWithRecorder(ctx, collector)
	}
	var redactor ingest.TextRedactor = &testutil.NoopRedactor{}
	if tc.Barrier {
		redactor = &concurrentProfileRedactor{TextRedactor: redactor, expected: int64(len(tc.Sessions)), release: make(chan struct{}), ctx: ctx, t: t}
	}
	pipeline, err := push.NewPipeline(st, client, creds, cfg, fs, runCfg, redactor, &output)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if tc.Annotations {
		annotationStore := &stubAnnotationStore{rows: []ingest.AnnotationPushRow{newSessionAnnotationRow(tc.Sessions[0], testutil.TestTypeIDSessionOutcome, "success")}}
		summary, err := push.PushAnnotationsSelected(ctx, client, annotationStore, push.AnnotationSelection{}, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Errors != tc.AnnotationErrors {
			t.Fatalf("annotation errors = %d", summary.Errors)
		}
	}
	if !tc.DryRun && result.New+result.Updated != tc.ExpectedPublished || result.Errors != tc.ExpectedFailed || len(st.SavedPublicationIDs) != tc.ExpectedSaved {
		t.Fatalf("result=%+v saved=%v", result, st.SavedPublicationIDs)
	}
	if requests.Load() != tc.ExpectedRequests {
		t.Fatalf("HTTP requests = %d want %d", requests.Load(), tc.ExpectedRequests)
	}
	doc, err := perf.BuildProfileDocument(collector, perf.ProfileProducer{}, perf.ProfileRun{}, "")
	if err != nil {
		t.Fatal(err)
	}
	evidence := pushProfileEvidence{output: output.String(), statuses: map[string]push.PushStatus{}, counters: map[perf.CounterName]int64{}, summaries: doc.Summaries.Stages}
	for _, sr := range result.Sessions {
		evidence.statuses[sr.SessionID] = sr.Status
	}
	if tc.Disabled {
		if len(collector.Events()) != 0 || trace.Len() != 0 {
			t.Fatal("disabled run emitted profiling events")
		}
		return evidence
	}
	assertStandaloneProfileAncestry(t, doc.Spans)
	for _, counter := range doc.Counters {
		evidence.counters[counter.Name] += counter.Value
		if counter.Attrs[string(perf.AttrOperation)] == "publish" && counter.Attrs[string(perf.AttrSafeSubjectID)] == "" {
			t.Errorf("publish counter %s lost per-session attribution", counter.Name)
		}
	}
	assertCount := func(name perf.CounterName, want int64) {
		t.Helper()
		if got := evidence.counters[name]; got != want {
			t.Errorf("%s=%d want %d", name, got, want)
		}
	}
	assertCount(perf.CounterPushHTTPRequests, tc.ExpectedRequests)
	assertCount(perf.CounterPushHTTPResponses, tc.ExpectedRequests)
	assertCount(perf.CounterPushDBReads, tc.ExpectedDBReads)
	assertCount(perf.CounterPushSessionsPublished, int64(tc.ExpectedPublished))
	assertCount(perf.CounterPushSessionsFailed, int64(tc.ExpectedFailed))
	assertCount(perf.CounterPushSessionsSkipped, int64(tc.ExpectedSkipped))
	assertCount(perf.CounterPushHTTPRetries, 0)
	assertCount(perf.CounterPushPayloadBytes, requestBytes.Load())
	assertCount(perf.CounterPushResponseBytes, responseBytes.Load())
	assertCount(perf.CounterPushConcurrencyHighWater, tc.ExpectedHighWater)
	assertCount(perf.CounterPushVisibilityPatchRequest, tc.ExpectedPatches)
	assertCount(perf.CounterPushAnnotationRequests, tc.ExpectedAnnotationRequests)
	selected := len(tc.Sessions)
	if tc.ExcludeAll {
		selected = 0
	}
	assertCount(perf.CounterPushSessionsSelected, int64(selected))
	stages := map[perf.StageID]bool{}
	failedStages := map[perf.StageID]bool{}
	skippedStages := map[perf.StageID]bool{}
	for _, span := range doc.Spans {
		stages[span.Stage] = true
		if span.Outcome == perf.OutcomeFailed {
			failedStages[span.Stage] = true
		}
		if span.Outcome == perf.OutcomeSkipped {
			skippedStages[span.Stage] = true
		}
		if span.Stage == perf.StagePushSession {
			evidence.subjects = append(evidence.subjects, span.SafeSubjectID)
		}
		if span.Stage == perf.StagePushRetry && span.Outcome != perf.OutcomeSkipped {
			t.Error("no retry loop exists: retry must be skipped")
		}
	}
	if stages[perf.StagePushRun] {
		t.Error("pipeline duplicated the CLI-owned push.run span")
	}
	for _, stage := range tc.Stages {
		if !stages[stage] {
			t.Errorf("missing stage %s", stage)
		}
	}
	if tc.FailedStage != "" && !failedStages[tc.FailedStage] {
		t.Errorf("stage %s did not record failure", tc.FailedStage)
	}
	if tc.SkippedStage != "" && !skippedStages[tc.SkippedStage] {
		t.Errorf("stage %s did not record skip", tc.SkippedStage)
	}
	if !sort.StringsAreSorted(evidence.subjects) {
		t.Fatal("session profile subjects are not sorted")
	}
	for _, subject := range evidence.subjects {
		found := false
		for _, id := range tc.Sessions {
			if subject == fmt.Sprintf("session:%x", sha256.Sum256([]byte(id))) {
				found = true
			}
		}
		if !found {
			t.Errorf("unexpected profile subject %s", subject)
		}
	}
	retryable := false
	for _, failure := range doc.Errors {
		retryable = retryable || failure.Retryable
	}
	if retryable != tc.Retryable {
		t.Errorf("retryable=%v want %v", retryable, tc.Retryable)
	}
	var profile bytes.Buffer
	if err := perf.WriteProfileJSON(&profile, doc); err != nil {
		t.Fatal(err)
	}
	shared, err := testutil.LoadProfilePushFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range shared.Cases {
		for _, forbidden := range fixture.ForbiddenInputs {
			if strings.Contains(profile.String(), forbidden) || strings.Contains(trace.String(), forbidden) {
				t.Errorf("profile leaked %s", forbidden)
			}
		}
	}
	if strings.Contains(profile.String(), server.URL) || strings.Contains(trace.String(), server.URL) {
		t.Error("profile leaked remote URL")
	}
	return evidence
}

func assertStandaloneProfileAncestry(t *testing.T, spans []perf.ProfileSpan) {
	t.Helper()
	byID := make(map[string]perf.ProfileSpan, len(spans))
	for _, span := range spans {
		if _, exists := byID[span.SpanID]; exists || span.SpanID == "" {
			t.Fatalf("duplicate or missing span ID %q", span.SpanID)
		}
		byID[span.SpanID] = span
	}
	for _, span := range spans {
		if span.Stage == perf.StagePushSession && span.ParentSpanID != "" {
			t.Error("standalone session must not fabricate a run parent")
		}
		if span.Stage == perf.StageRedactionApply {
			parent := byID[span.ParentSpanID]
			if parent.Stage != perf.StagePushSession || parent.SafeSubjectID != span.SafeSubjectID {
				t.Fatalf("redaction span %s lost its own session parent or attribution", span.SpanID)
			}
		}
		seen := make(map[string]bool)
		for current := span; current.ParentSpanID != ""; {
			if seen[current.SpanID] {
				t.Fatalf("cycle in ancestry of %s", span.SpanID)
			}
			seen[current.SpanID] = true
			parent, exists := byID[current.ParentSpanID]
			if !exists {
				t.Fatalf("dangling parent %s from %s", current.ParentSpanID, span.SpanID)
			}
			if parent.Stage == perf.StagePushSession && current.SafeSubjectID != parent.SafeSubjectID {
				t.Fatalf("span %s attached to another session", current.SpanID)
			}
			current = parent
		}
	}
}
