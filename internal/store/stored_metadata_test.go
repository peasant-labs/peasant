package store_test

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"zombiezen.com/go/sqlite/sqlitex"
)

func TestStoredMetadataRecoveryPreservesSparseEvidence(t *testing.T) {
	fixture := loadArtifactMirrorFixtures(t)
	db := openTestStore(t)
	entry := makeStoreEntry(t, fixture.SessionID, fixture.ProjectHash, fixture.HostSlug, defaults.HarnessOpenCode, fixture.StartedAt, 100, 50)
	entry.Session.Origin = fixture.OriginalOrigin
	if err := db.InsertSessions(t.Context(), []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertSessionCommits(t.Context(), entry.Metadata.SessionID, []ingest.CommitInfo{{Hash: fixture.OriginalCommit}}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertOpenCodeSeqCursor(t.Context(), entry.Metadata.SessionID, fixture.OriginalCursor); err != nil {
		t.Fatal(err)
	}
	computed, err := db.GetMetrics(t.Context(), entry.Metadata.SessionID)
	if err != nil || computed == nil {
		t.Fatalf("read original metrics: %v", err)
	}
	version := 1
	computed.ComputeVersion = &version
	computed.ComputedAt = &fixture.StartedAt
	if err := db.SaveMetrics(t.Context(), computed); err != nil {
		t.Fatal(err)
	}
	// Historical metadata can lack a retained seed while computed metrics exist.
	// Recovery must not relabel those computed values as adapter evidence.
	conn := takeConn(t, db.Pool())
	err = sqlitex.ExecuteTransient(conn, "UPDATE sessions SET metric_seed_json = NULL WHERE session_id = ?", &sqlitex.ExecOptions{Args: []any{fixture.SessionID}})
	db.Pool().Put(conn)
	if err != nil {
		t.Fatal(err)
	}
	before := mirrorDatabaseState(t, db.Pool(), fixture.SessionID)
	metadata, err := db.ReadStoredMetadata(t.Context(), entry.Metadata.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(metadata, []byte(fixture.Transcript))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(artifact.Metadata.Timestamp, entry.Metadata.Timestamp) || artifact.Metadata.Source != entry.Metadata.Source ||
		artifact.Metadata.Model != entry.Metadata.Model || artifact.Metadata.Project.Hash != entry.Metadata.Project.Hash ||
		artifact.Metadata.Project.FilePath != entry.Metadata.Project.FilePath || artifact.Metadata.HostSlug != entry.Metadata.HostSlug ||
		!reflect.DeepEqual(artifact.Metadata.Git.Commits, []ingest.CommitInfo{{Hash: fixture.OriginalCommit}}) {
		t.Fatalf("recorded context changed during recovery: %+v", artifact.Metadata)
	}
	output := filepath.Join(t.TempDir(), "managed")
	publisher, err := ingest.NewArtifactPublisher(&ingest.OSFileSystem{}, output, ingest.ArtifactPublisherOptions{Mirror: db})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := publisher.Observe(t.Context(), entry.Session, "")
	if err != nil {
		t.Fatal(err)
	}
	committed, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := publisher.Reconcile(t.Context(), committed)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(reconciled.MetadataJSON, &fields); err != nil {
		t.Fatal(err)
	}
	if fields["stats"] != nil || fields["adapterVersion"] != nil || fields["redaction"] != nil ||
		fields["diagnostics"] != nil || fields["subagents"] != nil || fields["cwd"] != nil || reconciled.MetricSeed() != nil {
		t.Fatal("publication fabricated unavailable historical metadata")
	}
	after := mirrorDatabaseState(t, db.Pool(), fixture.SessionID)
	if after.Hash == "" {
		t.Fatal("recovered artifact was not mirrored")
	}
	after.Hash = before.Hash // A newly validated artifact gets a new identity.
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("recovery changed acquired or missing evidence: before=%+v after=%+v", before, after)
	}
	metrics, err := db.GetMetrics(t.Context(), entry.Metadata.SessionID)
	if err != nil || !reflect.DeepEqual(computed, metrics) {
		t.Fatalf("recovery replaced existing computed metrics: %v", err)
	}
}
