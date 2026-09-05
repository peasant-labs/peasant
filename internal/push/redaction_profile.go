package push

import (
	"context"
	"errors"
	"math"
	"sort"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/perf"
	"github.com/peasant-labs/redact"
)

// redactionReporter is an optional capability of the injected engine, not a new
// module contract. Report contains private matches and warnings: neither is read
// or forwarded. Only validated aggregate counts cross the profile boundary.
type redactionReporter interface {
	Report() redact.RedactionReport
}

var _ redactionReporter = (redact.Redactor)(nil)

// profiledRedactor times the existing combined engine calls without replacing
// their implementation or serializing concurrent sessions. It is scoped to one
// Run. The engine must not be shared with unrelated concurrent runs: its report
// is cumulative and cannot attribute matches to callers.
type profiledRedactor struct {
	ingest.TextRedactor
	rec    perf.Recorder
	parent string
}

var _ ingest.TextRedactor = (*profiledRedactor)(nil)

type redactionProfileOperation string

const (
	redactionReportUnavailable    redactionProfileOperation = "report_unavailable"
	redactionReportValidation     redactionProfileOperation = "report_validation"
	redactionEntriesValidation    redactionProfileOperation = "entries_validation"
	redactionMetadataValidation   redactionProfileOperation = "metadata_validation"
	redactionTranscriptValidation redactionProfileOperation = "transcript_validation"
	redactionMetadataScanApply    redactionProfileOperation = "metadata_scan_apply"
	redactionJSONScanApply        redactionProfileOperation = "json_scan_apply"
	redactionJSONStringValues     redactionProfileOperation = "json_string_values"
)

func (o redactionProfileOperation) attributes() perf.Attributes {
	return perf.Attributes{perf.AttrOperation: string(o)}
}

