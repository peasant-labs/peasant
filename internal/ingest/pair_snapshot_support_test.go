package ingest_test

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"io"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pair_snapshot.yaml
var pairSnapshotExtYAML []byte

type pairSnapshotExtFixture struct {
	RequiredNames      []string `yaml:"requiredNames"`
	SessionID          string   `yaml:"sessionID"`
	HostSlug           string   `yaml:"hostSlug"`
	FirstTranscript    string   `yaml:"firstTranscript"`
	SecondTranscript   string   `yaml:"secondTranscript"`
	ExtraMetadataField string   `yaml:"extraMetadataField"`
	ExtraMetadataValue string   `yaml:"extraMetadataValue"`
}

type pairSnapshotExtCaptureRead struct {
	Name                  string `yaml:"name"`
	TranscriptPresent     bool   `yaml:"transcriptPresent"`
	MetadataPresent       bool   `yaml:"metadataPresent"`
	ExpectMetadataReads   int    `yaml:"expectMetadataReads"`
	ExpectTranscriptReads int    `yaml:"expectTranscriptReads"`
}

func loadPairSnapshotExtFixture(t *testing.T) pairSnapshotExtFixture {
	t.Helper()
	var fixture struct {
		pairSnapshotExtFixture `yaml:",inline"`
		CaptureReads           []pairSnapshotExtCaptureRead `yaml:"captureReads"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(pairSnapshotExtYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the pair snapshot fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the pair snapshot fixture must hold exactly one YAML document")
	}
	names := make(map[string]bool, len(fixture.CaptureReads))
	for _, row := range fixture.CaptureReads {
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("pair snapshot", "case", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	inner := fixture.pairSnapshotExtFixture
	if inner.SessionID == "" || inner.HostSlug == "" || inner.FirstTranscript == "" || inner.SecondTranscript == "" || inner.ExtraMetadataField == "" {
		t.Fatal("the pair snapshot fixture must name a session, a host, two transcript generations, and the unprojected field")
	}
	return inner
}

// buildPairSnapshotMetadata encodes one metadata generation for transcript,
// with the full project identity a store write requires and the fixture's
// extra field riding outside the decoded struct.
func buildPairSnapshotMetadata(t *testing.T, fixture pairSnapshotExtFixture, transcript string) []byte {
	t.Helper()
	sid, err := ingest.NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	meta := ingest.NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = ingest.HarnessClaudeCode
	meta.Source.Format = ingest.SourceFormatJSONL
	meta.Source.FilePath = "/sources/" + fixture.SessionID + ".jsonl"
	meta.SchemaVersion = ingest.CurrentSchemaVersion
	ingested := time.Now().Add(-2 * time.Hour).UnixMilli()
	meta.Timestamp = ingest.TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	remote := testutil.TestGitRemote
	worktree := "/home/test/testrepo"
	meta.Git = ingest.GitContext{Remote: &remote, Worktree: &worktree}
	projectHash, hostSlug, err := ingest.DeriveProjectIdentifiers(salt.Salt{}, remote, worktree)
	if err != nil {
		t.Fatal(err)
	}
	meta.Project = ingest.ProjectInfo{Hash: projectHash, FilePath: "/home/test/testrepo", Name: "testrepo"}
	if string(hostSlug) != fixture.HostSlug {
		t.Fatalf("derived host %q disagrees with the fixture host %q; the pair would fail its owned-locator check", hostSlug, fixture.HostSlug)
	}
	meta.HostSlug = hostSlug
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(fixture.ExtraMetadataValue)
	if err != nil {
		t.Fatal(err)
	}
	fields[fixture.ExtraMetadataField] = value
	withExtra, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ingest.NewManagedArtifact(withExtra, []byte(transcript)); err != nil {
		t.Fatalf("the seeded snapshot generation does not validate, so no case can prove a refusal: %v", err)
	}
	return withExtra
}

// snapshotTranscriptPath names the transcript half beside metadataPath.
func snapshotTranscriptPath(metadataPath, sessionID string) string {
	return filepath.Join(filepath.Dir(metadataPath), sessionID+"--transcript."+string(ingest.SourceFormatJSONL))
}

// swapPairFS serves the first generation until it is armed and the first
// transcript read completes, then installs the second generation: a writer
// landing between the fallback capture and the index capture. The first
// transcript read of a fallback run is the fallback's own pair read, so
// arming it after selection keeps earlier selection reads on the first
// generation.
type swapPairFS struct {
	*testutil.MemFS
	mu             sync.Mutex
	transcriptPath string
	metadataPath   string
	nextMetadata   []byte
	nextTranscript []byte
	armed          bool
	hookFired      bool
}

var _ ingest.FileSystem = (*swapPairFS)(nil)

func (filesystem *swapPairFS) arm() {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	filesystem.armed = true
	filesystem.hookFired = false
}

func (filesystem *swapPairFS) ReadFile(path string) ([]byte, error) {
	data, err := filesystem.MemFS.ReadFile(path)
	if err == nil && path == filesystem.transcriptPath {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if filesystem.armed && !filesystem.hookFired {
			if err := filesystem.MemFS.WriteFile(filesystem.metadataPath, filesystem.nextMetadata, 0o600); err != nil {
				return nil, err
			}
			if err := filesystem.MemFS.WriteFile(filesystem.transcriptPath, filesystem.nextTranscript, 0o600); err != nil {
				return nil, err
			}
			filesystem.hookFired = true
		}
		return data, nil
	}
	return data, err
}

func (filesystem *swapPairFS) fired() bool {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	return filesystem.hookFired
}

// snapshotSeedResult wraps one preview entry the way a stored index row holds it.
func snapshotSeedResult(sid ingest.SessionID, preview string) indexformat.V1 {
	return indexformat.V1{Entries: []schema.SessionEntry{{
		SessionID:      sid,
		Harness:        ingest.HarnessClaudeCode,
		EntryIndex:     0,
		EntryType:      schema.EntryTypeText,
		Role:           schema.RoleUser,
		ContentPreview: &preview,
	}}}
}
