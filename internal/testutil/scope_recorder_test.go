package testutil

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/scoperecorder"
)

// TestScopeDoublesRecordEveryScopeBearingCall proves the two shared doubles
// record the scope they receive through the stdlib-only recorder leaf. The
// recorder is a single generic slice keyed by operation name, not one field per
// method: adding a scope-bearing store read adds a method and one Record call,
// never an enumerated assertion list.
func TestScopeDoublesRecordEveryScopeBearingCall(t *testing.T) {
	t.Parallel()
	ids := []ingest.SessionID{"11111111-1111-4111-8111-111111111111"}
	hashes := []ingest.ProjectHash{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	storeScope := ingest.StoredSessions(ids)
	projectScope := ingest.Projects(hashes)

	sessionStore := &StubSessionStore{}
	if err := sessionStore.CleanupOrphanProjects(context.Background(), projectScope); err != nil {
		t.Fatal(err)
	}
	metricsStore := NewStubMetricsStore()
	if _, err := metricsStore.ListStaleIndexSessions(context.Background(), nil, storeScope); err != nil {
		t.Fatal(err)
	}

	sessionCalls := sessionStore.ScopeCalls.Calls()
	if len(sessionCalls) != 1 {
		t.Fatalf("StubSessionStore recorded %d scope calls, want 1", len(sessionCalls))
	}
	if sessionCalls[0].Operation != "CleanupOrphanProjects" {
		t.Fatalf("recorded operation = %q, want CleanupOrphanProjects", sessionCalls[0].Operation)
	}
	if sessionCalls[0].Kind != scoperecorder.KindProjects {
		t.Fatalf("recorded kind = %v, want KindProjects", sessionCalls[0].Kind)
	}
	if sessionCalls[0].All {
		t.Fatal("recorded project call reports the full-store scope; it was narrowed")
	}
	if len(sessionCalls[0].ProjectHashes) != 1 || sessionCalls[0].ProjectHashes[0] != string(hashes[0]) {
		t.Fatalf("recorded project scope = %v, want %v", sessionCalls[0].ProjectHashes, hashes)
	}

	metricsCalls := metricsStore.ScopeCalls.Calls()
	if len(metricsCalls) != 1 {
		t.Fatalf("StubMetricsStore recorded %d scope calls, want 1", len(metricsCalls))
	}
	if metricsCalls[0].Operation != "ListStaleIndexSessions" {
		t.Fatalf("recorded operation = %q, want ListStaleIndexSessions", metricsCalls[0].Operation)
	}
	if metricsCalls[0].Kind != scoperecorder.KindStore {
		t.Fatalf("recorded kind = %v, want KindStore", metricsCalls[0].Kind)
	}
	if metricsCalls[0].All {
		t.Fatal("recorded store call reports the full-store scope; it was narrowed")
	}
	if len(metricsCalls[0].IDs) != 1 || metricsCalls[0].IDs[0] != string(ids[0]) {
		t.Fatalf("recorded store scope = %v, want %v", metricsCalls[0].IDs, ids)
	}

	// The recorder keeps call order and is resettable, so a test can assert
	// "no call after the refusal" by resetting between phases.
	sessionStore.ScopeCalls.Reset()
	if len(sessionStore.ScopeCalls.Calls()) != 0 {
		t.Fatal("Recorder.Reset left recorded calls")
	}
}
