package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/pair_snapshot.yaml
var pairSnapshotYAML []byte

// pairSnapshotCaptureRead is one row of the read matrix: which halves the
// index capture holds in memory, and how many disk reads of each half one
// capture may then perform. Every row that lacks a half must read both
// halves as one validated pair; a row holding both halves reads nothing.
type pairSnapshotCaptureRead struct {
	Name                  string `yaml:"name"`
	TranscriptPresent     bool   `yaml:"transcriptPresent"`
	MetadataPresent       bool   `yaml:"metadataPresent"`
	ExpectMetadataReads   int    `yaml:"expectMetadataReads"`
	ExpectTranscriptReads int    `yaml:"expectTranscriptReads"`
}

type pairSnapshotFixture struct {
	RequiredNames      []string                  `yaml:"requiredNames"`
	SessionID          string                    `yaml:"sessionID"`
	HostSlug           string                    `yaml:"hostSlug"`
	FirstTranscript    string                    `yaml:"firstTranscript"`
	SecondTranscript   string                    `yaml:"secondTranscript"`
	ExtraMetadataField string                    `yaml:"extraMetadataField"`
	ExtraMetadataValue string                    `yaml:"extraMetadataValue"`
	CaptureReads       []pairSnapshotCaptureRead `yaml:"captureReads"`
}

func loadPairSnapshotFixture(t *testing.T) pairSnapshotFixture {
	t.Helper()
	var fixture pairSnapshotFixture
	decoder := yaml.NewDecoder(bytes.NewReader(pairSnapshotYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the pair snapshot fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the pair snapshot fixture must hold exactly one YAML document")
	}
	present := make(map[string]bool, len(fixture.CaptureReads))
	for _, row := range fixture.CaptureReads {
		if row.Name == "" || present[row.Name] {
			t.Fatalf("the pair snapshot fixture has an empty or repeated capture-read name %q", row.Name)
		}
		present[row.Name] = true
		if row.ExpectMetadataReads < 0 || row.ExpectTranscriptReads < 0 {
			t.Fatalf("fixture case %q expects a negative read count", row.Name)
		}
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("the pair snapshot fixture declares no required names; list every capture-read name it must retain")
	}
	for _, required := range fixture.RequiredNames {
		if !present[required] {
			t.Fatalf("required fixture case %q is missing; the read rule it pins would stop being tested", required)
		}
	}
	if fixture.SessionID == "" || fixture.HostSlug == "" || fixture.FirstTranscript == "" || fixture.SecondTranscript == "" {
		t.Fatal("the pair snapshot fixture must name a session, a host, and two transcript generations")
	}
	if fixture.FirstTranscript == fixture.SecondTranscript {
		t.Fatal("the pair snapshot fixture generations must differ, or a mixed snapshot is indistinguishable from a clean one")
	}
	if fixture.ExtraMetadataField == "" {
		t.Fatal("the pair snapshot fixture must name a metadata field the decoded projection drops")
	}
	return fixture
}

// buildSnapshotMetadata encodes one metadata generation for transcript: the
// content hash names transcript exactly as every production write path does,
// and the fixture's extra field rides along outside the decoded struct, the
// way supported producers' unprojected fields do.
func buildSnapshotMetadata(t *testing.T, fixture pairSnapshotFixture, transcript string) []byte {
	t.Helper()
	sid, err := NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatalf("NewSessionID(%q): %v", fixture.SessionID, err)
	}
	host, err := NewHostSlug(fixture.HostSlug)
	if err != nil {
		t.Fatalf("NewHostSlug(%q): %v", fixture.HostSlug, err)
	}
	meta := NewUnifiedMetadata()
	meta.SessionID = sid
	meta.ModelHarness = HarnessClaudeCode
	meta.HostSlug = host
	meta.Source.Format = SourceFormatJSONL
	meta.Source.FilePath = "/native/" + fixture.SessionID + ".jsonl"
	meta.SchemaVersion = CurrentSchemaVersion
	ingested := int64(1708300860000)
	meta.Timestamp = TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		t.Fatalf("marshal snapshot metadata: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("reopen snapshot metadata fields: %v", err)
	}
	value, err := json.Marshal(fixture.ExtraMetadataValue)
	if err != nil {
		t.Fatalf("marshal snapshot extra field: %v", err)
	}
	fields[fixture.ExtraMetadataField] = value
	withExtra, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("encode snapshot metadata with its extra field: %v", err)
	}
	if _, err := NewManagedArtifact(withExtra, []byte(transcript)); err != nil {
		t.Fatalf("the seeded snapshot generation does not validate, so no case can prove a refusal: %v", err)
	}
	return withExtra
}

