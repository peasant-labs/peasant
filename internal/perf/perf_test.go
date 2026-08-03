package perf_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/perf"
)

// --- Phase enum ---

// TestPhase_String verifies the typed Phase enum renders its bare label.
func TestPhase_String(t *testing.T) {
	t.Parallel()
	cases := map[perf.Phase]string{
		perf.PhaseSetup:      "setup",
		perf.PhaseServer:     "server",
		perf.PhaseRedact:     "redact",
		perf.PhaseAnnotation: "annotation",
	}
	for phase, want := range cases {
		if got := phase.String(); got != want {
			t.Errorf("Phase(%v).String() = %q, want %q", phase, got, want)
		}
	}
}

// --- Nop recorder ---

// TestNop_NoOp verifies the default recorder reports disabled and accepts calls
// without panicking or producing state — the off-by-default contract.
func TestNop_NoOp(t *testing.T) {
	t.Parallel()
	var rec perf.Recorder = perf.Nop{}
	if rec.Enabled() {
		t.Error("Nop.Enabled() = true, want false")
	}
	// Must not panic.
	rec.RecordPhase(perf.PhaseRedact, 5*time.Millisecond)
	rec.RecordUpload(perf.UploadSample{SessionID: "s1", Connected: true, Setup: time.Millisecond})
}

// TestRecorderFromContext_DefaultsToNop verifies an un-instrumented context
// yields a Nop recorder (never nil) so hot paths can record unconditionally.
func TestRecorderFromContext_DefaultsToNop(t *testing.T) {
	t.Parallel()
	rec := perf.RecorderFromContext(context.Background())
	if rec == nil {
		t.Fatal("RecorderFromContext returned nil")
	}
	if rec.Enabled() {
		t.Error("default recorder should be disabled (Nop)")
	}
}

// TestContextWithRecorder_RoundTrip verifies a Collector threaded through context
// is the same instance read back, and that it reports enabled.
func TestContextWithRecorder_RoundTrip(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	ctx := perf.ContextWithRecorder(context.Background(), c)
	got := perf.RecorderFromContext(ctx)
	if !got.Enabled() {
		t.Error("recorder from context should be enabled")
	}
	got.RecordPhase(perf.PhaseRedact, 3*time.Millisecond)
	if n := c.Rollup().Phases; len(n) != 1 || n[0].Phase != perf.PhaseRedact {
		t.Errorf("recording through context did not reach the collector: %+v", n)
	}
}

// --- Collector rollup ---

// TestCollector_RollupPercentiles verifies count, nearest-rank p50/p95, total,
// and the percent split across phases.
func TestCollector_RollupPercentiles(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	// redact: 10,20,30,40,50 ms  → p50=30, p95=50, total=150
	for _, ms := range []int{30, 10, 50, 20, 40} { // unsorted on purpose
		c.RecordPhase(perf.PhaseRedact, time.Duration(ms)*time.Millisecond)
	}
	// annotation: single 50ms batch → total=50
	c.RecordPhase(perf.PhaseAnnotation, 50*time.Millisecond)

	r := c.Rollup()
	if len(r.Phases) != 2 {
		t.Fatalf("expected 2 phases, got %d: %+v", len(r.Phases), r.Phases)
	}

	var redact perf.PhaseStat
	found := false
	for _, p := range r.Phases {
		if p.Phase == perf.PhaseRedact {
			redact, found = p, true
		}
	}
	if !found {
		t.Fatal("redact phase missing from rollup")
	}
	if redact.Count != 5 {
		t.Errorf("redact count = %d, want 5", redact.Count)
	}
	if redact.P50 != 30*time.Millisecond {
		t.Errorf("redact p50 = %v, want 30ms", redact.P50)
	}
	if redact.P95 != 50*time.Millisecond {
		t.Errorf("redact p95 = %v, want 50ms", redact.P95)
	}
	if redact.Total != 150*time.Millisecond {
		t.Errorf("redact total = %v, want 150ms", redact.Total)
	}
	// Grand total = 150 + 50 = 200; redact share = 75%.
	if redact.Percent < 74.9 || redact.Percent > 75.1 {
		t.Errorf("redact percent = %.2f, want ~75.0", redact.Percent)
	}
}

// TestCollector_RollupDeterministicOrder verifies phases always render in the
// fixed setup→server→redact→annotation order regardless of arrival order.
func TestCollector_RollupDeterministicOrder(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	c.RecordPhase(perf.PhaseAnnotation, time.Millisecond)
	c.RecordPhase(perf.PhaseRedact, time.Millisecond)
	c.RecordUpload(perf.UploadSample{SessionID: "s", Connected: true, Setup: time.Millisecond, Server: time.Millisecond})

	r := c.Rollup()
	want := []perf.Phase{perf.PhaseSetup, perf.PhaseServer, perf.PhaseRedact, perf.PhaseAnnotation}
	if len(r.Phases) != len(want) {
		t.Fatalf("phase count = %d, want %d", len(r.Phases), len(want))
	}
	for i, p := range r.Phases {
		if p.Phase != want[i] {
			t.Errorf("phase[%d] = %s, want %s", i, p.Phase, want[i])
		}
	}
}

