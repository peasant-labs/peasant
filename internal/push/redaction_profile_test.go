package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"math"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_redaction/cases.yaml
var redactionProfileCases []byte

//go:embed testdata/profile_redaction/manifest.yaml
var redactionProfileManifest []byte

type redactionProfileFixture struct {
	CustomRuleID         string           `yaml:"customRuleID"`
	DecreasingReport     bool             `yaml:"decreasingReport"`
	ReportUnavailable    bool             `yaml:"reportUnavailable"`
	InvalidTotal         bool             `yaml:"invalidTotal"`
	InconsistentCategory bool             `yaml:"inconsistentCategory"`
	NegativeCount        bool             `yaml:"negativeCount"`
	SessionIDs           []string         `yaml:"sessionIDs"`
	Name                 string           `yaml:"name"`
	Input                string           `yaml:"input"`
	Forbidden            []string         `yaml:"forbidden"`
	Rules                map[string]int64 `yaml:"rules"`
	Categories           map[string]int64 `yaml:"categories"`
	Disabled             bool             `yaml:"disabled"`
	InvalidCategory      string           `yaml:"invalidCategory"`
	InvalidRule          string           `yaml:"invalidRule"`
	FailureSeam          string           `yaml:"failureSeam"`
	FailureShape         bool             `yaml:"failureShape"`
	ExpectedFailures     int64            `yaml:"expectedFailures"`
}

