package push

import (
	"context"
	"math"
	"sort"

	"github.com/peasant-labs/peasant/internal/config"
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

// Only closed reasons and locally authored recovery text cross the error boundary.
// Report values and config conversion errors are never diagnostic prose. Validated
// configured IDs are intentionally reportable only as rule-counter metadata.
type redactionReportIssue string

const (
	redactionReportValid           redactionReportIssue = ""
	redactionReportUnknownRule     redactionReportIssue = "unknown_rule"
	redactionReportUnsafeRule      redactionReportIssue = "unsafe_rule"
	redactionReportConfig          redactionReportIssue = "config"
	redactionReportCustomCollision redactionReportIssue = "custom_collision"
	redactionReportCategory        redactionReportIssue = "category"
	redactionReportCount           redactionReportIssue = "count"
	redactionReportTotal           redactionReportIssue = "total"
	redactionReportDecreasing      redactionReportIssue = "decreasing"
)

func (issue redactionReportIssue) diagnostic() perf.SafeError {
	var message string
	switch issue {
	case redactionReportUnknownRule:
		message = "Push report has unsupported rule IDs; counts withheld, publication unchanged. Use a profile-supported engine and rule configuration."
	case redactionReportUnsafeRule:
		message = "Push report has unsafe rule IDs; counts withheld, publication unchanged. Use lowercase diagnostic IDs without paths, remotes, whitespace or control characters."
	case redactionReportConfig:
		message = "Push report configuration is invalid; counts withheld, publication unchanged. Supply the same valid custom pattern configuration used to construct the engine."
	case redactionReportCustomCollision:
		message = "Push report has a rule ID with conflicting categories; counts withheld, publication unchanged. Rename colliding custom rules before profiling again."
	case redactionReportCategory:
		message = "Push report categories are inconsistent; counts withheld, publication unchanged. Correct the engine report categories before profiling again."
	case redactionReportCount:
		message = "Push report counts are nonpositive or overflow; counts withheld, publication unchanged. Correct the engine report counts before profiling again."
	case redactionReportTotal:
		message = "Push report total differs from rule counts; counts withheld, publication unchanged. Correct the engine report total before profiling again."
	case redactionReportDecreasing:
		message = "Push report counts decreased during the run; counts withheld, publication unchanged. Use one cumulative engine owned by this run, then retry profiling."
	default:
		message = "Push report validation failed; counts withheld, publication unchanged. Correct the engine report before profiling again."
	}
	return perf.SafeError{Code: "redaction_report_" + string(issue), SafeMessage: message}
}

type redactionProfileCatalogue struct {
	categories map[string]redact.Category
	conflicts  map[string]bool
	issue      redactionReportIssue
}

func newRedactionProfileCatalogue(patterns []config.CustomPattern) redactionProfileCatalogue {
	// NewRedactor appends these exact engine-owned IDs to its instance rules,
	// not redact.Rules (redact.buildXDGRules). Only the patterns/replacements
	// depend on XDG paths. Never accept an ID prefix or derive IDs from paths.
	catalogue := redactionProfileCatalogue{
		categories: map[string]redact.Category{
			"xdg_data_home":   redact.CategoryPaths,
			"xdg_config_home": redact.CategoryPaths,
			"xdg_state_home":  redact.CategoryPaths,
		},
		conflicts: make(map[string]bool),
	}
	for _, rule := range redact.Rules {
		catalogue.categories[rule.ID] = rule.Category
	}
	// Use exactly the production conversion, never infer a category from an ID
	// or matched content. Its errors can include regex/config values: discard them.
	// The supplied config must describe the injected engine's custom patterns.
	custom, err := config.CustomPatternsToUserPatterns(patterns)
	if err != nil {
		catalogue.issue = redactionReportConfig
		return catalogue
	}
	for _, pattern := range custom {
		if category, exists := catalogue.categories[pattern.ID]; exists && category != pattern.Category {
			catalogue.conflicts[pattern.ID] = true
		}
		// Same-category collisions are honest combined match counts. A conflict
		// stays marked even if a later duplicate agrees with an earlier category.
		catalogue.categories[pattern.ID] = pattern.Category
	}
	return catalogue
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
			rec.Error(perf.StageRedactionApply, perf.SafeError{Code: "redaction_report_unavailable", SafeMessage: "Aggregate report unavailable at push redaction seam; findings were omitted. Use a report-capable engine for counts"}, redactionReportUnavailable.attributes())
		}
	}
	catalogue := newRedactionProfileCatalogue(pipeline.cfg.Redaction.CustomPatterns)
	before, issue := safeRedactionCounts(reporter.Report(), catalogue)
	return &copy, func() {
		after, afterIssue := safeRedactionCounts(reporter.Report(), catalogue)
		if issue == redactionReportValid {
			issue = afterIssue
		}
		if issue == redactionReportValid && !monotonicRedactionCounts(before, after) {
			issue = redactionReportDecreasing
		}
		if issue != redactionReportValid {
			attrs := redactionReportValidation.attributes()
			rec.Count(perf.CounterRedactionFailures, 1, perf.UnitCount, attrs)
			rec.Error(perf.StageRedactionApply, issue.diagnostic(), attrs)
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

func safeRedactionCounts(report redact.RedactionReport, catalogue redactionProfileCatalogue) (map[string]safeRedactionCount, redactionReportIssue) {
	if catalogue.issue != redactionReportValid {
		return nil, catalogue.issue
	}
	categories := make(map[redact.Category]bool)
	for _, raw := range report.Categories {
		found := false
		for _, category := range redact.AllCategories() {
			if raw == string(category) {
				if category.Validate() != nil || categories[category] {
					return nil, redactionReportCategory
				}
				categories[category] = true
				found = true
				break
			}
		}
		if !found {
			return nil, redactionReportCategory
		}
	}
	for _, category := range catalogue.categories {
		if category.Validate() != nil {
			return nil, redactionReportCategory
		}
	}
	counts := make(map[string]safeRedactionCount, len(report.Counts))
	observed := make(map[redact.Category]bool)
	var total int64
	// Stable validation order gives deterministic reasons for malformed reports.
	ids := make([]string, 0, len(report.Counts))
	for id := range report.Counts {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		count := report.Counts[id]
		category, known := catalogue.categories[id]
		if !known {
			return nil, redactionReportUnknownRule
		}
		// Require lossless passage through the existing diagnostic-ID boundary.
		// Do not merge malformed names under a sanitizer placeholder or trimmed ID.
		attrs := (perf.Sanitizer{}).SanitizeAttributes(perf.Attributes{perf.AttrRuleID: id})
		if id == "" || attrs[perf.AttrRuleID] != id {
			return nil, redactionReportUnsafeRule
		}
		if catalogue.conflicts[id] {
			return nil, redactionReportCustomCollision
		}
		if count <= 0 || int64(count) > math.MaxInt64-total {
			return nil, redactionReportCount
		}
		if !categories[category] {
			return nil, redactionReportCategory
		}
		counts[id] = safeRedactionCount{count: int64(count), category: category}
		observed[category] = true
		total += int64(count)
	}
	if total != int64(report.TotalRedactions) || report.TotalRedactions < 0 {
		return nil, redactionReportTotal
	}
	if len(observed) != len(categories) {
		return nil, redactionReportCategory
	}
	return counts, redactionReportValid
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
	rec.Error(perf.StageRedactionApply, perf.SafeError{Code: "redaction_" + string(operation), SafeMessage: message}, attrs)
}

func observeRedactionDocument(redactor redact.JSONRedactor, err *error, operation redactionProfileOperation) {
	if *err == nil {
		return
	}
	if profiled, ok := redactor.(*profiledRedactor); ok {
		recordRedactionProfileFailure(profiled.rec, operation)
	}
}
