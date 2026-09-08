package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_recovery_rejections.yaml
var artifactRecoveryRejectionsYAML []byte

func TestArtifactRecoveryRefusesBadEvidenceWithoutBlockingPeers(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames   []string `yaml:"requiredNames"`
		BadSession      string   `yaml:"badSession"`
		GoodSession     string   `yaml:"goodSession"`
		Transcript      string   `yaml:"transcript"`
		NewerTranscript string   `yaml:"newerTranscript"`
		Cases           []struct {
			Name           string `yaml:"name"`
			Malformed      bool   `yaml:"malformed"`
			Escaping       bool   `yaml:"escaping"`
			Version        int    `yaml:"version"`
			Stale          bool   `yaml:"stale"`
			InventoryError bool   `yaml:"inventoryError"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactRecoveryRejectionsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("recovery rejection fixture requires one document")
	}
	required := []string{"malformed-intent", "escaping-path", "unsupported-intent", "stale-intent"}
	if !reflect.DeepEqual(fixture.RequiredNames, required) {
		t.Fatal("recovery rejection manifest changed")
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid recovery rejection case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			output := filepath.Join(t.TempDir(), "managed")
			fault := &artifactPublicationFault{row: artifactPublicationCase{Operation: "rename", Target: "--metadata.json", After: true}}
			filesystem := &artifactPublicationFS{fault: fault}
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, ingest.ArtifactPublisherOptions{})
			if err != nil {
				t.Fatal(err)
			}
			bad, badPath := seedPendingPublication(t, publisher, filesystem, fault, output, fixture.BadSession, fixture.Transcript)
			good, goodPath := seedPendingPublication(t, publisher, filesystem, fault, output, fixture.GoodSession, fixture.Transcript)
			intentPath := filepath.Join(output, ".peasant-state", "transactions", schema.ComputeTranscriptHash([]byte(fixture.BadSession)), "intent.json")
			intent, err := filesystem.ReadFile(intentPath)
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]json.RawMessage
			if err := json.Unmarshal(intent, &document); err != nil {
				t.Fatal(err)
			}
			if row.Malformed {
				intent = []byte("{incomplete")
			} else if row.Version != 0 {
				document["version"], _ = json.Marshal(row.Version)
				intent, err = json.Marshal(document)
			} else if row.Escaping {
				var files []map[string]json.RawMessage
				if err := json.Unmarshal(document["files"], &files); err != nil {
					t.Fatal(err)
				}
				files[0]["path"] = json.RawMessage(`"../unowned-file"`)
				document["files"], _ = json.Marshal(files)
				intent, err = json.Marshal(document)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := filesystem.WriteFile(intentPath, intent, 0600); err != nil {
				t.Fatal(err)
			}
			if row.Stale {
				bad = publicationArtifactForID(t, fixture.BadSession, fixture.NewerTranscript)
				if err := filesystem.WriteFile(badPath, bad.MetadataJSON, 0600); err != nil {
					t.Fatal(err)
				}
				if err := filesystem.WriteFile(filepath.Join(filepath.Dir(badPath), fixture.BadSession+"--transcript.jsonl"), bad.Transcript, 0600); err != nil {
					t.Fatal(err)
				}
			}
			recovery, err := ingest.NewArtifactPublisher(&ingest.OSFileSystem{}, output, ingest.ArtifactPublisherOptions{})
			if err != nil {
				t.Fatal(err)
			}
			ids, err := recovery.PendingSessions()
			if (err != nil) != row.InventoryError || !slices.Contains(ids, good.Metadata.SessionID) {
				t.Fatalf("bad intent discarded independent recovery work: ids=%v err=%v", ids, err)
			}
			if err := recovery.Recover(t.Context(), bad.Metadata.SessionID); err == nil {
				t.Fatal("bad/stale recovery evidence authorized mutation")
			}
			if err := recovery.Recover(t.Context(), good.Metadata.SessionID); err != nil {
				t.Fatalf("bad intent blocked compatible peer: %v", err)
			}
			if _, err := recovery.Capture(t.Context(), good.Metadata.SessionID, goodPath); err != nil {
				t.Fatal(err)
			}
			if data, err := filesystem.ReadFile(badPath); err != nil || !bytes.Equal(data, bad.MetadataJSON) {
				t.Fatal("refused recovery changed current/newer metadata")
			}
			if data, err := filesystem.ReadFile(filepath.Join(filepath.Dir(badPath), fixture.BadSession+"--transcript.jsonl")); err != nil || !bytes.Equal(data, bad.Transcript) {
				t.Fatal("refused recovery changed current/newer transcript")
			}
			if data, err := filesystem.ReadFile(intentPath); err != nil || !bytes.Equal(data, intent) {
				t.Fatal("refused recovery erased or modified uncertain intent evidence")
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing recovery rejection fixture %q", name)
		}
	}
}

func publicationArtifactForID(t *testing.T, id, transcript string) *ingest.ManagedArtifact {
	t.Helper()
	base := publicationTestArtifact(t, transcript)
	meta := base.Metadata
	sid, err := ingest.NewSessionID(id)
	if err != nil {
		t.Fatal(err)
	}
	meta.SessionID = sid
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	data, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := ingest.NewManagedArtifact(data, []byte(transcript))
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func seedPendingPublication(t *testing.T, publisher *ingest.ArtifactPublisher, filesystem *artifactPublicationFS, fault *artifactPublicationFault, output, id, transcript string) (*ingest.ManagedArtifact, string) {
	t.Helper()
	artifact := publicationArtifactForID(t, id, transcript)
	observation, err := publisher.Observe(t.Context(), ingest.DiscoveredSession{SessionID: artifact.Metadata.SessionID, Harness: artifact.Metadata.ModelHarness}, "")
	if err != nil {
		t.Fatal(err)
	}
	fault.armed.Store(true)
	if _, err := publisher.Publish(t.Context(), ingest.ArtifactPublication{Artifact: artifact, Observation: observation}); err == nil {
		t.Fatal("pending committed fixture did not stop after metadata commit")
	}
	path := ingest.SessionMetadataPath(output, string(artifact.Metadata.HostSlug), id, "")
	data, err := filesystem.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err = ingest.NewManagedArtifact(data, artifact.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	return artifact, path
}
