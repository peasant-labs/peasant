// Package perf is a reusable, low-overhead timing harness for the peasant push
// path. It exists to measure before optimizing: it splits
// each village upload into connection-setup and server-processing time, times the
// per-session redaction and per-annotation-batch work, and rolls the samples up
// into a per-phase summary plus a per-upload JSONL log.
//
// Design goals:
//   - OFF by default. When timing is disabled the caller holds a Nop recorder,
//     whose methods are empty and inlinable — negligible overhead on the hot path.
//   - Dependency-free. perf imports only the standard library, so it can be reused
//     by the ingest pipeline later (out of scope here — do NOT wire ingest now)
//     without creating an import cycle.
//   - Concurrency-safe. Uploads run in parallel (errgroup), so the Collector and
//     the per-request UploadTrace both guard their state with a mutex; the package
//     is exercised under `go test -race`.
package perf

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ProfileAPIVersion = "1"
	JSONFormatVersion = 1
)

type StageID string

const (
	StagePushRun                StageID = "push.run"
	StagePushDiscovery          StageID = "push.discovery"
	StagePushSelection          StageID = "push.selection"
	StagePushSession            StageID = "push.session"
	StagePushSessionLoad        StageID = "push.session.load"
	StagePushSessionRedact      StageID = "push.session.redact"
	StagePushPayloadBuild       StageID = "push.payload.build"
	StagePushNegotiate          StageID = "push.negotiate"
	StagePushPublish            StageID = "push.publish"
	StagePushVisibilityUpdate   StageID = "push.visibility.update"
	StagePushReceiptPersist     StageID = "push.receipt.persist"
	StagePushAnnotationsPublish StageID = "push.annotations.publish"
	StagePushRetry              StageID = "push.retry"
	StageRedactionScan          StageID = "redaction.scan"
	StageRedactionRuleEvaluate  StageID = "redaction.rule.evaluate"
	StageRedactionApply         StageID = "redaction.apply"
)

func (s StageID) String() string { return string(s) }

func AllStageIDs() []StageID {
	return []StageID{StagePushRun, StagePushDiscovery, StagePushSelection, StagePushSession, StagePushSessionLoad, StagePushSessionRedact, StagePushPayloadBuild, StagePushNegotiate, StagePushPublish, StagePushVisibilityUpdate, StagePushReceiptPersist, StagePushAnnotationsPublish, StagePushRetry, StageRedactionScan, StageRedactionRuleEvaluate, StageRedactionApply}
}

func (s StageID) Validate() error {
	for _, valid := range AllStageIDs() {
		if s == valid {
			return nil
		}
	}
	return fmt.Errorf("validate profile stage: unknown stage %q; use one of the typed perf.Stage* constants so profile output remains stable", s)
}

type Outcome string

const (
	OutcomeOK      Outcome = "ok"
	OutcomeSkipped Outcome = "skipped"
	OutcomeFailed  Outcome = "failed"
)

func (o Outcome) String() string { return string(o) }

func (o Outcome) Validate() error {
	switch o {
	case OutcomeOK, OutcomeSkipped, OutcomeFailed:
		return nil
	default:
		return fmt.Errorf("validate profile outcome: unknown outcome %q; use perf.OutcomeOK, perf.OutcomeSkipped, or perf.OutcomeFailed", o)
	}
}

type Unit string

const (
	UnitCount        Unit = "count"
	UnitBytes        Unit = "bytes"
	UnitMilliseconds Unit = "milliseconds"
	UnitRequests     Unit = "requests"
)

func (u Unit) String() string { return string(u) }

func (u Unit) Validate() error {
	switch u {
	case UnitCount, UnitBytes, UnitMilliseconds, UnitRequests:
		return nil
	default:
		return fmt.Errorf("validate profile unit: unknown unit %q; use one of the typed perf.Unit* constants", u)
	}
}

type CounterName string

