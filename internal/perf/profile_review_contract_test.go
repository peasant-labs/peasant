package perf_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

type errorContractCase struct {
	Name            string   `yaml:"name"`
	Message         string   `yaml:"message"`
	Wrapper         string   `yaml:"wrapper"`
	Carrier         bool     `yaml:"carrier"`
	Pointer         bool     `yaml:"pointer"`
	RejectErrorCall bool     `yaml:"rejectErrorCall"`
	Code            string   `yaml:"code"`
	Retryable       bool     `yaml:"retryable"`
	ExpectedCode    string   `yaml:"expectedCode"`
	ExpectedMessage string   `yaml:"expectedMessage"`
	ForbiddenValues []string `yaml:"forbiddenValues"`
}

type unreadableProfileError struct{}

func (unreadableProfileError) Error() string {
	panic("generic profile error prose must never be read")
}

var _ error = unreadableProfileError{}

func TestProfileErrorPrivacyContract(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []errorContractCase `yaml:"cases"`
	}
	loadReviewFixtures(t, "error", &fixture)
	names := make([]string, 0, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		names = append(names, tc.Name)
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			var input error = errors.New(tc.Message)
			if tc.RejectErrorCall {
				input = unreadableProfileError{}
			}
			if tc.Carrier {
				diagnostic := perf.SafeError{Code: tc.Code, SafeMessage: tc.Message, Retryable: tc.Retryable}
				input = diagnostic
				if tc.Pointer {
					input = &diagnostic
				}
			}
			if tc.Wrapper != "" {
				input = fmt.Errorf("%s: %w", tc.Wrapper, input)
			}
			var trace bytes.Buffer
			collector := perf.NewCollectorWithOptions(nil, perf.NewJSONLTraceSink(&trace), perf.Options{Enabled: true})
			collector.Error(perf.StagePushPublish, input, nil)
			doc, err := perf.BuildProfileDocument(collector, perf.ProfileProducer{}, perf.ProfileRun{}, "")
			if err != nil {
				t.Fatal(err)
			}
			var output bytes.Buffer
			if err := perf.WriteProfileJSON(&output, doc); err != nil {
				t.Fatal(err)
			}
			assertDoesNotContain(t, output.String(), tc.ForbiddenValues)
			assertDoesNotContain(t, trace.String(), tc.ForbiddenValues)
			want := perf.SafeError{Stage: perf.StagePushPublish, Code: tc.ExpectedCode, SafeMessage: tc.ExpectedMessage, Retryable: tc.Retryable}
			if got := (perf.Sanitizer{}).SafeError(perf.StagePushPublish, "error_code", input, false); got != want {
				t.Fatalf("sanitizer = %#v, want %#v", got, want)
			}
			if !reflect.DeepEqual(doc.Errors, []perf.SafeError{want}) {
				t.Fatalf("JSON errors = %+v, want %+v", doc.Errors, want)
			}
			var event perf.Event
			if err := json.Unmarshal(trace.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			if event.SafeError == nil || *event.SafeError != want {
				t.Fatalf("JSONL error = %+v, want %+v", event.SafeError, want)
			}
		})
	}
	assertReviewManifest(t, "error", names)
}

type bottleneckContractCase struct {
	Name          string `yaml:"name"`
	RunDurationMs int64  `yaml:"runDurationMs"`
	RunEndedMs    int64  `yaml:"runEndedMs"`
	Spans         []struct {
		ID      string       `yaml:"id"`
		Parent  string       `yaml:"parent"`
		Stage   perf.StageID `yaml:"stage"`
		StartMs int64        `yaml:"startMs"`
		EndMs   int64        `yaml:"endMs"`
	} `yaml:"spans"`
	Expected []struct {
		Stage      perf.StageID `yaml:"stage"`
		TotalMs    int64        `yaml:"totalMs"`
		ShareOfRun float64      `yaml:"shareOfRun"`
	} `yaml:"expected"`
}