// TestCollector_DropsUnconnectedUploads verifies a transport failure before
// GotConn (Connected=false) is excluded from both the rollup and the upload set
// so it is not mistaken for a 0ms server response.
func TestCollector_DropsUnconnectedUploads(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	c.RecordUpload(perf.UploadSample{SessionID: "ok", Connected: true, Setup: 2 * time.Millisecond, Server: 3 * time.Millisecond, Reused: true})
	c.RecordUpload(perf.UploadSample{SessionID: "fail", Connected: false})

	r := c.Rollup()
	if r.UploadCount != 1 {
		t.Errorf("UploadCount = %d, want 1 (unconnected dropped)", r.UploadCount)
	}
	if r.ReusedCount != 1 {
		t.Errorf("ReusedCount = %d, want 1", r.ReusedCount)
	}
}

// --- Rollup formatting ---

// TestWriteRollup_Format verifies the stderr rollup is greppable and includes the
// per-phase count/p50/p95/total/percent and the reuse line.
func TestWriteRollup_Format(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	c.RecordUpload(perf.UploadSample{SessionID: "s1", Connected: true, Setup: 5 * time.Millisecond, Server: 20 * time.Millisecond, Reused: true})
	c.RecordPhase(perf.PhaseRedact, 8*time.Millisecond)

	var buf bytes.Buffer
	if err := perf.WriteRollup(&buf, c.Rollup()); err != nil {
		t.Fatalf("WriteRollup: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"per-phase rollup", "setup", "server", "redact", "count=", "p50=", "p95=", "uploads=1", "reused=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("rollup output missing %q:\n%s", want, out)
		}
	}
}

// --- JSONL log ---

// TestWriteJSONL_LineShape verifies one JSON object per Connected upload with the
// exact field names (sessionId/setupMs/serverMs/reused).
func TestWriteJSONL_LineShape(t *testing.T) {
	t.Parallel()
	c := perf.NewCollector()
	c.RecordUpload(perf.UploadSample{SessionID: "sess-a", Connected: true, Setup: 1500 * time.Microsecond, Server: 42 * time.Millisecond, Reused: false})
	c.RecordUpload(perf.UploadSample{SessionID: "sess-b", Connected: true, Setup: 0, Server: 10 * time.Millisecond, Reused: true})
	c.RecordUpload(perf.UploadSample{SessionID: "dropped", Connected: false})

	var buf bytes.Buffer
	if err := perf.WriteJSONL(&buf, c); err != nil {
		t.Fatalf("WriteJSONL: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines (unconnected dropped), got %d:\n%s", len(lines), buf.String())
	}

	var first struct {
		SessionID string  `json:"sessionId"`
		SetupMs   float64 `json:"setupMs"`
		ServerMs  float64 `json:"serverMs"`
		Reused    bool    `json:"reused"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal line 0: %v\n%s", err, lines[0])
	}
	if first.SessionID != "sess-a" {
		t.Errorf("line0 sessionId = %q, want sess-a", first.SessionID)
	}
	if first.SetupMs != 1.5 {
		t.Errorf("line0 setupMs = %v, want 1.5", first.SetupMs)
	}
	if first.ServerMs != 42 {
		t.Errorf("line0 serverMs = %v, want 42", first.ServerMs)
	}
	if first.Reused {
		t.Error("line0 reused = true, want false")
	}
	// Ensure exact key spelling on the raw wire.
	if !strings.Contains(lines[1], `"reused":true`) {
		t.Errorf("line1 missing reused:true: %s", lines[1])
	}
}

// --- httptrace setup/server/reused split via a real server ---

// TestUploadTrace_SplitViaHTTPTest drives a real httptest server through the
// UploadTrace hooks and asserts: (1) a connection is obtained (Connected) on the
// first request with Reused=false, and (2) a second request over the same client
// reuses the connection (Reused=true). It also asserts server_ms reflects a
// deliberate server delay. This exercises the production httptrace seam, not a
// mock.
func TestUploadTrace_SplitViaHTTPTest(t *testing.T) {
	t.Parallel()

	const serverDelay = 30 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(serverDelay) // delay BEFORE first byte → counts as server_ms
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	client := srv.Client()

	doOnce := func(t *testing.T) perf.UploadSample {
		t.Helper()
		_, trace := perf.ContextWithUploadTrace(context.Background())
		ctx := httptrace.WithClientTrace(context.Background(), trace.ClientTrace())
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
		if err != nil {
			t.Fatalf("new request: %v", err)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("do: %v", err)
		}
		// Drain + close so the connection returns to the pool for reuse.
		_, _ = bytes.NewBuffer(nil).ReadFrom(resp.Body)
		_ = resp.Body.Close()
		return trace.Sample("sess-x")
	}

	first := doOnce(t)
	if !first.Connected {
		t.Fatal("first request: Connected=false, want true")
	}
	if first.Reused {
		t.Error("first request: Reused=true, want false (fresh connection)")
	}
	if first.Server < serverDelay {
		t.Errorf("first request: server_ms = %v, want >= %v (server delay)", first.Server, serverDelay)
	}

	second := doOnce(t)
	if !second.Connected {
		t.Fatal("second request: Connected=false, want true")
	}
	if !second.Reused {
		t.Error("second request: Reused=false, want true (connection should be reused)")
	}
}
