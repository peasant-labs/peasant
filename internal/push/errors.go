package push

import (
	"errors"
	"regexp"
	"sort"
)

// Package sentinel errors. Each error origin in pushSession wraps the matching
// sentinel (via fmt.Errorf("...: %w", ErrX)) so ClassifyPushError can categorize
// via errors.Is rather than fragile string matching.
var (
	// ErrNoModel marks a session skipped because its metadata recorded no model.
	// The push is refused client-side (before upload) so the village never sees a
	// modelless request that would 400. The root cause is an ingest gap.
	// The message follows C-actionable-errors (what / why / cause / how-to-fix).
	ErrNoModel = errors.New(
		"no model recorded in the session metadata; the village requires a model and " +
			"would reject the upload with HTTP 400, so peasant skips it client-side before " +
			"uploading — this is a known ingest gap (the model was not captured at ingest time), " +
			"re-ingest the session after model metadata is available, or exclude it " +
			"from your push selection")
	// ErrInvalidPublishBody marks a session whose mapped PublishRequest body fails
	// client-side schema validation against the same generated
	// publish-request schema the village vendors + enforces — so the village would
	// answer the upload with a documented schema-422. The push is refused
	// client-side (before upload) as a pre-flight; the village remains the
	// authority. The wrapped validator error carries the specific failure (e.g.
	// "missing properties: 'harness'" or the harness enum). Message follows
	// C-actionable-errors (what / why / cause / how-to-fix).
	ErrInvalidPublishBody = errors.New(
		"the session's publish body failed client-side schema validation against the " +
			"publish contract the village enforces (the model object and its harness + model " +
			"fields are required); peasant rejects it before upload to avoid a doomed round-trip " +
			"that the village would answer with HTTP 422 — re-ingest the session so model.harness " +
			"and model.model are captured, or exclude it from your push selection")
	// ErrMetadataMissing marks a session whose metadata.json could not be read.
	// One cause is not discoverable from the path and has a specific repair, so
	// it is diagnosed separately: see redactionPlaceholder.
	ErrMetadataMissing = errors.New("metadata file missing or unreadable")
	// ErrVillageRejected marks an upload the village answered with a non-2xx status.
	ErrVillageRejected = errors.New("village rejected upload")
	// ErrNetwork marks a transport-level failure reaching the village.
	ErrNetwork = errors.New("network failure")
)

// redactionPlaceholder matches a redaction substitution left in a value that is
// meant to be a literal identifier, such as "<HIGH_ENTROPY>" in a stored host
// slug. Redaction is the only thing that puts angle brackets in a host slug: the
// slug charset admits them for exactly that reason and nothing else, so their
// presence is positive evidence rather than a heuristic.
var redactionPlaceholder = regexp.MustCompile(`<[A-Z0-9_]+>`)

// PushErrorCategory is a typed, closed enum of push-failure categories used to
// group per-session errors into the human-readable error-summary table.
type PushErrorCategory string

const (
	// CategoryNoModel: session had no model recorded (ErrNoModel).
	CategoryNoModel PushErrorCategory = "no-model"
	// CategoryInvalidBody: mapped publish body failed client-side schema
	// validation (ErrInvalidPublishBody) — e.g. missing model.harness/model.
	CategoryInvalidBody PushErrorCategory = "invalid-body"
	// CategoryMetadataMissing: metadata.json could not be read (ErrMetadataMissing).
	CategoryMetadataMissing PushErrorCategory = "metadata-missing"
	// CategoryVillageRejected: village returned a non-2xx status (ErrVillageRejected).
	CategoryVillageRejected PushErrorCategory = "village-rejected"
	// CategoryNetwork: transport-level connection failure (ErrNetwork).
	CategoryNetwork PushErrorCategory = "network"
	// CategoryOther: anything not matched by a known sentinel.
	CategoryOther PushErrorCategory = "other"
)

// String implements fmt.Stringer, returning the stable enum value.
func (c PushErrorCategory) String() string { return string(c) }

// categoryOrder defines the deterministic declaration-order rank used as the
// secondary tie-break when two categories have equal counts. Lower sorts first.
var categoryOrder = map[PushErrorCategory]int{
	CategoryNoModel:         0,
	CategoryInvalidBody:     1,
	CategoryMetadataMissing: 2,
	CategoryVillageRejected: 3,
	CategoryNetwork:         4,
	CategoryOther:           5,
}

// ClassifyPushError maps an error to its PushErrorCategory using errors.Is
// against the package sentinels (NOT string matching). A nil error and any
// unrecognized error both classify as CategoryOther. The network check also
// falls back to isConnectionError so raw transport errors that were not wrapped
// with ErrNetwork are still grouped correctly.
func ClassifyPushError(err error) PushErrorCategory {
	switch {
	case err == nil:
		return CategoryOther
	case errors.Is(err, ErrNoModel):
		return CategoryNoModel
	case errors.Is(err, ErrInvalidPublishBody):
		return CategoryInvalidBody
	case errors.Is(err, ErrMetadataMissing):
		return CategoryMetadataMissing
	case errors.Is(err, ErrNetwork) || isConnectionError(err):
		return CategoryNetwork
	case errors.Is(err, ErrVillageRejected):
		return CategoryVillageRejected
	default:
		return CategoryOther
	}
}

// ErrorTypeCount is one row in the error-summary table: a category, how many
// sessions failed with it, and one example message for context.
type ErrorTypeCount struct {
	Category PushErrorCategory
	Count    int
	Example  string
}

// SummarizePushErrors groups a run's failed sessions by typed category. The
// result is sorted descending by Count, with a deterministic secondary tie-break
// on the category's declaration order, so identical inputs always render
// identically. Sessions that did not error are ignored.
func SummarizePushErrors(result *PushResult) []ErrorTypeCount {
	if result == nil {
		return nil
	}
	counts := make(map[PushErrorCategory]*ErrorTypeCount)
	for _, sr := range result.Sessions {
		if sr.Status != PushStatusError || sr.Error == nil {
			continue
		}
		cat := ClassifyPushError(sr.Error)
		if c, ok := counts[cat]; ok {
			c.Count++
		} else {
			counts[cat] = &ErrorTypeCount{
				Category: cat,
				Count:    1,
				Example:  sr.Error.Error(),
			}
		}
	}
	if len(counts) == 0 {
		return nil
	}
	out := make([]ErrorTypeCount, 0, len(counts))
	for _, c := range counts {
		out = append(out, *c)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count // descending by count
		}
		return categoryOrder[out[i].Category] < categoryOrder[out[j].Category]
	})
	return out
}