func loadRedactionProfileFixtures(t *testing.T) []redactionProfileFixture {
	t.Helper()
	var document struct {
		Cases []redactionProfileFixture `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(redactionProfileCases))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(redactionProfileManifest, "redaction profile")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, c := range document.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "redaction profile"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

type profileReportRedactor struct {
	ingest.TextRedactor
	real        redact.Redactor
	fixture     redactionProfileFixture
	reportCalls int
}

var _ ingest.TextRedactor = (*profileReportRedactor)(nil)

func (r *profileReportRedactor) Report() redact.RedactionReport {
	r.reportCalls++
	report := r.real.Report()
	if r.fixture.DecreasingReport && r.reportCalls > 1 {
		return redact.RedactionReport{}
	}
	report.Warnings = []string{r.fixture.Input}
	report.Matches = []redact.Match{{MatchedText: r.fixture.Input, Rule: r.fixture.Input}}
	if r.fixture.InvalidTotal {
		report.TotalRedactions++
	}
	if r.fixture.InconsistentCategory {
		report.Categories = []string{string(redact.CategoryProject)}
	}
	if r.fixture.NegativeCount {
		report.Counts["email"] = -1
	}
	// Malformed reports must be withheld even if the baseline is also malformed.
	if report.TotalRedactions > 0 {
		if r.fixture.InvalidCategory != "" {
			report.Categories = append(report.Categories, r.fixture.InvalidCategory)
		}
		if r.fixture.InvalidRule != "" {
			report.Counts[r.fixture.InvalidRule]++
			report.TotalRedactions++
		}
	}
	return report
}

type profileNoReportRedactor struct{ ingest.TextRedactor }

var _ ingest.TextRedactor = profileNoReportRedactor{}

type redactionProfileClock struct{ tick atomic.Int64 }

var _ perf.Clock = (*redactionProfileClock)(nil)

func (c *redactionProfileClock) Now() time.Time {
	return time.Unix(0, c.tick.Add(1)*int64(time.Millisecond))
}

func TestPipelineRedactionProfile(t *testing.T) {
	shared, err := testutil.LoadProfileRedactionFixtures()
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range loadRedactionProfileFixtures(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			originalInput := fixture.Input
			for _, c := range shared.Cases {
				fixture.Input += " " + strings.Join(c.ForbiddenInputs, " ")
			}
			fs := testutil.NewMemFS()
			sid := testutil.TestSessionUUID
			ids := fixture.SessionIDs
			if len(ids) == 0 {
				ids = []string{sid}
			}
			store := &testutil.StubPushStore{Entries: make(map[ingest.SessionID][]schema.SessionEntry)}
			for _, sid := range ids {
				seedMemFS(t, fs, testutil.TestHostSlug, sid, defaults.HarnessClaudeCode)
				store.Sessions = append(store.Sessions, makeSession(sid, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), nil))
				store.Entries[ingest.SessionID(sid)] = []schema.SessionEntry{{
					SessionID: schema.SessionID(sid), EntryIndex: 1, Role: schema.RoleAssistant,
					Harness: schema.Harness(defaults.HarnessClaudeCode), EntryType: schema.EntryTypeText, ContentPreview: &fixture.Input,
				}}
			}
			var patterns []redact.UserPattern
			if fixture.CustomRuleID != "" {
				patterns = append(patterns, redact.UserPattern{ID: fixture.CustomRuleID, Category: redact.CategoryPII, Pattern: regexp.QuoteMeta(fixture.Input), Replacement: "<CUSTOM>"})
			}
			r, err := redact.NewRedactor(redact.Standard, patterns, redact.XDGPaths{})
			if err != nil {
				t.Fatal(err)
			}
			// Exercise subtraction of history unrelated to this push.
			r.RedactText(fixture.Input)
			var redactor ingest.TextRedactor = &profileReportRedactor{TextRedactor: r, real: r, fixture: fixture}
			if fixture.ReportUnavailable {
				redactor = profileNoReportRedactor{TextRedactor: r}
			}
			if fixture.FailureSeam != "" {
				var broken any = math.NaN()
				if fixture.FailureShape {
					broken = map[string]any{"not": "entries"}
				}
				var selects func(any) bool
				switch fixture.FailureSeam {
				case "entries":
					selects = seamEntries
				case "metadata":
					selects = seamMetadata
				case "transcript":
					selects = seamTranscript
				default:
					t.Fatal("unknown fixture failure seam")
				}
				redactor = &profileReportRedactor{TextRedactor: stubJSONRedactor{json: broken, breaks: selects, real: r}, real: r, fixture: fixture}
			}
			pub := &testutil.StubPublisher{StatusCode: 201}
			var stderr, trace bytes.Buffer
			sink := perf.NewJSONLTraceSink(&trace)
			collector := perf.NewCollectorWithOptions(&redactionProfileClock{}, sink, perf.Options{Enabled: true})
			ctx := context.Background()
			if !fixture.Disabled {
				ctx = perf.ContextWithRecorder(ctx, collector)
			}
			pipeline, err := push.NewPipeline(store, pub, baseCreds(), baseTestConfig(), fs, push.PipelineConfig{}, redactor, &stderr)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if fixture.FailureSeam != "" {
				if len(pub.Calls) != 0 || len(result.Sessions) != 1 || result.Sessions[0].Status != push.PushStatusError {
					t.Fatal("failed redaction must refuse publication and report a session error")
				}
			} else if len(pub.Calls) != len(ids) {
				t.Fatalf("expected publication, got %+v", result)
			}
			doc, err := perf.BuildProfileDocument(collector, perf.ProfileProducer{}, perf.ProfileRun{}, "")
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := perf.WriteProfileJSON(&output, doc); err != nil {
				t.Fatal(err)
			}
			// A second run on the same Pipeline with no context recorder must not
			// retain the run-scoped decorator or change publication bytes.
			firstCalls := append([]testutil.StubPublishCall(nil), pub.Calls...)
			firstStderr := stderr.String()
			pub.Calls = nil
			stderr.Reset()
			eventCount, traceSize := len(collector.Events()), trace.Len()
			plainResult, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(plainResult.Sessions) != len(result.Sessions) {
				t.Fatal("profiling changed session results")
			}
			for i := range result.Sessions {
				if plainResult.Sessions[i].Status != result.Sessions[i].Status {
					t.Fatal("profiling changed push status")
				}
			}
			if len(collector.Events()) != eventCount || trace.Len() != traceSize {
				t.Fatal("disabled run retained a recorder")
			}
			if len(ids) == 1 && stderr.String() != firstStderr {
				t.Fatal("profiling changed ordinary output")
			}
			if !reflect.DeepEqual(sortedPublishedParts(firstCalls), sortedPublishedParts(pub.Calls)) {
				t.Fatal("profiling changed published bytes")
			}
			forbidden := append([]string{originalInput, fixture.Input, fixture.InvalidCategory, fixture.InvalidRule, fixture.CustomRuleID}, fixture.Forbidden...)
			for _, c := range shared.Cases {
				forbidden = append(forbidden, c.ForbiddenInputs...)
			}
			for _, sentinel := range forbidden {
				if sentinel == "" {
					continue
				}
				encoded, _ := json.Marshal(sentinel)
				if strings.Contains(output.String(), sentinel) || strings.Contains(trace.String(), sentinel) || bytes.Contains(output.Bytes(), encoded) || bytes.Contains(trace.Bytes(), encoded) {
					t.Fatal("profile JSON or JSONL leaked a forbidden sentinel")
				}
			}
			if fixture.Disabled {
				if reporter, ok := redactor.(*profileReportRedactor); ok && reporter.reportCalls != 0 {
					t.Fatal("disabled profiling read the engine report")
				}
				if len(collector.Events()) != 0 || trace.Len() != 0 {
					t.Fatal("disabled profiling recorded events")
				}
				return
			}
			if !reflect.DeepEqual(doc.Redaction.RulesMatched, fixture.Rules) {
				t.Errorf("rules: got %v want %v", doc.Redaction.RulesMatched, fixture.Rules)
			}
			if !reflect.DeepEqual(doc.Redaction.FindingsByCategory, fixture.Categories) {
				t.Errorf("categories: got %v want %v", doc.Redaction.FindingsByCategory, fixture.Categories)
			}
			if doc.Redaction.Failures != fixture.ExpectedFailures {
				t.Errorf("failures: got %d want %d", doc.Redaction.Failures, fixture.ExpectedFailures)
			}
			expectedErrors := fixture.ExpectedFailures
			if fixture.ReportUnavailable {
				expectedErrors++
			}
			if int64(len(doc.Errors)) != expectedErrors {
				t.Errorf("safe errors: got %d want %d", len(doc.Errors), expectedErrors)
			}
			requireRedactionTraceMetrics(t, trace.Bytes(), doc.Redaction)
			if doc.Redaction.EntriesScanned != int64(len(ids)) || doc.Redaction.BytesScanned < int64(len(fixture.Input)*len(ids)) {
				t.Errorf("missing scanned input aggregates: %+v", doc.Redaction)
			}
			if len(doc.Spans) == 0 || trace.Len() == 0 {
				t.Fatal("missing application timing or JSONL events")
			}
			for _, span := range doc.Spans {
				if span.Stage == perf.StageRedactionApply && span.DurationMs <= 0 {
					t.Fatal("application spans must use the injected recorder clock")
				}
				if span.Stage == perf.StageRedactionScan || span.Stage == perf.StageRedactionRuleEvaluate {
					t.Fatal("seam cannot measure separate engine scan or rule durations")
				}
			}
			for _, counter := range doc.Counters {
				if counter.Name == perf.CounterRedactionReplacements {
					t.Fatal("engine report matches are not exact replacement counts")
				}
			}
		})
	}
}

func requireRedactionTraceMetrics(t *testing.T, trace []byte, expected perf.RedactionMetrics) {
	t.Helper()
	actual := perf.RedactionMetrics{FindingsByCategory: map[string]int64{}, RulesMatched: map[string]int64{}}
	decoder := json.NewDecoder(bytes.NewReader(trace))
	for {
		var event perf.Event
		if err := decoder.Decode(&event); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if event.Kind != perf.EventKindCounter {
			continue
		}
		switch event.CounterName {
		case perf.CounterRedactionEntriesScanned:
			actual.EntriesScanned += event.CounterValue
		case perf.CounterRedactionBytesScanned:
			actual.BytesScanned += event.CounterValue
		case perf.CounterRedactionFindings:
			actual.FindingsByCategory[event.Attributes[perf.AttrCategory]] += event.CounterValue
		case perf.CounterRedactionRulesMatched:
			actual.RulesMatched[event.Attributes[perf.AttrRuleID]] += event.CounterValue
		case perf.CounterRedactionFailures:
			actual.Failures += event.CounterValue
		}
	}
	if !reflect.DeepEqual(actual, expected) {
		t.Fatalf("JSONL metrics differ from JSON: got %+v want %+v", actual, expected)
	}
}

func sortedPublishedParts(calls []testutil.StubPublishCall) []string {
	var parts []string
	for _, call := range calls {
		for _, part := range publishedParts(call) {
			parts = append(parts, part)
		}
	}
	sort.Strings(parts)
	return parts
}