func profileRedactionRun(ctx context.Context, pipeline *Pipeline) (*Pipeline, func()) {
	rec := perf.RecorderFromContext(ctx)
	if !rec.Enabled() {
		return pipeline, func() {}
	}
	copy := *pipeline
	copy.redactor = &profiledRedactor{TextRedactor: pipeline.redactor, rec: rec, parent: perf.ParentSpanFromContext(ctx)}
	reporter, ok := pipeline.redactor.(redactionReporter)
	if !ok {
		// Timing still works for implementations of the narrower ingest seam.
		return &copy, func() {
			rec.Error(perf.StageRedactionApply, errors.New("Aggregate report unavailable at push redaction seam; findings were omitted. Use a report-capable engine for counts"), redactionReportUnavailable.attributes())
		}
	}
	before, valid := safeRedactionCounts(reporter.Report())
	return &copy, func() {
		after, afterValid := safeRedactionCounts(reporter.Report())
		if !valid || !afterValid || !monotonicRedactionCounts(before, after) {
			recordRedactionProfileFailure(rec, redactionReportValidation)
			return
		}
		// Sort before emitting so JSONL ordering does not inherit Go map order.
		ids := make([]string, 0, len(after))
		for id := range after {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		categories := make(map[redact.Category]int64)
		for _, id := range ids {
			count := after[id].count - before[id].count
			if count == 0 {
				continue
			}
			rec.Count(perf.CounterRedactionRulesMatched, count, perf.UnitCount, perf.Attributes{perf.AttrRuleID: id})
			categories[after[id].category] += count
		}
		for _, category := range redact.AllCategories() {
			if count := categories[category]; count > 0 {
				rec.Count(perf.CounterRedactionFindings, count, perf.UnitCount, perf.Attributes{perf.AttrCategory: category.String().String()})
			}
		}
	}
}

// profileRedactionSession binds context-free engine calls to this session using
// immutable copies. The engine and collector stay shared, with no lock around
// calls and no second aggregate report snapshot or flush.
func profileRedactionSession(ctx context.Context, pipeline *Pipeline) *Pipeline {
	redactor, ok := pipeline.redactor.(*profiledRedactor)
	if !ok {
		return pipeline
	}
	copy := *pipeline
	bound := *redactor
	bound.rec = perf.RecorderFromContext(ctx)
	bound.parent = perf.ParentSpanFromContext(ctx)
	copy.redactor = &bound
	return &copy
}

type safeRedactionCount struct {
	count    int64
	category redact.Category
}

func safeRedactionCounts(report redact.RedactionReport) (map[string]safeRedactionCount, bool) {
	categories := make(map[redact.Category]bool)
	for _, raw := range report.Categories {
		found := false
		for _, category := range redact.AllCategories() {
			if raw == string(category) {
				if category.Validate() != nil || categories[category] {
					return nil, false
				}
				categories[category] = true
				found = true
				break
			}
		}
		if !found {
			return nil, false
		}
	}
	// A syntactically safe token can itself be private text. Only IDs in the
	// engine's built-in catalogue are eligible, never arbitrary custom IDs.
	catalogue := make(map[string]redact.Category, len(redact.Rules))
	for _, rule := range redact.Rules {
		if rule.Category.Validate() != nil {
			return nil, false
		}
		catalogue[rule.ID] = rule.Category
	}
	counts := make(map[string]safeRedactionCount, len(report.Counts))
	observed := make(map[redact.Category]bool)
	var total int64
	for id, count := range report.Counts {
		category, known := catalogue[id]
		if !known || count <= 0 || !categories[category] || int64(count) > math.MaxInt64-total {
			return nil, false
		}
		counts[id] = safeRedactionCount{count: int64(count), category: category}
		observed[category] = true
		total += int64(count)
	}
	if total != int64(report.TotalRedactions) || report.TotalRedactions < 0 || len(observed) != len(categories) {
		return nil, false
	}
	return counts, true
}

func monotonicRedactionCounts(before, after map[string]safeRedactionCount) bool {
	for id, count := range before {
		if after[id].count < count.count {
			return false
		}
	}
	return true
}

func (r *profiledRedactor) RedactMetadata(meta *ingest.UnifiedMetadata) *ingest.UnifiedMetadata {
	span := r.rec.StartChildSpan(perf.StageRedactionApply, r.parent, redactionMetadataScanApply.attributes())
	// No metadata-byte estimate: the engine rewrites a hand-selected field list
	// after contextual normalization, not the metadata's serialized bytes.
	out := r.TextRedactor.RedactMetadata(meta)
	span.End(perf.OutcomeOK, nil)
	return out
}

func (r *profiledRedactor) RedactJSON(value any) any {
	r.rec.Count(perf.CounterRedactionBytesScanned, jsonStringBytes(value), perf.UnitBytes, redactionJSONStringValues.attributes())
	span := r.rec.StartChildSpan(perf.StageRedactionApply, r.parent, redactionJSONScanApply.attributes())
	out := r.TextRedactor.RedactJSON(value)
	span.End(perf.OutcomeOK, nil)
	return out
}

// jsonStringBytes counts the input the JSON engine scans: UTF-8 string values,
// excluding keys, structural JSON bytes and non-string scalars. Repeated passes
// count again, intentionally measuring work rather than unique content.
func jsonStringBytes(value any) int64 {
	switch value := value.(type) {
	case string:
		return int64(len(value))
	case []any:
		var total int64
		for _, item := range value {
			total += jsonStringBytes(item)
		}
		return total
	case map[string]any:
		var total int64
		for _, item := range value {
			total += jsonStringBytes(item)
		}
		return total
	default:
		return 0
	}
}

func recordRedactionProfileFailure(rec perf.Recorder, operation redactionProfileOperation) {
	if !rec.Enabled() {
		return
	}
	attrs := operation.attributes()
	rec.Count(perf.CounterRedactionFailures, 1, perf.UnitCount, attrs)
	// Never pass the original error to the general sanitizer: engine errors can
	// contain private text even if they look like ordinary words or safe tokens.
	message := "Push document validation failed after redaction; publication was refused. Correct the engine or custom rule and retry"
	if operation == redactionReportValidation {
		message = "Push redaction report validation failed; profile finding counts were withheld, not publication. Use supported rules and a consistent engine report, then retry profiling"
	}
	rec.Error(perf.StageRedactionApply, errors.New(message), attrs)
}

func observeRedactionDocument(redactor redact.JSONRedactor, err *error, operation redactionProfileOperation) {
	if *err == nil {
		return
	}
	if profiled, ok := redactor.(*profiledRedactor); ok {
		recordRedactionProfileFailure(profiled.rec, operation)
	}
}
