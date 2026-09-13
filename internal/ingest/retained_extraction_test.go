package ingest

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"maps"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/adapter_transcript.yaml
var retainedExtractionYAML []byte

func TestRetainedMetadataPublicationPreservesContext(t *testing.T) {
	var fixture struct {
		Metadata        string   `yaml:"publicationMetadata"`
		Transcript      string   `yaml:"publicationTranscript"`
		PreservedValues []string `yaml:"preservedValues"`
	}
	if err := yaml.Unmarshal(retainedExtractionYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	artifact, err := NewManagedArtifact([]byte(fixture.Metadata), []byte(fixture.Transcript))
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "managed")
	filesystem := &OSFileSystem{}
	versions := maps.Clone(HarvesterVersionRegistry)
	versions[HarnessClaudeCode] = HarvesterVersions{AdapterVersion: 2, IndexerVersion: versions[HarnessClaudeCode].IndexerVersion, IndexVersion: versions[HarnessClaudeCode].IndexVersion}
	pipeline, err := NewPipeline(filesystem, nil, DefaultAdapterRegistry, PipelineConfig{OutputDir: ResolvedPath(output)}, WithHarvesterVersions(versions))
	if err != nil {
		t.Fatal(err)
	}
	session := DiscoveredSession{SessionID: artifact.Metadata.SessionID, Harness: artifact.Metadata.ModelHarness, SourceFormat: artifact.Metadata.Source.Format}
	// Seed the saved pair by writing its files, the state a completed harvest
	// leaves. The adapter refresh below reads it, extracts fresh metadata and
	// re-installs the pair by rename.
	sessionDir := SessionDir(output, string(artifact.Metadata.HostSlug), string(session.SessionID), "")
	if err := filesystem.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.WriteFile(filepath.Join(sessionDir, string(session.SessionID)+"--transcript."+string(artifact.Metadata.Source.Format)), artifact.Transcript, 0o600); err != nil {
		t.Fatal(err)
	}
	path := SessionMetadataPath(output, string(artifact.Metadata.HostSlug), string(session.SessionID), "")
	if err := filesystem.WriteFile(path, artifact.MetadataJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	result := pipeline.processRetainedSession(t.Context(), session, path)
	if result.result.Error != nil {
		t.Fatal(result.result.Error)
	}
	current, err := readArtifactPair(filesystem, output, path, session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata.AdapterVersion == nil || *current.Metadata.AdapterVersion != 2 || current.Metadata.Stats.TurnCount != 1 {
		t.Fatalf("metadata extraction output was not committed: %+v", current.Metadata)
	}
	if current.Metadata.Timestamp.Ingested == nil || *current.Metadata.Timestamp.Ingested != *artifact.Metadata.Timestamp.Ingested || current.Metadata.Source != artifact.Metadata.Source || current.Metadata.DerivedAt != nil || !bytes.Equal(current.Transcript, artifact.Transcript) {
		t.Fatal("metadata extraction changed native evidence or claimed database completion")
	}
	for _, marker := range fixture.PreservedValues {
		if !bytes.Contains(current.MetadataJSON, []byte(marker)) {
			t.Fatalf("unknown retained field %s was lost", marker)
		}
	}
	var before, after map[string]json.RawMessage
	if json.Unmarshal(artifact.MetadataJSON, &before) != nil || json.Unmarshal(current.MetadataJSON, &after) != nil {
		t.Fatal("invalid committed metadata")
	}
	if !bytes.Equal(before["extension"], after["extension"]) {
		t.Fatal("unknown metadata object changed")
	}
}
