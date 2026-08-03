package perf

import (
	"context"
	"net/http/httptrace"
	"sync"
	"time"
)

// ctxKey is an unexported context key type so perf's context values cannot
// collide with another package's keys.
type ctxKey int

const (
	recorderKey ctxKey = iota
	uploadTraceKey
)

// ContextWithRecorder returns a child context carrying rec. The push path reads
// it back via RecorderFromContext, which lets a single --timing toggle at the CLI
// thread a Collector through the whole run without changing the signatures of the
// shared pipeline/annotation functions (keeping the change additive and the annotation path's
// rebase clean). Passing a nil recorder is treated as "no recorder".
func ContextWithRecorder(ctx context.Context, rec Recorder) context.Context {
	if rec == nil {
		return ctx
	}
	return context.WithValue(ctx, recorderKey, rec)
}

// RecorderFromContext returns the Recorder carried by ctx, or a Nop recorder when
// none is present. It never returns nil — callers can record unconditionally.
func RecorderFromContext(ctx context.Context) Recorder {
	if rec, ok := ctx.Value(recorderKey).(Recorder); ok && rec != nil {
		return rec
	}
	return Nop{}
}

// ContextWithUploadTrace returns a child context carrying a fresh UploadTrace and
// the trace itself. The HTTP client (VillageClient) attaches the trace's
// httptrace hooks when it finds one on the request context; the caller reads the
// populated trace back via Sample after the request completes. One trace is
// created per upload so concurrent uploads never share trace state.
func ContextWithUploadTrace(ctx context.Context) (context.Context, *UploadTrace) {
	t := &UploadTrace{}
	return context.WithValue(ctx, uploadTraceKey, t), t
}

// UploadTraceFromContext returns the UploadTrace carried by ctx, or nil when none
// is present (timing off). The client only builds httptrace hooks when this is
// non-nil, so an un-timed request pays no instrumentation cost.
func UploadTraceFromContext(ctx context.Context) *UploadTrace {
	if t, ok := ctx.Value(uploadTraceKey).(*UploadTrace); ok {
		return t
	}
	return nil
}

// UploadTrace captures the connection-setup and server-processing split for a
// single HTTP upload via net/http/httptrace. httptrace callbacks may fire on
// different goroutines than the caller (the request write loop vs. the read
// loop), so all fields are guarded by mu and read back through Sample under the
// same lock — the package is verified under `go test -race`.
type UploadTrace struct {
	mu         sync.Mutex
	getConnAt  time.Time
	wroteReqAt time.Time
	setup      time.Duration
	server     time.Duration
	reused     bool
	wasIdle    bool
	gotConn    bool
}

// ClientTrace returns the httptrace.ClientTrace whose callbacks populate t. Attach
// it to a request context with httptrace.WithClientTrace before issuing the
// request:
//
//	ctx = httptrace.WithClientTrace(ctx, trace.ClientTrace())
//
// The split separates orchestration from measured operations:
//   - setup  = GotConn − GetConn               (dial + TLS handshake; ~0 if reused)
//   - server = GotFirstResponseByte − WroteRequest (server processing)
func (t *UploadTrace) ClientTrace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		GetConn: func(string) {
			t.mu.Lock()
			t.getConnAt = time.Now()
			t.mu.Unlock()
		},
		GotConn: func(info httptrace.GotConnInfo) {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.getConnAt.IsZero() {
				t.setup = time.Since(t.getConnAt)
			}
			t.reused = info.Reused
			t.wasIdle = info.WasIdle
			t.gotConn = true
		},
		WroteRequest: func(httptrace.WroteRequestInfo) {
			t.mu.Lock()
			t.wroteReqAt = time.Now()
			t.mu.Unlock()
		},
		GotFirstResponseByte: func() {
			t.mu.Lock()
			defer t.mu.Unlock()
			if !t.wroteReqAt.IsZero() {
				t.server = time.Since(t.wroteReqAt)
			}
		},
	}
}

// Sample reads the populated trace back into an UploadSample keyed by sessionID.
// Connected is true only when a connection was actually obtained (GotConn fired),
// so a transport failure before connect yields Connected=false and is dropped by
// the Collector rather than logged as a 0ms upload.
func (t *UploadTrace) Sample(sessionID string) UploadSample {
	t.mu.Lock()
	defer t.mu.Unlock()
	return UploadSample{
		SessionID: sessionID,
		Setup:     t.setup,
		Server:    t.server,
		Reused:    t.reused,
		WasIdle:   t.wasIdle,
		Connected: t.gotConn,
	}
}