const (
	CounterPushSessionsSelected       CounterName = "push.sessions.selected"
	CounterPushSessionsPublished      CounterName = "push.sessions.published"
	CounterPushSessionsFailed         CounterName = "push.sessions.failed"
	CounterPushSessionsSkipped        CounterName = "push.sessions.skipped"
	CounterPushDBReads                CounterName = "push.db.reads"
	CounterPushHTTPRequests           CounterName = "push.http.requests"
	CounterPushHTTPResponses          CounterName = "push.http.responses"
	CounterPushHTTPRetries            CounterName = "push.http.retries"
	CounterPushPayloadBytes           CounterName = "push.payload.bytes"
	CounterPushResponseBytes          CounterName = "push.response.bytes"
	CounterPushVisibilityPatchRequest CounterName = "push.visibility.patch.requests"
	CounterPushAnnotationRequests     CounterName = "push.annotation.requests"
	CounterPushConcurrencyHighWater   CounterName = "push.concurrency.high_water"
	CounterRedactionEntriesScanned    CounterName = "redaction.entries.scanned"
	CounterRedactionBytesScanned      CounterName = "redaction.bytes.scanned"
	CounterRedactionFindings          CounterName = "redaction.findings"
	CounterRedactionRulesMatched      CounterName = "redaction.rules.matched"
	CounterRedactionReplacements      CounterName = "redaction.replacements"
	CounterRedactionFailures          CounterName = "redaction.failures"
)

func (n CounterName) String() string { return string(n) }

func AllCounterNames() []CounterName {
	return []CounterName{CounterPushSessionsSelected, CounterPushSessionsPublished, CounterPushSessionsFailed, CounterPushSessionsSkipped, CounterPushDBReads, CounterPushHTTPRequests, CounterPushHTTPResponses, CounterPushHTTPRetries, CounterPushPayloadBytes, CounterPushResponseBytes, CounterPushVisibilityPatchRequest, CounterPushAnnotationRequests, CounterPushConcurrencyHighWater, CounterRedactionEntriesScanned, CounterRedactionBytesScanned, CounterRedactionFindings, CounterRedactionRulesMatched, CounterRedactionReplacements, CounterRedactionFailures}
}

func (n CounterName) Validate() error {
	for _, valid := range AllCounterNames() {
		if n == valid {
			return nil
		}
	}
	return fmt.Errorf("validate profile counter: unknown counter %q; use one of the typed perf.Counter* constants", n)
}

type AttributeKey string

const (
	AttrSafeSubjectID   AttributeKey = "safeSubjectId"
	AttrCategory        AttributeKey = "category"
	AttrRuleID          AttributeKey = "ruleId"
	AttrOperation       AttributeKey = "operation"
	AttrSelectionMode   AttributeKey = "selectionMode"
	AttrRedactionLevel  AttributeKey = "redaction.level"
	AttrSessionOrdinal  AttributeKey = "session.ordinal"
	AttrHTTPStatusClass AttributeKey = "http.status_class"
	AttrErrorCode       AttributeKey = "error.code"
)

type Attributes map[AttributeKey]string

type Counter struct {
	Name       CounterName
	Delta      int64
	Unit       Unit
	Attributes Attributes
}

type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

type TraceSink interface {
	WriteEvent(Event) error
	Close() error
}

type SummaryReducer interface {
	Reduce([]Event) (ProfileSummary, error)
}

type Span interface {
	End(Outcome, Attributes)
	ID() string
}

type EventKind string

const (
	EventKindSpanEnd EventKind = "span_end"
	EventKindCounter EventKind = "counter"
	EventKindError   EventKind = "error"
)

type Event struct {
	Kind         EventKind     `json:"kind"`
	Order        int64         `json:"order"`
	SpanID       string        `json:"spanId,omitempty"`
	ParentSpanID string        `json:"parentSpanId,omitempty"`
	Stage        StageID       `json:"stage,omitempty"`
	StartedAt    time.Time     `json:"startedAt,omitempty"`
	EndedAt      time.Time     `json:"endedAt,omitempty"`
	Duration     time.Duration `json:"-"`
	DurationMs   int64         `json:"durationMs,omitempty"`
	Outcome      Outcome       `json:"outcome,omitempty"`
	CounterName  CounterName   `json:"name,omitempty"`
	CounterValue int64         `json:"value,omitempty"`
	Unit         Unit          `json:"unit,omitempty"`
	Attributes   Attributes    `json:"attrs,omitempty"`
	SafeError    *SafeError    `json:"error,omitempty"`
}

type SafeError struct {
	Stage       StageID `json:"stage"`
	Code        string  `json:"code"`
	SafeMessage string  `json:"safeMessage"`
	Retryable   bool    `json:"retryable"`
}

// Error lets instrumentation pass a classified diagnostic through Recorder.Error.
// The collector still sanitizes its code and message at the recording boundary.
func (e SafeError) Error() string { return e.SafeMessage }

var _ error = SafeError{}

type Options struct {
	Enabled   bool
	Sanitizer Sanitizer
}

type Sanitizer struct{}

