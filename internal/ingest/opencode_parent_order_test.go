package ingest_test

import (
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

// TestCanonicalOpenCodeParentsEmittedBeforeChildren proves that discovery
// orders a subagent after its root even when the child's raw session ID sorts
// before the parent's. OpenCode session IDs are time-descending, so a child
// created after its parent sorts first by raw ID. The pipeline parent gate
// admits a subagent only after its root passed the selection filter, so a
// child emitted first would be marked unchanged in selected mode. The mounted
// dry run asserts both the root and the subagent are admitted.
func TestCanonicalOpenCodeParentsEmittedBeforeChildren(t *testing.T) {
	const (
		childID  = "ses_3cd91f52effeXd3QAJ54jOyzv5" // sorts before the parent by raw ID
		parentID = "ses_4cd91f52effeXd3QAJ54jOyzv6"
	)
	materialized := testfixture.MaterializeByName(t, "legacy-mounted-projection")
	databasePath := materialized.Path
	withCanonicalConnection(t, databasePath, func(connection *sqlite.Conn) error {
		return sqlitex.Execute(connection, `UPDATE session SET parent_id = ?2 WHERE id = ?1`, &sqlitex.ExecOptions{Args: []any{childID, parentID}})
	})

	root, err := ingest.NewResolvedPath(filepath.Dir(databasePath))
	if err != nil {
		t.Fatal(err)
	}
	environment := mountedCurrentEnvironment{"OPENCODE_DB": databasePath}
	adapterFactory := canonicalAdapterFactory(t, environment)

	adapter := adapterFactory(&ingest.OSFileSystem{}, testutil.NoGitResolver(), salt.Salt{})
	discovered, err := adapter.Discover(t.Context(), ingest.SourceConfig{Enabled: true, Paths: []ingest.ResolvedPath{root}})
	if err != nil {
		t.Fatal(err)
	}
	if len(discovered) != 2 {
		t.Fatalf("discovery returned %d sessions, want 2", len(discovered))
	}
	// The parent must be emitted before the child so the pipeline parent gate
	// can admit the child in selected mode.
	sawParent := false
	for _, session := range discovered {
		if string(session.SessionID) == childID && !sawParent {
			t.Fatalf("child %q emitted before parent %q; parent gate would drop it", childID, parentID)
		}
		if string(session.SessionID) == parentID {
			sawParent = true
		}
	}

	output, err := ingest.NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Selected mode: admit only root sessions; subagents inherit their root.
	selectedRoots := func(session ingest.DiscoveredSession) bool {
		return session.ParentUUID == nil
	}
	config := ingest.PipelineConfig{
		Sources:       map[ingest.Harness]ingest.SourceConfig{ingest.HarnessOpenCode: {Enabled: true, Paths: []ingest.ResolvedPath{root}}},
		OutputDir:     output,
		Parallelism:   1,
		DryRun:        true,
		SessionFilter: selectedRoots,
	}
	pipeline, err := ingest.NewPipeline(&ingest.OSFileSystem{}, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessOpenCode: adapterFactory}, config)
	if err != nil {
		t.Fatal(err)
	}
	result, err := pipeline.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if result.Summary.New != 2 || result.Summary.Unchanged != 0 {
		t.Fatalf("selected-mode admission summary=%+v, want the root and its subagent both admitted (New=2, Unchanged=0)", result.Summary)
	}
}
