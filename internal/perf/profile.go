package perf

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"
)

type ProfileProducer struct {
	App               string `json:"app"`
	Command           string `json:"command"`
	Version           string `json:"version"`
	ProfileAPIVersion string `json:"profileApiVersion"`
}

type ProfileRun struct {
	RunID            string    `json:"runId"`
	StartedAt        time.Time `json:"startedAt"`
	EndedAt          time.Time `json:"endedAt"`
	DurationMs       int64     `json:"durationMs"`
	ProfiledSubject  string    `json:"profiledSubject"`
	SelectionMode    string    `json:"selectionMode,omitempty"`
	SessionCount     int       `json:"sessionCount,omitempty"`
	ConcurrencyLimit int       `json:"concurrencyLimit,omitempty"`
}

type ProfileSummary struct {
	Stages         []StageSummary      `json:"stages"`
	TopBottlenecks []BottleneckSummary `json:"topBottlenecks"`
}

type StageSummary struct {
	Stage    StageID         `json:"stage"`
	Count    int             `json:"count"`
	TotalMs  int64           `json:"totalMs"`
	MinMs    int64           `json:"minMs"`
	MaxMs    int64           `json:"maxMs"`
	P50Ms    int64           `json:"p50Ms"`
	P95Ms    int64           `json:"p95Ms"`
	P99Ms    int64           `json:"p99Ms"`
	Errors   int             `json:"errors"`
	Outcomes map[Outcome]int `json:"outcomes"`
}

type BottleneckSummary struct {
	Stage      StageID `json:"stage"`
	TotalMs    int64   `json:"totalMs"`
	ShareOfRun float64 `json:"shareOfRun"`
}

type ProfileSpan struct {
	SpanID        string            `json:"spanId"`
	ParentSpanID  string            `json:"parentSpanId,omitempty"`
	Stage         StageID           `json:"stage"`
	SafeSubjectID string            `json:"safeSubjectId,omitempty"`
	StartedAt     time.Time         `json:"startedAt"`
	DurationMs    int64             `json:"durationMs"`
	Outcome       Outcome           `json:"outcome"`
	Attrs         map[string]string `json:"attrs,omitempty"`
}

