package codemap

import (
	"context"
	"testing"

	"github.com/peasant-labs/peasant/internal/codegraph"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/gitops"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

// TestResolveProject_LegacyLabelResolvesEveryStoredProject proves that an
// ingested project cannot be hidden from project resolution by the saved
// kickstart selection. The saved selection controls future ingestion, so a
// legacy human-readable label for a stored project must resolve to its
// canonical hash exactly like an explicitly listed project does.
func TestResolveProject_LegacyLabelResolvesEveryStoredProject(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)

	projectHash, err := schema.NewProjectHash("2223456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("fixture project hash: %v", err)
	}
	sessionID, err := schema.NewSessionID("99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("fixture session ID: %v", err)
	}
	const projectPath = "/work/legacy-label-project"
	ingestedMs := int64(1501)
	metadata := &schema.UnifiedMetadata{
		SessionID:    sessionID,
		ModelHarness: defaults.HarnessClaudeCode,
		Model:        testutil.TestModel,
		HostSlug:     schema.HostSlug(testutil.TestHostSlug),
		Project:      schema.ProjectContext{Hash: projectHash, Name: "project", FilePath: projectPath},
		Timestamp:    schema.TimestampInfo{Start: 1000, End: 1500, Ingested: &ingestedMs},
		Source:       schema.SourceInfo{FilePath: "/legacy-label.jsonl", Format: schema.SourceFormatJSONL},
	}
	if err := database.InsertSessions(context.Background(), []ingest.StoreEntry{{Metadata: metadata}}); err != nil {
		t.Fatalf("seed stored project: %v", err)
	}

	service := NewService(database, func(string) gitops.Repository { return testutil.NoGitRepository() }, codegraph.NewGraphBuilder())

	resolved, err := service.ResolveProject(context.Background(), projectPath)
	if err != nil {
		t.Fatalf("ResolveProject(%q) returned %v, want the stored project's canonical hash; the saved kickstart selection must not hide an ingested project from the viewer", projectPath, err)
	}
	if resolved.ProjectHash != projectHash {
		t.Fatalf("ResolveProject(%q) hash = %s, want %s", projectPath, resolved.ProjectHash, projectHash)
	}

	if _, err := service.ResolveProject(context.Background(), "/work/not-a-stored-project"); err == nil {
		t.Fatal("ResolveProject(nonexistent label) unexpectedly succeeded; a genuinely unknown label must still report not-found")
	}
}
