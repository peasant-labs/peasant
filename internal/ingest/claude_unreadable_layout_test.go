package ingest_test

import (
	"context"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestClaudeAdapter_UnreadableLayoutParentDoesNotRelink is the case a stored
// chain must not answer. A readable layout parent proves the child was copied
// from another root, so the first discovery relinks the child. When that
// layout file cannot be read, the full walk has no chain to prove the copy.
// A nil chain is "not loaded" and would be refilled from the store, which
// would relink the child the walk refuses. An empty chain is the failed read.
func TestClaudeAdapter_UnreadableLayoutParentDoesNotRelink(t *testing.T) {
	ctx := context.Background()
	const ownerID = "11111111-1111-4111-8111-111111111111"
	const layoutID = "22222222-2222-4222-8222-222222222222"
	const childID = "agent-abc123"
	layoutPath := claudeEvidenceCacheRoot + "/-workspace/" + layoutID + ".jsonl"

	database := openEvidenceStore(t, storetest.CopyGoldenDB(t))
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	writeClaudeEvidenceFiles(t, fs, []claudeEvidenceFile{
		{
			Path: "-workspace/" + ownerID + ".jsonl",
			Lines: []string{
				`{"sessionId":"` + ownerID + `","uuid":"s1","type":"user","message":{"role":"user","content":"synthetic"}}`,
			},
		},
		{
			Path: "-workspace/" + layoutID + ".jsonl",
			Lines: []string{
				`{"sessionId":"` + layoutID + `","uuid":"s1","type":"user","message":{"role":"user","content":"synthetic"}}`,
				`{"sessionId":"` + ownerID + `","uuid":"command","parentUuid":"s1","type":"system","subtype":"local_command"}`,
				`{"sessionId":"` + ownerID + `","uuid":"fork-tail","parentUuid":"command","type":"assistant","message":{"role":"assistant","content":"synthetic"}}`,
			},
		},
		{
			Path: "-workspace/" + layoutID + "/subagents/" + childID + ".jsonl",
			Lines: []string{
				`{"sessionId":"` + ownerID + `","agentId":"abc123","isSidechain":true,"uuid":"agent-record","type":"assistant","message":{"role":"assistant","content":"synthetic"}}`,
			},
		},
	})
	adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
	ingest.AttachClaudeEvidenceCache(adapter, database)
	cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeEvidenceCacheRoot}, Enabled: true}

	first, err := adapter.Discover(ctx, cfg)
	if err != nil {
		t.Fatalf("first discovery: %v", err)
	}
	if got := discoveredParent(t, first, childID); got != ownerID {
		t.Fatalf("readable layout parent = %s, want %s; the history link did not relink the child, so the unreadable case would not prove a refusal", got, ownerID)
	}
	stored, unreadable, err := database.LoadClaudeIdentityChains(ctx, []ingest.ResolvedPath{ingest.ResolvedPath(layoutPath)})
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 0 || len(stored[ingest.ResolvedPath(layoutPath)]) == 0 {
		t.Fatalf("stored layout chain = %d links, unreadable = %v; a refill from an empty store would not relink the child", len(stored[ingest.ResolvedPath(layoutPath)]), unreadable)
	}
	before, err := database.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	path := ingest.ResolvedPath(layoutPath)
	beforeRow, found := before[path]
	if !found || beforeRow.ProofVersion != ingest.ClaudeIdentityProofVersion {
		t.Fatalf("layout evidence before failure missing or stale: found=%t proof=%d", found, beforeRow.ProofVersion)
	}

	// A changed mtime forces the re-mine. The planted read error is the only
	// difference from the discovery that just proved the link.
	fs.ModTimes[layoutPath] = time.Now().Add(time.Hour)
	fs.Fail(testutil.FSOpReadFile, layoutPath, os.ErrPermission)
	fs.ResetCounts()
	second, err := adapter.Discover(ctx, cfg)
	if err != nil {
		t.Fatalf("discovery with an unreadable layout parent: %v", err)
	}
	if got := discoveredParent(t, second, childID); got != layoutID {
		t.Fatalf("unreadable layout parent = %s, want %s; the stored chain answered a read the full walk refuses", got, layoutID)
	}
	if fs.ReadCount(layoutPath) == 0 {
		t.Fatal("second discovery did not read the layout parent; the cached chain was used without a failed re-mine")
	}
	after, err := database.LoadClaudeEvidence(ctx)
	if err != nil {
		t.Fatal(err)
	}
	afterRow, found := after[path]
	if !found || !reflect.DeepEqual(beforeRow, afterRow) {
		t.Fatalf("failed read changed stored layout evidence: found=%t before=%+v after=%+v", found, beforeRow, afterRow)
	}
	afterChains, unreadable, err := database.LoadClaudeIdentityChains(ctx, []ingest.ResolvedPath{path})
	if err != nil {
		t.Fatal(err)
	}
	if len(unreadable) != 0 || !reflect.DeepEqual(stored[path], afterChains[path]) {
		t.Fatalf("failed read changed stored layout chain: before=%v after=%v unreadable=%v", stored[path], afterChains[path], unreadable)
	}
}

func discoveredParent(t *testing.T, sessions []ingest.DiscoveredSession, id string) string {
	t.Helper()
	for _, session := range sessions {
		if session.SessionID.String() != id {
			continue
		}
		if session.ParentUUID == nil {
			return ""
		}
		return session.ParentUUID.String()
	}
	t.Fatalf("session %s was not discovered", id)
	return ""
}