var unsafeValueRE = regexp.MustCompile(`(?i)([a-z]:\\|/[^\s]+|git@|https?://|-----BEGIN|password|branch\s+|commit\s+[0-9a-f]{7,40})`)
var unsafeErrorRE = regexp.MustCompile(`(?i)([a-z]:\\|/[^\s]+|git@|https?://|-----BEGIN|secret|token|password|branch\s+|commit\s+[0-9a-f]{7,40})`)
var safeTokenRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]*$`)

func (Sanitizer) SanitizeAttributes(attrs Attributes) Attributes {
	if len(attrs) == 0 {
		return nil
	}
	out := make(Attributes, len(attrs))
	for key, value := range attrs {
		if !isAllowedAttribute(key) {
			continue
		}
		trimmed := strings.TrimSpace(value)
		if trimmed == "" {
			continue
		}
		if sanitized, ok := sanitizeAttributeValue(key, trimmed); ok {
			out[key] = sanitized
			continue
		}
		if unsafeValueRE.MatchString(trimmed) {
			out[key] = "redacted"
			continue
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func sanitizeAttributeValue(key AttributeKey, value string) (string, bool) {
	switch key {
	case AttrSafeSubjectID:
		if strings.HasPrefix(value, "session:") && safeTokenRE.MatchString(value) {
			return value, true
		}
		return "redacted", true
	case AttrCategory:
		switch value {
		case "secrets", "pii", "paths", "project":
			return value, true
		default:
			return "redacted", true
		}
	case AttrRuleID, AttrOperation, AttrSelectionMode, AttrRedactionLevel, AttrHTTPStatusClass, AttrErrorCode:
		if safeTokenRE.MatchString(value) {
			return strings.ToLower(value), true
		}
		return "redacted", true
	case AttrSessionOrdinal:
		for _, r := range value {
			if r < '0' || r > '9' {
				return "redacted", true
			}
		}
		return value, true
	default:
		return "", false
	}
}

func (s Sanitizer) SafeError(stage StageID, code string, err error, retryable bool) SafeError {
	message := "Profiled operation failed during " + stage.String() + "; the profile records only a safe error code. Check the command output and service availability, then retry the operation."
	if err != nil && !unsafeErrorRE.MatchString(err.Error()) && len(err.Error()) <= 180 {
		message = err.Error() + "; this happened during " + stage.String() + ". Check the related service or input, then retry if it is safe."
	}
	return SafeError{Stage: stage, Code: sanitizeCode(code), SafeMessage: message, Retryable: retryable}
}

func isAllowedAttribute(key AttributeKey) bool {
	switch key {
	case AttrSafeSubjectID, AttrCategory, AttrRuleID, AttrOperation, AttrSelectionMode, AttrRedactionLevel, AttrSessionOrdinal, AttrHTTPStatusClass, AttrErrorCode:
		return true
	default:
		return false
	}
}

func sanitizeCode(code string) string {
	code = strings.TrimSpace(code)
	if code == "" || unsafeValueRE.MatchString(code) {
		return "profile_error"
	}
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			return r
		}
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return '_'
	}, code)
}

// Phase identifies a measured segment of a push run. It is a typed enum (not a
// bare string) so phases flow through the rollup typed and ast-grep can catch
// stray literals; String returns the bare label for log/rollup output.
type Phase string

const (
	// PhaseSetup is connection acquisition time for an upload: the interval
	// between httptrace GetConn and GotConn (TLS handshake + dial, or ~0 on a
	// reused connection). This is the signal for the connection-reuse win.
	PhaseSetup Phase = "setup"
	// PhaseServer is server-processing time for an upload: the interval between
	// the request being written (WroteRequest) and the first response byte
	// (GotFirstResponseByte). It excludes local connection setup.
	PhaseServer Phase = "server"
	// PhaseRedact is the per-session safety-net redaction time applied before
	// upload.
	PhaseRedact Phase = "redact"
	// PhaseAnnotation is the wall-clock time of a single annotation-batch upload.
	PhaseAnnotation Phase = "annotation"
)

// String returns the bare phase label for serialization / rollup output.
func (p Phase) String() string { return string(p) }

func (p Phase) StageID() StageID {
	switch p {
	case PhaseSetup:
		return StagePushPublish
	case PhaseServer:
		return StagePushPublish
	case PhaseRedact:
		return StagePushSessionRedact
	case PhaseAnnotation:
		return StagePushAnnotationsPublish
	default:
		return StageID("push." + p.String())
	}
}

// phaseOrder is the fixed display order for the rollup so output is deterministic
// regardless of the (concurrent, map-backed) order in which samples arrived.
var phaseOrder = []Phase{PhaseSetup, PhaseServer, PhaseRedact, PhaseAnnotation}

// UploadSample is one transcript upload's timing, keyed by session id. Setup and
// Server come from the httptrace split (see UploadTrace); Reused/WasIdle report
// whether the underlying TCP/TLS connection was reused (the steady-state goal).
// Connected is false when no connection was ever obtained (a transport failure
// before GotConn) — such samples are dropped from the rollup and the JSONL log so
// a failed dial is not mistaken for a 0ms server response.
type UploadSample struct {
	SessionID string
	Setup     time.Duration
	Server    time.Duration
	Reused    bool
	WasIdle   bool
	Connected bool
}

// Recorder is the collection seam called from the hot push path. Implementations:
//   - Nop (default, timing off): every method is a no-op.
//   - Collector (timing on): accumulates samples for an end-of-run rollup + JSONL.
//
// Recorder is intentionally narrow — it only COLLECTS. Reporting (rollup
// formatting, JSONL writing) lives on the concrete Collector so the interface
// stays trivial to satisfy and cheap to call.
type Recorder interface {
	StartSpan(stage StageID, attrs Attributes) Span
	StartChildSpan(stage StageID, parentSpanID string, attrs Attributes) Span
	Count(name CounterName, delta int64, unit Unit, attrs Attributes)
	Error(stage StageID, err error, attrs Attributes)
	// RecordPhase records one duration sample for a phase (e.g. a session's
	// redaction time, or one annotation batch's wall-clock).
	RecordPhase(phase Phase, d time.Duration)
	// RecordUpload records one transcript upload's timing. A Collector folds the
	// Setup/Server durations into the setup/server phases and retains the sample
	// for the per-upload JSONL log.
	RecordUpload(sample UploadSample)
	// Enabled reports whether recording is active. Hot paths use it to skip
	// building the (non-trivial) httptrace hooks when timing is off.
	Enabled() bool
}

// Nop is the zero-overhead Recorder used when --timing is off. Its methods are
// empty so the compiler can inline them away.
type Nop struct{}

// Compile-time guard: Nop satisfies Recorder.
var _ Recorder = Nop{}

// RecordPhase does nothing.
func (Nop) RecordPhase(Phase, time.Duration) {}

// RecordUpload does nothing.
func (Nop) RecordUpload(UploadSample) {}

// Enabled always reports false.
func (Nop) Enabled() bool { return false }

func (Nop) StartSpan(StageID, Attributes) Span { return nopSpan{} }
func (Nop) StartChildSpan(StageID, string, Attributes) Span {
	return nopSpan{}
}
func (Nop) Count(CounterName, int64, Unit, Attributes) {}
func (Nop) Error(StageID, error, Attributes)           {}

type nopSpan struct{}

func (nopSpan) End(Outcome, Attributes) {}
func (nopSpan) ID() string              { return "" }

// Collector is the active Recorder. It accumulates phase-duration samples and
// per-upload samples under a mutex (uploads run concurrently) and produces a
// Rollup and a JSONL log at end of run.
type Collector struct {
	mu        sync.Mutex
	phases    map[Phase][]time.Duration
	uploads   []UploadSample
	clock     Clock
	sink      TraceSink
	sanitizer Sanitizer
	nextID    atomic.Int64
	events    []Event
}

// Compile-time guard: *Collector satisfies Recorder.
var _ Recorder = (*Collector)(nil)

// NewCollector returns an empty, ready-to-use Collector.
func NewCollector() *Collector {
	return newCollector(realClock{}, nil, Options{Enabled: true})
}

func NewRecorder(clock Clock, sink TraceSink, opts Options) Recorder {
	if !opts.Enabled {
		return Nop{}
	}
	return newCollector(clock, sink, opts)
}

func NewCollectorWithOptions(clock Clock, sink TraceSink, opts Options) *Collector {
	if !opts.Enabled {
		opts.Enabled = true
	}
	return newCollector(clock, sink, opts)
}

func newCollector(clock Clock, sink TraceSink, opts Options) *Collector {
	if clock == nil {
		clock = realClock{}
	}
	return &Collector{phases: make(map[Phase][]time.Duration), clock: clock, sink: sink, sanitizer: opts.Sanitizer}
}

// Enabled always reports true for a Collector.
func (c *Collector) Enabled() bool { return true }

// RecordPhase appends a duration sample for the given phase.
func (c *Collector) RecordPhase(phase Phase, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases[phase] = append(c.phases[phase], d)
}

func (c *Collector) StartSpan(stage StageID, attrs Attributes) Span {
	return c.StartChildSpan(stage, "", attrs)
}

func (c *Collector) StartChildSpan(stage StageID, parentSpanID string, attrs Attributes) Span {
	if err := stage.Validate(); err != nil {
		c.Error(stage, err, attrs)
		return nopSpan{}
	}
	id := c.nextID.Add(1)
	return &collectorSpan{collector: c, spanID: fmt.Sprintf("span-%d", id), parentSpanID: sanitizeParentSpanID(parentSpanID), stage: stage, attrs: c.sanitizer.SanitizeAttributes(attrs), startedAt: c.clock.Now(), order: id}
}

func (c *Collector) Count(name CounterName, delta int64, unit Unit, attrs Attributes) {
	if err := name.Validate(); err != nil {
		c.Error(StagePushRun, err, attrs)
		return
	}
	if err := unit.Validate(); err != nil {
		c.Error(StagePushRun, err, attrs)
		return
	}
	c.appendEvent(Event{Kind: EventKindCounter, Order: c.nextID.Add(1), CounterName: name, CounterValue: delta, Unit: unit, Attributes: c.sanitizer.SanitizeAttributes(attrs)})
}

func (c *Collector) Error(stage StageID, err error, attrs Attributes) {
	safe := c.sanitizer.SafeError(stage, string(AttrErrorCode), err, false)
	var classified SafeError
	if errors.As(err, &classified) {
		safe = c.sanitizer.SafeError(stage, classified.Code, err, classified.Retryable)
	}
	c.appendEvent(Event{Kind: EventKindError, Order: c.nextID.Add(1), Stage: stage, Attributes: c.sanitizer.SanitizeAttributes(attrs), SafeError: &safe})
}

func (c *Collector) Events() []Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Event, len(c.events))
	copy(out, c.events)
	return out
}

func (c *Collector) appendEvent(event Event) {
	if event.Duration > 0 && event.DurationMs == 0 {
		event.DurationMs = durationMs(event.Duration)
	}
	c.mu.Lock()
	c.events = append(c.events, event)
	c.mu.Unlock()
	if c.sink != nil {
		_ = c.sink.WriteEvent(event)
	}
}

type collectorSpan struct {
	collector    *Collector
	spanID       string
	stage        StageID
	attrs        Attributes
	startedAt    time.Time
	order        int64
	once         sync.Once
	parentSpanID string
}

func (s *collectorSpan) End(outcome Outcome, attrs Attributes) {
	s.once.Do(func() {
		if err := outcome.Validate(); err != nil {
			s.collector.Error(s.stage, err, attrs)
			outcome = OutcomeFailed
		}
		endedAt := s.collector.clock.Now()
		merged := mergeAttrs(s.attrs, s.collector.sanitizer.SanitizeAttributes(attrs))
		s.collector.appendEvent(Event{Kind: EventKindSpanEnd, Order: s.order, SpanID: s.spanID, ParentSpanID: s.parentSpanID, Stage: s.stage, StartedAt: s.startedAt, EndedAt: endedAt, Duration: endedAt.Sub(s.startedAt), DurationMs: durationMs(endedAt.Sub(s.startedAt)), Outcome: outcome, Attributes: merged})
	})
}

func (s *collectorSpan) ID() string { return s.spanID }

func sanitizeParentSpanID(id string) string {
	if id == "" || unsafeValueRE.MatchString(id) {
		return ""
	}
	if safeTokenRE.MatchString(id) {
		return id
	}
	return ""
}

func mergeAttrs(a, b Attributes) Attributes {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(Attributes, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func durationMs(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	return d.Milliseconds()
}

func sortedAttrMap(attrs Attributes) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for key := range attrs {
		keys = append(keys, string(key))
	}
	sort.Strings(keys)
	out := make(map[string]string, len(attrs))
	for _, key := range keys {
		out[key] = attrs[AttributeKey(key)]
	}
	return out
}

// RecordUpload retains a transcript upload sample and, when the upload actually
// reached a server (Connected), folds its Setup/Server durations into the
// setup/server phase stats so the rollup reflects them. A non-Connected sample
// (transport failure before GotConn) is dropped entirely.
func (c *Collector) RecordUpload(sample UploadSample) {
	if !sample.Connected {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uploads = append(c.uploads, sample)
	c.phases[PhaseSetup] = append(c.phases[PhaseSetup], sample.Setup)
	c.phases[PhaseServer] = append(c.phases[PhaseServer], sample.Server)
}