func TestProfileBottleneckRunShareContract(t *testing.T) {
	t.Parallel()
	var fixture struct {
		Cases []bottleneckContractCase `yaml:"cases"`
	}
	loadReviewFixtures(t, "bottleneck", &fixture)
	names := make([]string, 0, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		names = append(names, tc.Name)
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			base := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
			clock := &fakeClock{now: base}
			collector := perf.NewCollectorWithOptions(clock, nil, perf.Options{Enabled: true})
			type boundary struct {
				ms    int64
				index int
				end   bool
			}
			boundaries := make([]boundary, 0, 2*len(tc.Spans))
			for i, span := range tc.Spans {
				boundaries = append(boundaries, boundary{ms: span.StartMs, index: i}, boundary{ms: span.EndMs, index: i, end: true})
			}
			sort.SliceStable(boundaries, func(i, j int) bool { return boundaries[i].ms < boundaries[j].ms })
			spans := make(map[string]perf.Span)
			for _, point := range boundaries {
				clock.now = base.Add(time.Duration(point.ms) * time.Millisecond)
				spec := tc.Spans[point.index]
				if point.end {
					spans[spec.ID].End(perf.OutcomeOK, nil)
					continue
				}
				parent := ""
				if spec.Parent != "" {
					parent = spans[spec.Parent].ID()
				}
				spans[spec.ID] = collector.StartChildSpan(spec.Stage, parent, nil)
			}
			run := perf.ProfileRun{DurationMs: tc.RunDurationMs}
			if tc.RunEndedMs != 0 {
				run.StartedAt = base
				run.EndedAt = base.Add(time.Duration(tc.RunEndedMs) * time.Millisecond)
			}
			doc, err := perf.BuildProfileDocument(collector, perf.ProfileProducer{}, run, "")
			if err != nil {
				t.Fatal(err)
			}
			want := make([]perf.BottleneckSummary, 0, len(tc.Expected))
			for _, expected := range tc.Expected {
				want = append(want, perf.BottleneckSummary{Stage: expected.Stage, TotalMs: expected.TotalMs, ShareOfRun: expected.ShareOfRun})
			}
			if !reflect.DeepEqual(doc.Summaries.TopBottlenecks, want) {
				t.Fatalf("bottlenecks = %+v, want %+v", doc.Summaries.TopBottlenecks, want)
			}
			reduced, err := (perf.Reducer{RunDurationMs: doc.Run.DurationMs}).Reduce(collector.Events())
			if err != nil || !reflect.DeepEqual(reduced, doc.Summaries) {
				t.Fatalf("reducer = %+v, %v; want document summary %+v", reduced, err, doc.Summaries)
			}
			if len(tc.Spans) > 0 && len(doc.Summaries.Stages) == 0 {
				t.Fatal("stage evidence discarded with unavailable run share")
			}
			var output bytes.Buffer
			if err := perf.WriteProfileJSON(&output, doc); err != nil {
				t.Fatal(err)
			}
			var decoded perf.ProfileDocument
			if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(decoded.Summaries.TopBottlenecks, want) {
				t.Fatalf("JSON bottlenecks = %+v, want %+v", decoded.Summaries.TopBottlenecks, want)
			}
		})
	}
	assertReviewManifest(t, "bottleneck", names)
}

func loadReviewFixtures(t *testing.T, family string, fixture any) {
	t.Helper()
	data, err := profileContractFS.ReadFile("testdata/profile_contract/" + family + "_cases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(fixture); err != nil {
		t.Fatal(err)
	}
}

func assertReviewManifest(t *testing.T, family string, names []string) {
	t.Helper()
	manifest := loadRequiredNames(t, "testdata/profile_contract/"+family+"_manifest.yaml")
	if err := testutil.ValidateRequiredNames(manifest, names, family+" contract fixture"); err != nil {
		t.Fatal(err)
	}
}