type ProfileCounter struct {
	Name  CounterName       `json:"name"`
	Value int64             `json:"value"`
	Unit  Unit              `json:"unit"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

type ResourceMetrics struct {
	DBReads              *int64 `json:"dbReads"`
	HTTPRequests         *int64 `json:"httpRequests"`
	HTTPRetries          *int64 `json:"httpRetries"`
	PayloadBytes         *int64 `json:"payloadBytes"`
	ResponseBytes        *int64 `json:"responseBytes"`
	AllocBytes           *int64 `json:"allocBytes"`
	ConcurrencyHighWater *int64 `json:"concurrencyHighWater"`
}

type RedactionMetrics struct {
	EntriesScanned     int64            `json:"entriesScanned"`
	BytesScanned       int64            `json:"bytesScanned"`
	FindingsByCategory map[string]int64 `json:"findingsByCategory"`
	RulesMatched       map[string]int64 `json:"rulesMatched"`
	Failures           int64            `json:"failures"`
}

type ProfileDocument struct {
	FormatVersion int              `json:"formatVersion"`
	Producer      ProfileProducer  `json:"producer"`
	Run           ProfileRun       `json:"run"`
	Summaries     ProfileSummary   `json:"summaries"`
	Spans         []ProfileSpan    `json:"spans"`
	Counters      []ProfileCounter `json:"counters"`
	Resources     ResourceMetrics  `json:"resources"`
	Redaction     RedactionMetrics `json:"redaction"`
	Errors        []SafeError      `json:"errors"`
	TraceFile     string           `json:"traceFile,omitempty"`
}

type Reducer struct{}

var _ SummaryReducer = Reducer{}

func (Reducer) Reduce(events []Event) (ProfileSummary, error) {
	byStage := map[StageID][]Event{}
	errors := map[StageID]int{}
	for _, event := range events {
		switch event.Kind {
		case EventKindSpanEnd:
			if err := event.Stage.Validate(); err != nil {
				return ProfileSummary{}, err
			}
			byStage[event.Stage] = append(byStage[event.Stage], event)
		case EventKindError:
			errors[event.Stage]++
		}
	}
	stages := make([]StageSummary, 0, len(byStage))
	for stage, stageEvents := range byStage {
		durations := make([]int64, 0, len(stageEvents))
		outcomes := map[Outcome]int{}
		var total int64
		for _, event := range stageEvents {
			durations = append(durations, event.DurationMs)
			total += event.DurationMs
			outcomes[event.Outcome]++
		}
		slices.Sort(durations)
		stages = append(stages, StageSummary{Stage: stage, Count: len(stageEvents), TotalMs: total, MinMs: minValue(durations), MaxMs: maxValue(durations), P50Ms: percentileValue(durations, 50), P95Ms: percentileValue(durations, 95), P99Ms: percentileValue(durations, 99), Errors: errors[stage], Outcomes: outcomes})
	}
	sort.Slice(stages, func(i, j int) bool { return stages[i].Stage < stages[j].Stage })
	top := bottlenecks(stages)
	return ProfileSummary{Stages: stages, TopBottlenecks: top}, nil
}

func BuildProfileDocument(c *Collector, producer ProfileProducer, run ProfileRun, traceFile string) (ProfileDocument, error) {
	if c == nil {
		return ProfileDocument{}, fmt.Errorf("build profile document: collector is nil; create an enabled perf.Collector before writing JSON v1")
	}
	events := c.Events()
	summary, err := (Reducer{}).Reduce(events)
	if err != nil {
		return ProfileDocument{}, err
	}
	if producer.ProfileAPIVersion == "" {
		producer.ProfileAPIVersion = ProfileAPIVersion
	}
	if run.DurationMs == 0 && !run.StartedAt.IsZero() && !run.EndedAt.IsZero() {
		run.DurationMs = durationMs(run.EndedAt.Sub(run.StartedAt))
	}
	spans, counters, resources, redaction, errors := reduceProfileParts(events)
	return ProfileDocument{FormatVersion: JSONFormatVersion, Producer: producer, Run: run, Summaries: summary, Spans: spans, Counters: counters, Resources: resources, Redaction: redaction, Errors: errors, TraceFile: traceFile}, nil
}

func WriteProfileJSON(w io.Writer, doc ProfileDocument) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return fmt.Errorf("write profile JSON v1: encode profile document failed; profile was not written and caller should retry with a writable destination: %w", err)
	}
	return nil
}

func WriteProfileJSONFile(path string, doc ProfileDocument) error {
	if path == "" {
		return fmt.Errorf("write profile JSON v1 file: output path is empty; pass a writable file path for the local diagnostic profile")
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".profile-*.json.tmp")
	if err != nil {
		return fmt.Errorf("write profile JSON v1 file: create temporary file in %s failed; check directory permissions and retry: %w", dir, err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()
	if err := WriteProfileJSON(tmp, doc); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write profile JSON v1 file: close temporary file %s failed; profile was not committed and caller should retry: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return fmt.Errorf("write profile JSON v1 file: set private permissions on %s failed; profile was not committed and caller should retry: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("write profile JSON v1 file: atomic rename to %s failed; profile was not committed and caller should retry: %w", path, err)
	}
	committed = true
	return nil
}

func reduceProfileParts(events []Event) ([]ProfileSpan, []ProfileCounter, ResourceMetrics, RedactionMetrics, []SafeError) {
	spans := []ProfileSpan{}
	counters := []ProfileCounter{}
	resourceValues := map[CounterName]int64{}
	redaction := RedactionMetrics{FindingsByCategory: map[string]int64{}, RulesMatched: map[string]int64{}}
	errors := []SafeError{}
	for _, event := range events {
		switch event.Kind {
		case EventKindSpanEnd:
			attrs := sortedAttrMap(event.Attributes)
			safeSubject := attrs[string(AttrSafeSubjectID)]
			delete(attrs, string(AttrSafeSubjectID))
			spans = append(spans, ProfileSpan{SpanID: event.SpanID, ParentSpanID: event.ParentSpanID, Stage: event.Stage, SafeSubjectID: safeSubject, StartedAt: event.StartedAt, DurationMs: event.DurationMs, Outcome: event.Outcome, Attrs: attrs})
		case EventKindCounter:
			attrs := sortedAttrMap(event.Attributes)
			counters = append(counters, ProfileCounter{Name: event.CounterName, Value: event.CounterValue, Unit: event.Unit, Attrs: attrs})
			resourceValues[event.CounterName] += event.CounterValue
			applyRedactionCounter(&redaction, event, attrs)
		case EventKindError:
			if event.SafeError != nil {
				errors = append(errors, *event.SafeError)
			}
		}
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].Stage != spans[j].Stage {
			return spans[i].Stage < spans[j].Stage
		}
		if spans[i].SafeSubjectID != spans[j].SafeSubjectID {
			return spans[i].SafeSubjectID < spans[j].SafeSubjectID
		}
		return spans[i].SpanID < spans[j].SpanID
	})
	sort.Slice(counters, func(i, j int) bool {
		if counters[i].Name != counters[j].Name {
			return counters[i].Name < counters[j].Name
		}
		return fmt.Sprint(counters[i].Attrs) < fmt.Sprint(counters[j].Attrs)
	})
	sort.Slice(errors, func(i, j int) bool {
		if errors[i].Stage != errors[j].Stage {
			return errors[i].Stage < errors[j].Stage
		}
		return errors[i].Code < errors[j].Code
	})
	return spans, counters, resourcesFromCounters(resourceValues), redaction, errors
}

func applyRedactionCounter(redaction *RedactionMetrics, event Event, attrs map[string]string) {
	switch event.CounterName {
	case CounterRedactionEntriesScanned:
		redaction.EntriesScanned += event.CounterValue
	case CounterRedactionBytesScanned:
		redaction.BytesScanned += event.CounterValue
	case CounterRedactionFindings:
		redaction.FindingsByCategory[attrs[string(AttrCategory)]] += event.CounterValue
	case CounterRedactionRulesMatched:
		redaction.RulesMatched[attrs[string(AttrRuleID)]] += event.CounterValue
	case CounterRedactionFailures:
		redaction.Failures += event.CounterValue
	}
}

func resourcesFromCounters(values map[CounterName]int64) ResourceMetrics {
	return ResourceMetrics{DBReads: ptrIfPresent(values, CounterPushDBReads), HTTPRequests: ptrIfPresent(values, CounterPushHTTPRequests), HTTPRetries: ptrIfPresent(values, CounterPushHTTPRetries), PayloadBytes: ptrIfPresent(values, CounterPushPayloadBytes), ResponseBytes: ptrIfPresent(values, CounterPushResponseBytes), AllocBytes: nil, ConcurrencyHighWater: ptrIfPresent(values, CounterPushConcurrencyHighWater)}
}

func ptrIfPresent(values map[CounterName]int64, name CounterName) *int64 {
	if value, ok := values[name]; ok {
		return &value
	}
	return nil
}

func minValue(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	return values[0]
}
func maxValue(values []int64) int64 {
	if len(values) == 0 {
		return 0
	}
	return values[len(values)-1]
}

func percentileValue(values []int64, p int) int64 {
	if len(values) == 0 {
		return 0
	}
	rank := (p*len(values) + 99) / 100
	if rank < 1 {
		rank = 1
	}
	if rank > len(values) {
		rank = len(values)
	}
	return values[rank-1]
}

func bottlenecks(stages []StageSummary) []BottleneckSummary {
	var grand int64
	for _, stage := range stages {
		grand += stage.TotalMs
	}
	out := make([]BottleneckSummary, 0, len(stages))
	for _, stage := range stages {
		share := 0.0
		if grand > 0 {
			share = float64(stage.TotalMs) / float64(grand)
		}
		out = append(out, BottleneckSummary{Stage: stage.Stage, TotalMs: stage.TotalMs, ShareOfRun: share})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TotalMs != out[j].TotalMs {
			return out[i].TotalMs > out[j].TotalMs
		}
		return out[i].Stage < out[j].Stage
	})
	if len(out) > 5 {
		return out[:5]
	}
	return out
}
