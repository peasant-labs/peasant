package ingest_test

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/index_parse_metadata.yaml
var indexParseMetadataYAML []byte

type indexParseMetadataDocument struct {
	Transcript string `yaml:"transcript"`
	Cases      []struct {
		Name   string `yaml:"name"`
		Change string `yaml:"change"`
	} `yaml:"cases"`
	RequiredNames []string `yaml:"requiredNames"`
}

// TestIndexParseIgnoresMetadataCapture pins the assumption the rule "bind
// without a re-parse" rests on: the format-1 entry parse of a captured pair
// reads nothing from its metadata capture beyond the session identity the
// input digest covers. A metadata-only refresh therefore leaves the stored
// input proof valid and the stored entries exactly what a parse would write.
// If a parse ever reads one of these fields, its case fails here, and the rule
// must stop binding such a session without a parse.
func TestIndexParseIgnoresMetadataCapture(t *testing.T) {
	t.Parallel()
	var document indexParseMetadataDocument
	if err := testutil.DecodeFixtureYAML(indexParseMetadataYAML, &document); err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(document.Cases))
	for i, c := range document.Cases {
		names[i] = c.Name
	}
	if err := testutil.ValidateRequiredNames(testutil.RequiredNamesManifest{RequiredNames: document.RequiredNames}, names, "index parse metadata"); err != nil {
		t.Fatal(err)
	}
	transcript := []byte(document.Transcript)
	parse := func(t *testing.T, meta *ingest.UnifiedMetadata) (digest, entries string) {
		t.Helper()
		meta.ContentHash = schema.ComputeTranscriptHash(transcript)
		encoded, err := json.Marshal(meta)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := ingest.NewManagedArtifact(encoded, transcript)
		if err != nil {
			t.Fatal(err)
		}
		// The production harvest's indexer registry and the session the
		// pipeline hands it: identity from the artifact, the saved transcript
		// as its source.
		indexer := ingest.NewIndexerRegistry(testutil.NewMemFS(), ingest.IndexerRegistryOptions{})[ingest.HarnessClaudeCode]
		session := ingest.DiscoveredSession{SessionID: meta.SessionID, Harness: meta.ModelHarness, SourcePath: "/saved/transcript.jsonl"}
		input, err := ingest.CaptureIndexInput(t.Context(), indexer, session, artifact)
		if err != nil {
			t.Fatal(err)
		}
		result, err := input.Parse(t.Context(), indexer)
		if err != nil {
			t.Fatal(err)
		}
		out, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return input.Hash(), string(out)
	}
	baseDigest, baseEntries := parse(t, makeMinimalMeta(t, testSessionID))
	if baseEntries == "null" || baseEntries == "{}" {
		t.Fatalf("the baseline parse produced no entries: %s", baseEntries)
	}
	for _, c := range document.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			meta := makeMinimalMeta(t, testSessionID)
			changeIndexParseMetadata(t, meta, c.Change)
			digest, entries := parse(t, meta)
			if digest != baseDigest {
				t.Errorf("input digest changed with %s: the stored input proof would no longer match", c.Change)
			}
			if entries != baseEntries {
				t.Errorf("entries changed with %s:\n got: %s\nwant: %s", c.Change, entries, baseEntries)
			}
		})
	}
}

// changeIndexParseMetadata applies one named metadata edit from the closed set
// the fixture documents.
func changeIndexParseMetadata(t *testing.T, meta *ingest.UnifiedMetadata, change string) {
	t.Helper()
	switch change {
	case "cwd":
		meta.CWD = "/elsewhere/other-repo"
	case "git":
		branch, remote, worktree := "feature/other", "https://github.com/other/other.git", "/elsewhere/other-repo"
		meta.Git.Branch, meta.Git.Remote, meta.Git.Worktree = &branch, &remote, &worktree
	case "project":
		meta.Project.Hash = schema.ProjectHash("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		meta.Project.Name, meta.Project.FilePath = "other-repo", "/elsewhere/other-repo"
	case "parent":
		parent, err := ingest.NewSessionID("22222222-2222-4222-8222-222222222222")
		if err != nil {
			t.Fatal(err)
		}
		meta.ParentUUID = &parent
	case "sourcePath":
		meta.Source.FilePath = "/elsewhere/native/session.jsonl"
	case "adapterVersion":
		revision := 99
		meta.AdapterVersion = &revision
	case "redaction":
		meta.Redaction = schema.RedactionInfo{Applied: true, Level: "standard", RuleSetVersion: "9.9.9"}
	default:
		t.Fatalf("unknown metadata change %q", change)
	}
}
