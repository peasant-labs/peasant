package testutil

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
)

// TestScopeDoublesRecordEveryScopeBearingCall proves the two shared doubles
// record the scope they receive. The recorder is a single generic slice keyed
// by operation name, not one field per method: adding a scope-bearing store
// read adds a method and one Record call, never an enumerated assertion list.
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
	if sessionCalls[0].Projects == nil {
		t.Fatal("recorded project call carries no ProjectScope")
	}
	gotHashes, narrowed := sessionCalls[0].Projects.Narrowed()
	if !narrowed || len(gotHashes) != 1 || gotHashes[0] != hashes[0] {
		t.Fatalf("recorded project scope = %v, want %v", gotHashes, hashes)
	}

	metricsCalls := metricsStore.ScopeCalls.Calls()
	if len(metricsCalls) != 1 {
		t.Fatalf("StubMetricsStore recorded %d scope calls, want 1", len(metricsCalls))
	}
	if metricsCalls[0].Operation != "ListStaleIndexSessions" {
		t.Fatalf("recorded operation = %q, want ListStaleIndexSessions", metricsCalls[0].Operation)
	}
	if metricsCalls[0].Store == nil {
		t.Fatal("recorded store call carries no StoreScope")
	}
	gotIDs, narrowed := metricsCalls[0].Store.Narrowed()
	if !narrowed || len(gotIDs) != 1 || gotIDs[0] != ids[0] {
		t.Fatalf("recorded store scope = %v, want %v", gotIDs, ids)
	}

	// The recorder keeps call order and is resettable, so a test can assert
	// "no call after the refusal" by resetting between phases.
	sessionStore.ScopeCalls.Reset()
	if len(sessionStore.ScopeCalls.Calls()) != 0 {
		t.Fatal("ScopeCallRecorder.Reset left recorded calls")
	}
}
