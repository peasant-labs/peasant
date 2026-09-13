package perf_test

import (
	"bytes"
	"embed"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/profile_contract/*.yaml
var profileContractFS embed.FS

type contractBehaviorFixture struct {
	Cases []contractBehaviorCase `yaml:"cases"`
}

type contractBehaviorCase struct {
	Name            string            `yaml:"name"`
	ForbiddenValues []string          `yaml:"forbiddenValues"`
	SafeAttrs       map[string]string `yaml:"safeAttrs"`
	UnsafeAttrs     map[string]string `yaml:"unsafeAttrs"`
}

type fakeClock struct {
	now  time.Time
	step time.Duration
}

func (c *fakeClock) Now() time.Time {
	current := c.now
	c.now = c.now.Add(c.step)
	return current
}

func TestProfileCatalogMatchesRequiredNameManifests(t *testing.T) {
	t.Parallel()
	stageManifest := loadRequiredNames(t, "testdata/profile_contract/stages_manifest.yaml")
	stageNames := make([]string, 0, len(perf.AllStageIDs()))
	for _, stage := range perf.AllStageIDs() {
		if err := stage.Validate(); err != nil {
			t.Fatalf("stage catalog contains invalid stage %q: %v", stage, err)
		}
		stageNames = append(stageNames, stage.String())
	}
	if err := testutil.ValidateRequiredNames(stageManifest, stageNames, "profile stage catalog"); err != nil {
		t.Fatal(err)
	}

	counterManifest := loadRequiredNames(t, "testdata/profile_contract/counters_manifest.yaml")
	counterNames := make([]string, 0, len(perf.AllCounterNames()))
	for _, counter := range perf.AllCounterNames() {
		if err := counter.Validate(); err != nil {
			t.Fatalf("counter catalog contains invalid counter %q: %v", counter, err)
		}
		counterNames = append(counterNames, counter.String())
	}
	if err := testutil.ValidateRequiredNames(counterManifest, counterNames, "profile counter catalog"); err != nil {
		t.Fatal(err)
	}
}

func TestProfileRecorderReducesSafeJSONV1FromFixture(t *testing.T) {
	t.Parallel()
	fixture := loadBehaviorFixture(t)
	manifest := loadRequiredNames(t, "testdata/profile_contract/behavior_manifest.yaml")
	names := make([]string, 0, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		names = append(names, tc.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "profile behavior fixture"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			clock := &fakeClock{now: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), step: 10 * time.Millisecond}
			var trace bytes.Buffer
			collector := perf.NewCollectorWithOptions(clock, perf.NewJSONLTraceSink(&trace), perf.Options{Enabled: true})

			root := collector.StartSpan(perf.StagePushSession, perf.Attributes{perf.AttrSafeSubjectID: "session:parent"})
			span := collector.StartChildSpan(perf.StagePushSessionRedact, root.ID(), attrsFromStrings(tc.SafeAttrs))
			collector.Count(perf.CounterRedactionEntriesScanned, 2, perf.UnitCount, nil)
			collector.Count(perf.CounterRedactionBytesScanned, 128, perf.UnitBytes, nil)
			collector.Count(perf.CounterRedactionFindings, 1, perf.UnitCount, perf.Attributes{perf.AttrCategory: "secrets"})
			collector.Count(perf.CounterRedactionRulesMatched, 1, perf.UnitCount, perf.Attributes{perf.AttrRuleID: "secret_token"})
			collector.Count(perf.CounterPushHTTPRequests, 1, perf.UnitRequests, perf.Attributes{perf.AttrOperation: "publish"})
			span.End(perf.OutcomeOK, attrsFromStrings(tc.UnsafeAttrs))

			doc, err := perf.BuildProfileDocument(collector, perf.ProfileProducer{App: "peasant", Command: "village push", Version: "test"}, perf.ProfileRun{RunID: "safe-run", StartedAt: clock.now.Add(-20 * time.Millisecond), EndedAt: clock.now, ProfiledSubject: "push", SelectionMode: "selected", SessionCount: 1, ConcurrencyLimit: 1}, "trace.jsonl")
			if err != nil {
				t.Fatalf("BuildProfileDocument: %v", err)
			}
			var output bytes.Buffer
			if err := perf.WriteProfileJSON(&output, doc); err != nil {
				t.Fatalf("WriteProfileJSON: %v", err)
			}
			jsonPath := filepath.Join(t.TempDir(), "profile.json")
			if err := perf.WriteProfileJSONFile(jsonPath, doc); err != nil {
				t.Fatalf("WriteProfileJSONFile: %v", err)
			}
			info, err := os.Stat(jsonPath)
			if err != nil {
				t.Fatalf("stat profile file: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o600 {
				t.Fatalf("profile file mode = %v, want 0600", got)
			}

			assertJSONV1Shape(t, output.Bytes())
			assertDoesNotContain(t, output.String(), tc.ForbiddenValues)
			assertDoesNotContain(t, trace.String(), tc.ForbiddenValues)
			if doc.Summaries.Stages[0].Stage != perf.StagePushSessionRedact {
				t.Fatalf("first stage summary = %s, want %s", doc.Summaries.Stages[0].Stage, perf.StagePushSessionRedact)
			}
			if doc.Redaction.FindingsByCategory["secrets"] != 1 {
				t.Fatalf("redaction secret findings = %d, want 1", doc.Redaction.FindingsByCategory["secrets"])
			}
			if doc.Resources.HTTPRequests == nil || *doc.Resources.HTTPRequests != 1 {
				t.Fatalf("HTTPRequests = %v, want 1", doc.Resources.HTTPRequests)
			}
		})
	}
}

func TestProfileNoopRecorderPreservesDisabledContract(t *testing.T) {
	t.Parallel()
	recorder := perf.NewRecorder(nil, nil, perf.Options{})
	if recorder.Enabled() {
		t.Fatal("disabled recorder reports enabled")
	}
	recorder.Count(perf.CounterPushDBReads, 1, perf.UnitCount, nil)
	recorder.StartSpan(perf.StagePushRun, nil).End(perf.OutcomeOK, nil)
}

func TestProfileValidationErrorsAreActionable(t *testing.T) {
	t.Parallel()
	if err := perf.StageID("push.private-history").Validate(); err == nil || !strings.Contains(err.Error(), "typed perf.Stage") {
		t.Fatalf("stage validation error = %v, want typed-stage guidance", err)
	}
	if err := perf.CounterName("push.raw.path").Validate(); err == nil || !strings.Contains(err.Error(), "typed perf.Counter") {
		t.Fatalf("counter validation error = %v, want typed-counter guidance", err)
	}
	safe := (perf.Sanitizer{}).SafeError(perf.StagePushPublish, "HTTP_STATUS", errors.New("failed for /home/private/project secret token"), true)
	if strings.Contains(safe.SafeMessage, "/home/private") || strings.Contains(safe.SafeMessage, "secret token") {
		t.Fatalf("safe error leaked unsafe input: %+v", safe)
	}
	if safe.Code != "http_status" {
		t.Fatalf("safe error code = %q, want http_status", safe.Code)
	}
}

func loadRequiredNames(t *testing.T, path string) testutil.RequiredNamesManifest {
	t.Helper()
	data, err := profileContractFS.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(data, path)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return manifest
}

func loadBehaviorFixture(t *testing.T) contractBehaviorFixture {
	t.Helper()
	data, err := profileContractFS.ReadFile("testdata/profile_contract/behavior_cases.yaml")
	if err != nil {
		t.Fatalf("read behavior fixture: %v", err)
	}
	var fixture contractBehaviorFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode behavior fixture: %v", err)
	}
	return fixture
}

func attrsFromStrings(source map[string]string) perf.Attributes {
	if len(source) == 0 {
		return nil
	}
	out := make(perf.Attributes, len(source))
	for key, value := range source {
		out[perf.AttributeKey(key)] = value
	}
	return out
}

func assertJSONV1Shape(t *testing.T, data []byte) {
	t.Helper()
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("profile JSON is invalid: %v\n%s", err, string(data))
	}
	if raw["formatVersion"] != float64(perf.JSONFormatVersion) {
		t.Fatalf("formatVersion = %v, want %d", raw["formatVersion"], perf.JSONFormatVersion)
	}
	for _, key := range []string{"producer", "run", "summaries", "spans", "counters", "resources", "redaction", "errors"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("profile JSON missing top-level key %q", key)
		}
	}
}

func assertDoesNotContain(t *testing.T, output string, forbidden []string) {
	t.Helper()
	for _, value := range forbidden {
		if strings.Contains(output, value) {
			t.Fatalf("profile output leaked forbidden value %q in:\n%s", value, output)
		}
	}
}
