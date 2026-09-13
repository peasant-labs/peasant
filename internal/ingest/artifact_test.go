package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/artifact_capture.yaml
var artifactCaptureYAML []byte

func TestManagedArtifactCapturePreservesValidatedInput(t *testing.T) {
	t.Parallel()
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Metadata      string   `yaml:"metadata"`
		Transcript    string   `yaml:"transcript"`
		Cases         []struct {
			Name          string `yaml:"name"`
			Patch         string `yaml:"patch"`
			Changed       bool   `yaml:"changed"`
			ErrorContains string `yaml:"errorContains"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(artifactCaptureYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatal("artifact capture fixture must have exactly one YAML document")
	}
	required := []string{"valid-legacy-hashes-absent", "derived-cache-time-not-input", "ingested-time-not-input", "redaction-time-not-input", "nested-key-order-is-not-input", "adapter-version-is-input", "future-adapter-remains-readable", "future-schema-refused", "invalid-content-hash-refused", "unknown-field-preserved-in-input", "invalid-adapter-refused"}
	if !reflect.DeepEqual(required, fixture.RequiredNames) {
		t.Fatal("artifact capture required-name manifest changed")
	}
	base, err := ingest.NewManagedArtifact([]byte(fixture.Metadata), []byte(fixture.Transcript))
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("invalid capture fixture %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			var fields map[string]json.RawMessage
			if err := json.Unmarshal([]byte(fixture.Metadata), &fields); err != nil {
				t.Fatal(err)
			}
			if row.Patch != "" {
				var patch map[string]json.RawMessage
				if err := json.Unmarshal([]byte(row.Patch), &patch); err != nil {
					t.Fatal(err)
				}
				for key, value := range patch {
					fields[key] = value
				}
			}
			data, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			transcript := []byte(fixture.Transcript)
			artifact, err := ingest.NewManagedArtifact(data, transcript)
			if row.ErrorContains != "" {
				if err == nil || !strings.Contains(err.Error(), row.ErrorContains) {
					t.Fatalf("capture error=%v want=%q", err, row.ErrorContains)
				}
				return
			}
			if err != nil || artifact.Validate() != nil {
				t.Fatalf("capture/validate: %v", err)
			}
			if (artifact.ArtifactHash != base.ArtifactHash) != row.Changed {
				t.Fatalf("semantic hash changed=%t want=%t", artifact.ArtifactHash != base.ArtifactHash, row.Changed)
			}
			if !bytes.Equal(artifact.MetadataJSON, data) || !bytes.Equal(artifact.Transcript, transcript) {
				t.Fatal("capture rewrote source bytes")
			}
			data[0], transcript[0] = ' ', ' '
			if err := artifact.Validate(); err != nil {
				t.Fatalf("caller mutation changed captured bytes: %v", err)
			}
			artifact.Metadata.Stats.TokensIn++
			if err := artifact.Validate(); err == nil {
				t.Fatal("mutated captured metadata was accepted for persistence")
			}
		})
	}
	for _, name := range required {
		if !seen[name] {
			t.Fatalf("missing capture fixture %q", name)
		}
	}
}
