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
	"sync"
	"time"
)

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

// Collector is the active Recorder. It accumulates phase-duration samples and
// per-upload samples under a mutex (uploads run concurrently) and produces a
// Rollup and a JSONL log at end of run.
type Collector struct {
	mu      sync.Mutex
	phases  map[Phase][]time.Duration
	uploads []UploadSample
}

// Compile-time guard: *Collector satisfies Recorder.
var _ Recorder = (*Collector)(nil)

// NewCollector returns an empty, ready-to-use Collector.
func NewCollector() *Collector {
	return &Collector{phases: make(map[Phase][]time.Duration)}
}

// Enabled always reports true for a Collector.
func (c *Collector) Enabled() bool { return true }

// RecordPhase appends a duration sample for the given phase.
func (c *Collector) RecordPhase(phase Phase, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases[phase] = append(c.phases[phase], d)
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