// seedSnapshotPair writes one validated generation at its owned locator.
func seedSnapshotPair(t *testing.T, filesystem FileSystem, output, metadataPath string, sid SessionID, metadataJSON []byte, transcript string) string {
	t.Helper()
	if err := filesystem.MkdirAll(filepath.Dir(metadataPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := filesystem.WriteFile(metadataPath, metadataJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	transcriptPath := retainedTranscriptPath(metadataPath, sid, SourceFormatJSONL)
	if err := filesystem.WriteFile(transcriptPath, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	return transcriptPath
}

// countingReadFS records disk reads of one pair's two halves, so a case can
// tell a lone metadata read (the mixed snapshot) from one validated read of
// both halves.
type countingReadFS struct {
	FileSystem
	metadataPath    string
	transcriptPath  string
	metadataReads   int
	transcriptReads int
}

func (filesystem *countingReadFS) ReadFile(path string) ([]byte, error) {
	switch path {
	case filesystem.metadataPath:
		filesystem.metadataReads++
	case filesystem.transcriptPath:
		filesystem.transcriptReads++
	}
	return filesystem.FileSystem.ReadFile(path)
}

// swapAfterTranscriptReadFS serves the first generation until the first
// transcript read completes, then installs the second generation. The first
// transcript read of a fallback run is the fallback's own pair read, so the
// swap lands after the fallback capture and before the index capture, the
// way a concurrent writer landing between the two reads does.
type swapAfterTranscriptReadFS struct {
	FileSystem
	mu             sync.Mutex
	transcriptPath string
	metadataPath   string
	nextMetadata   []byte
	nextTranscript []byte
	hookFired      bool
}

func (filesystem *swapAfterTranscriptReadFS) ReadFile(path string) ([]byte, error) {
	data, err := filesystem.FileSystem.ReadFile(path)
	if err == nil && path == filesystem.transcriptPath {
		filesystem.mu.Lock()
		defer filesystem.mu.Unlock()
		if !filesystem.hookFired {
			if err := filesystem.FileSystem.WriteFile(filesystem.metadataPath, filesystem.nextMetadata, 0o600); err != nil {
				return nil, err
			}
			if err := filesystem.FileSystem.WriteFile(filesystem.transcriptPath, filesystem.nextTranscript, 0o600); err != nil {
				return nil, err
			}
			filesystem.hookFired = true
		}
		return data, nil
	}
	return data, err
}

func (filesystem *swapAfterTranscriptReadFS) fired() bool {
	filesystem.mu.Lock()
	defer filesystem.mu.Unlock()
	return filesystem.hookFired
}

type snapshotGitResolver struct{}

func (snapshotGitResolver) RemoteURL(_ context.Context, _ string) (string, error) { return "", nil }
func (snapshotGitResolver) Branch(_ context.Context, _ string) (string, error)    { return "", nil }
func (snapshotGitResolver) Worktree(_ context.Context, _ string) (string, error)  { return "", nil }
func (snapshotGitResolver) TrackingBranch(_ context.Context, _ string) (string, error) {
	return "", nil
}
func (snapshotGitResolver) UserEmail(_ context.Context) (string, error) { return "", nil }
func (snapshotGitResolver) WalkUpRemoteURL(_ context.Context, _ string) (string, string, error) {
	return "", "", nil
}

// snapshotFailingAdapter registers the harness without ever reaching
// extraction: the native source file is absent, so the capture fails before
// the adapter is consulted and the retained fallback engages.
type snapshotFailingAdapter struct{}

func (snapshotFailingAdapter) Harness() Harness { return HarnessClaudeCode }
func (snapshotFailingAdapter) Discover(_ context.Context, _ SourceConfig) ([]DiscoveredSession, error) {
	return nil, nil
}
func (snapshotFailingAdapter) ExtractMetadata(_ context.Context, session DiscoveredSession) (*UnifiedMetadata, error) {
	return nil, context.DeadlineExceeded
}

func snapshotPipeline(filesystem FileSystem, output ResolvedPath) *Pipeline {
	return &Pipeline{
		fs:  filesystem,
		git: snapshotGitResolver{},
		adapters: map[Harness]AdapterFactory{
			HarnessClaudeCode: func(FileSystem, GitResolver, salt.Salt) SourceAdapter { return snapshotFailingAdapter{} },
		},
		config: PipelineConfig{OutputDir: output},
	}
}

// TestWorkerMetadataJSONPrefersRetainedBytes pins the carry precedence: exact
// bytes the run already validated win over the committed artifact, which in
// turn wins over nothing.
func TestWorkerMetadataJSONPrefersRetainedBytes(t *testing.T) {
	retained := []byte(`{"snapshot":"retained"}`)
	artifact := buildPrecedenceArtifact(t)
	got := workerMetadataJSON(&workerResult{retainedMetadataJSON: retained, artifact: artifact})
	if !bytes.Equal(got, retained) {
		t.Fatalf("workerMetadataJSON ignored %d carried bytes for committed bytes", len(retained))
	}

	onlyCommitted := workerMetadataJSON(&workerResult{artifact: artifact})
	if !bytes.Equal(onlyCommitted, artifact.MetadataJSON) {
		t.Fatal("workerMetadataJSON without carried bytes must serve the committed pair")
	}

	if workerMetadataJSON(&workerResult{}) != nil {
		t.Fatal("workerMetadataJSON with neither carried nor committed bytes must serve nothing")
	}
}

// buildPrecedenceArtifact owns a committed pair for the precedence test.
func buildPrecedenceArtifact(t *testing.T) *ManagedArtifact {
	t.Helper()
	transcript := "{}\n"
	meta := NewUnifiedMetadata()
	sid, err := NewSessionID("99d59925-36bc-424c-a789-8be54d9702ba")
	if err != nil {
		t.Fatal(err)
	}
	host, err := NewHostSlug("github.com--testuser--testrepo")
	if err != nil {
		t.Fatal(err)
	}
	meta.SessionID = sid
	meta.ModelHarness = HarnessClaudeCode
	meta.HostSlug = host
	meta.Source.Format = SourceFormatJSONL
	meta.SchemaVersion = CurrentSchemaVersion
	ingested := int64(1708300860000)
	meta.Timestamp = TimestampInfo{Start: 1708300800000, End: 1708300860000, Ingested: &ingested}
	meta.ContentHash = schema.ComputeTranscriptHash([]byte(transcript))
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	encoded, err := json.Marshal(&meta)
	if err != nil {
		t.Fatal(err)
	}
	artifact, err := NewManagedArtifact(encoded, []byte(transcript))
	if err != nil {
		t.Fatalf("seed the committed artifact: %v", err)
	}
	return artifact
}

// TestCaptureArtifactReadsBothHalvesOrNeither walks the read matrix: any
// capture missing a half performs one validated read of both halves, and a
// capture holding both halves performs no disk read at all.
func TestCaptureArtifactReadsBothHalvesOrNeither(t *testing.T) {
	fixture := loadPairSnapshotFixture(t)
	for _, row := range fixture.CaptureReads {
		t.Run(row.Name, func(t *testing.T) {
			output, err := NewResolvedPath(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			sid, err := NewSessionID(fixture.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			metadataPath := SessionMetadataPath(string(output), fixture.HostSlug, fixture.SessionID, "")
			base := &OSFileSystem{}
			metadataJSON := buildSnapshotMetadata(t, fixture, fixture.FirstTranscript)
			transcriptPath := seedSnapshotPair(t, base, string(output), metadataPath, sid, metadataJSON, fixture.FirstTranscript)
			filesystem := &countingReadFS{FileSystem: base, metadataPath: metadataPath, transcriptPath: transcriptPath}
			pipeline := snapshotPipeline(filesystem, output)
			im := indexedMeta{
				session:              DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode},
				outputTranscriptPath: transcriptPath,
			}
			if row.TranscriptPresent {
				im.transcriptData = []byte(fixture.FirstTranscript)
			}
			if row.MetadataPresent {
				im.metadataData = metadataJSON
			}
			artifact, err := pipeline.captureArtifactForIndex(metadataPath, im)
			if err != nil {
				t.Fatalf("capture with transcript=%v metadata=%v refused a consistent pair: %v", row.TranscriptPresent, row.MetadataPresent, err)
			}
			if string(artifact.Transcript) != fixture.FirstTranscript {
				t.Fatal("the captured pair does not hold the generation the caller validated")
			}
			if !bytes.Contains(artifact.MetadataJSON, []byte(fixture.ExtraMetadataValue)) {
				t.Fatal("the captured pair lost the metadata field the decoded projection drops")
			}
			if filesystem.metadataReads != row.ExpectMetadataReads || filesystem.transcriptReads != row.ExpectTranscriptReads {
				t.Fatalf("case %q read metadata %d times and transcript %d times, want %d and %d; a lone metadata read pairs bytes from two different reads",
					row.Name, filesystem.metadataReads, filesystem.transcriptReads, row.ExpectMetadataReads, row.ExpectTranscriptReads)
			}
		})
	}
}

// TestFallbackCarrySurvivesConcurrentMetadataWrite reproduces the mixed
// snapshot: the native-failure fallback validates one generation, a writer
// installs the next generation before the index capture, and the index must
// still parse the generation the fallback validated.
func TestFallbackCarrySurvivesConcurrentMetadataWrite(t *testing.T) {
	fixture := loadPairSnapshotFixture(t)
	output, err := NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := SessionMetadataPath(string(output), fixture.HostSlug, fixture.SessionID, "")
	base := &OSFileSystem{}
	firstJSON := buildSnapshotMetadata(t, fixture, fixture.FirstTranscript)
	secondJSON := buildSnapshotMetadata(t, fixture, fixture.SecondTranscript)
	transcriptPath := seedSnapshotPair(t, base, string(output), metadataPath, sid, firstJSON, fixture.FirstTranscript)
	filesystem := &swapAfterTranscriptReadFS{
		FileSystem:     base,
		transcriptPath: transcriptPath,
		metadataPath:   metadataPath,
		nextMetadata:   secondJSON,
		nextTranscript: []byte(fixture.SecondTranscript),
	}
	pipeline := snapshotPipeline(filesystem, output)
	session := DiscoveredSession{
		SessionID:    sid,
		Harness:      HarnessClaudeCode,
		SourcePath:   ResolvedPath(filepath.Join(string(output), "native-missing.jsonl")),
		SourceFormat: SourceFormatJSONL,
	}
	result := pipeline.processSession(t.Context(), DiffEntry{Session: session, Status: DiffUpdated})
	if result.result.Error != nil {
		t.Fatalf("the fallback run failed instead of carrying its retained pair: %v", result.result.Error)
	}
	if !filesystem.fired() {
		t.Fatal("the concurrent writer never landed between the fallback capture and the index capture, so the case proves nothing")
	}
	// The on-disk generation must actually have moved on: otherwise the
	// green capture below would pass without proving anything about mixing.
	if diskMeta, err := base.ReadFile(metadataPath); err != nil || !bytes.Equal(diskMeta, secondJSON) {
		t.Fatalf("the metadata file does not hold the second generation after the swap: %v", err)
	}
	if diskTranscript, err := base.ReadFile(transcriptPath); err != nil || string(diskTranscript) != fixture.SecondTranscript {
		t.Fatalf("the transcript file does not hold the second generation after the swap: %v", err)
	}
	if result.artifact != nil {
		t.Fatal("the fallback claimed a commit it never made; the drain would mirror a phantom pair and report a false publication")
	}
	if string(result.transcriptData) != fixture.FirstTranscript {
		t.Fatal("the fallback kept a transcript it never validated")
	}
	im := indexedMeta{
		session:              session,
		outputTranscriptPath: transcriptPath,
		transcriptData:       result.transcriptData,
		metadataData:         workerMetadataJSON(&result),
	}
	artifact, err := pipeline.captureArtifactForIndex(metadataPath, im)
	if err != nil {
		t.Fatalf("the index capture refused a validated snapshot with %v; it paired the carried transcript with a separately-read metadata", err)
	}
	if string(artifact.Transcript) != fixture.FirstTranscript {
		t.Fatal("the indexed pair does not represent the generation the fallback validated")
	}
	if !bytes.Contains(artifact.MetadataJSON, []byte(fixture.ExtraMetadataValue)) {
		t.Fatal("the indexed pair lost the metadata field the decoded projection drops")
	}
	if got := schema.ComputeTranscriptHash(artifact.Transcript); artifact.Metadata.ContentHash != got {
		t.Fatal("the indexed pair is not internally consistent")
	}
}

// snapshotFileIndexer is the smallest indexer the reindex fallback consults:
// only its source kind matters, because the fallback refuses directory
// captures before any parse.
type snapshotFileIndexer struct{}

func (snapshotFileIndexer) SourceKind() TranscriptSourceKind { return TranscriptSourceFile }
func (snapshotFileIndexer) IndexTranscript(_ context.Context, _ DiscoveredSession) ([]schema.SessionEntry, error) {
	return nil, nil
}
func (snapshotFileIndexer) IndexTranscriptBytes(_ context.Context, _ DiscoveredSession, _ []byte) ([]schema.SessionEntry, error) {
	return nil, nil
}

// snapshotPublicationStore answers the publication projection the way the
// store does: with the decoded metadata, whose unprojected fields are already
// gone. The fallback must never use these bytes as the pair's metadata.
type snapshotPublicationStore struct {
	SessionStore
	metadata UnifiedMetadata
	revision int64
}

func (store snapshotPublicationStore) LoadPublicationMetadata(_ context.Context, ids []SessionID) (map[SessionID]PublicationMetadata, error) {
	out := make(map[SessionID]PublicationMetadata, len(ids))
	for _, id := range ids {
		out[id] = PublicationMetadata{Metadata: store.metadata, Readiness: PublicationReady, CaptureRevision: store.revision}
	}
	return out, nil
}

// TestPrepareReindexFallbackKeepsUnprojectedFields pins the rejected seam:
// the reindex fallback carries the raw on-disk metadata bytes, never a
// re-encoding of the decoded projection whose unprojected fields are
// already gone, so the index capture parses one complete snapshot.
func TestPrepareReindexFallbackKeepsUnprojectedFields(t *testing.T) {
	fixture := loadPairSnapshotFixture(t)
	output, err := NewResolvedPath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sid, err := NewSessionID(fixture.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	metadataPath := SessionMetadataPath(string(output), fixture.HostSlug, fixture.SessionID, "")
	base := &OSFileSystem{}
	metadataJSON := buildSnapshotMetadata(t, fixture, fixture.FirstTranscript)
	transcriptPath := seedSnapshotPair(t, base, string(output), metadataPath, sid, metadataJSON, fixture.FirstTranscript)
	decoded, err := decodeManagedMetadata(metadataJSON, metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := snapshotPipeline(base, output)
	pipeline.store = snapshotPublicationStore{metadata: *decoded, revision: 7}
	pipeline.indexers = map[Harness]TranscriptIndexer{HarnessClaudeCode: snapshotFileIndexer{}}
	session := DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourceFormat: SourceFormatJSONL}
	im := pipeline.prepareReindexFallback(t.Context(), reindexTarget{session: session, transcriptPath: transcriptPath})
	if len(im.transcriptData) == 0 {
		t.Fatal("the reindex fallback lost the transcript the projection proof verified")
	}
	// The carried bytes must be the document bytes, not a re-encoding of
	// the decoded projection: only the document still holds the fields the
	// projection drops.
	diskMetadata, err := base.ReadFile(metadataPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(im.metadataData, diskMetadata) {
		t.Fatal("the reindex fallback synthesized metadata bytes instead of carrying the validated document")
	}
	if !bytes.Contains(im.metadataData, []byte(fixture.ExtraMetadataValue)) {
		t.Fatal("the reindex fallback lost the metadata field the decoded projection drops")
	}
	if im.captureRevision != 7 {
		t.Fatalf("the reindex fallback lost the publication proof revision, got %d", im.captureRevision)
	}
	artifact, err := pipeline.captureArtifactForIndex(metadataPath, im)
	if err != nil {
		t.Fatalf("the reindex capture refused a consistent pair: %v", err)
	}
	if !bytes.Contains(artifact.MetadataJSON, []byte(fixture.ExtraMetadataValue)) {
		t.Fatal("the reindexed pair lost the metadata field the decoded projection drops; the capture did not read both halves from disk")
	}
	if !strings.Contains(metadataPath, string(sid)+defaults.MetadataSuffix) {
		t.Fatal("the test addressed a path outside the owned metadata layout")
	}
}
