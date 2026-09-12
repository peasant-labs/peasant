package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pi_metadata_compatibility.yaml
var piMetadataCompatibilityYAML []byte

//go:embed testdata/pi_metadata_compatibility.manifest.yaml
var piMetadataCompatibilityManifest []byte

func TestPiSchemaPinDoesNotInvalidateUnchangedHarnesses(t *testing.T) {
	var f struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Version int    `yaml:"version"`
			Updated int    `yaml:"updated"`
			Status  string `yaml:"status"`
			Refused bool   `yaml:"refused"`
		} `yaml:"cases"`
	}
	d := yaml.NewDecoder(bytes.NewReader(piMetadataCompatibilityYAML))
	d.KnownFields(true)
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		t.Fatalf("trailing YAML: %v", err)
	}
	m, err := testutil.DecodeRequiredNamesManifest(piMetadataCompatibilityManifest, "metadata compatibility")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(f.Cases))
	for i, c := range f.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(m, names, "metadata compatibility"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			fs := testutil.NewMemFS()
			git := testutil.DefaultGitResolver()
			path := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
			setupSourceFile(t, fs, path)
			modified := time.Now().Add(-2 * time.Hour)
			fs.ModTimes[path] = modified
			session := makeDiscoveredSession(t, testSessionID, path, modified)
			meta := makeMinimalMeta(t, testSessionID)
			adapters := map[ingest.Harness]ingest.AdapterFactory{ingest.HarnessClaudeCode: makeStubAdapter([]ingest.DiscoveredSession{session}, map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta})}
			cfg := makePipelineConfig(testOutputDir)
			pipeline, err := ingest.NewPipeline(fs, git, adapters, cfg)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := pipeline.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			metadataPath := fmt.Sprintf("%s/%s--metadata.json", expectedOutputBase(testOutputDir, testSessionID), testSessionID)
			raw, err := fs.ReadFile(metadataPath)
			if err != nil {
				t.Fatal(err)
			}
			var written ingest.UnifiedMetadata
			if err := json.Unmarshal(raw, &written); err != nil {
				t.Fatal(err)
			}
			// Only a case that actually pins a different schema version rewrites
			// the sidecar. Rewriting it with the version it already carries
			// would replace the published file out of band and provoke one
			// refresh that has nothing to do with the version under test.
			if written.SchemaVersion != c.Version {
				written.SchemaVersion = c.Version
				raw, err = json.Marshal(written)
				if err != nil {
					t.Fatal(err)
				}
				if err := fs.WriteFile(metadataPath, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			pipeline, err = ingest.NewPipeline(fs, git, adapters, cfg)
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Updated != c.Updated || result.Summary.New != 0 {
				t.Fatalf("sidecar diff changed: %+v", result.Summary)
			}
			// A sidecar this build cannot read is refused out loud, and the
			// refusal names the version it found, so a user learns to upgrade
			// instead of finding a silently skipped session.
			refused := false
			for _, diagnostic := range result.Diagnostics {
				refused = refused || strings.Contains(diagnostic.Message, "is newer than supported version")
			}
			if refused != c.Refused {
				t.Fatalf("future-version refusal reported = %t, want %t: %+v", refused, c.Refused, result.Diagnostics)
			}
			// Upstream captured-source evidence deliberately refreshes legacy
			// sidecars once. The compatibility guarantee is that this never
			// becomes a repeated refresh merely because Schema added Pi.
			result, err = pipeline.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Summary.Updated != 0 || result.Summary.Unchanged != 1 {
				t.Fatalf("captured sidecar did not settle to unchanged: %+v", result.Summary)
			}
			ingested := time.Now().UnixMilli()
			status := ingest.ClassifyAgainstStore(session, ingest.SessionLocation{SchemaVersion: c.Version, IngestedMs: &ingested}, 0)
			if status.String() != c.Status {
				t.Fatalf("stored diff=%s want %s", status, c.Status)
			}
		})
	}
}
