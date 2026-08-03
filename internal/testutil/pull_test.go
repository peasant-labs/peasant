package testutil

import (
	"errors"
	"testing"

	"github.com/peasant-labs/schema"
)

// TestPullValueReExport_ZeroBehaviourChange pins the testutil village-pull
// constants that now RE-EXPORT the canonical YAML fixture `values` against their
// historical inlined literals. If the fixture's values.uuid_lower /
// values.village_host ever diverge from what testutil historically published,
// this fails loudly — the re-export must stay a zero-behaviour-change wiring.
func TestPullValueReExport_ZeroBehaviourChange(t *testing.T) {
	const (
		histVillageHost    = "village.example.com"
		histTranscriptUUID = "11111111-2222-3333-4444-555555555555"
	)
	if TestVillageHost != histVillageHost {
		t.Errorf("TestVillageHost = %q, want historical %q (fixture re-export drifted)", TestVillageHost, histVillageHost)
	}
	if TestTranscriptUUID != histTranscriptUUID {
		t.Errorf("TestTranscriptUUID = %q, want historical %q (fixture re-export drifted)", TestTranscriptUUID, histTranscriptUUID)
	}
	// The typed re-export must round-trip through the real constructor.
	if _, err := schema.NewTranscriptID(TestTranscriptUUID); err != nil {
		t.Errorf("re-exported TestTranscriptUUID is not a valid TranscriptID: %v", err)
	}
	if string(TestTranscriptID) != TestTranscriptUUID {
		t.Errorf("TestTranscriptID %q != TestTranscriptUUID %q", TestTranscriptID, TestTranscriptUUID)
	}
}

// TestFailingFS_PerMethodInjection verifies the FailingFS decorator injects an
// error on the chosen method and delegates the rest to its inner FS.
func TestFailingFS_PerMethodInjection(t *testing.T) {
	mem := NewMemFS()
	if err := mem.WriteFile("/a/b.txt", []byte("hi"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f := NewFailingFS(mem)
	boom := errors.New("boom")
	f.ReadFileErr = boom

	if _, err := f.ReadFile("/a/b.txt"); !errors.Is(err, boom) {
		t.Errorf("ReadFile error = %v, want boom", err)
	}
	// Non-injected methods delegate.
	if err := f.WriteFile("/a/c.txt", []byte("ok"), 0o600); err != nil {
		t.Errorf("WriteFile should delegate: %v", err)
	}
	if _, err := mem.ReadFile("/a/c.txt"); err != nil {
		t.Errorf("delegated write did not reach inner FS: %v", err)
	}
}

// TestFailingFS_RemoveAllSkipFirstN verifies the skip-first-N semantics used by
// the pull compensation-failure test: the first matching RemoveAll succeeds, the
// next fails.
func TestFailingFS_RemoveAllSkipFirstN(t *testing.T) {
	mem := NewMemFS()
	f := NewFailingFS(mem)
	boom := errors.New("removeall boom")
	f.RemoveAllErr = boom
	f.RemoveAllOnPaths = map[string]bool{"/target": true}
	f.RemoveAllSkipFirstN = 1

	if err := f.RemoveAll("/target"); err != nil {
		t.Errorf("first RemoveAll should be skipped (succeed): %v", err)
	}
	if err := f.RemoveAll("/target"); !errors.Is(err, boom) {
		t.Errorf("second RemoveAll should fail; got %v", err)
	}
	// A non-matching path always delegates.
	if err := f.RemoveAll("/other"); err != nil {
		t.Errorf("non-matching RemoveAll should succeed: %v", err)
	}
	if len(f.RemoveAllCalls) != 3 {
		t.Errorf("RemoveAllCalls = %d, want 3", len(f.RemoveAllCalls))
	}
}
