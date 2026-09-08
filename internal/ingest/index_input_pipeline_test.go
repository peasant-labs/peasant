package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_input_pipeline.yaml
var indexInputPipelineYAML []byte

type indexInputChange string

const (
	indexInputNoChange       indexInputChange = ""
	indexInputArtifactChange indexInputChange = "artifact"
	indexInputSQLChange      indexInputChange = "sql"
)

type indexInputPipelineFixture struct {
	Transcript       string              `yaml:"transcript"`
	Replacement      string              `yaml:"replacement"`
	PreviousPreview  string              `yaml:"previousPreview"`
	ExpectedPreview  string              `yaml:"expectedPreview"`
	Malformed        string              `yaml:"malformed"`
	EligibilityModes []indexInputRunMode `yaml:"eligibilityModes"`
	RequiredNames    []string            `yaml:"requiredNames"`
	Cases            []struct {
		Name   string           `yaml:"name"`
		Empty  bool             `yaml:"empty"`
		Change indexInputChange `yaml:"change"`
	} `yaml:"cases"`
}

type indexInputRunMode string

const (
	indexInputNormalRun indexInputRunMode = "normal"
	indexInputIndexRun  indexInputRunMode = "index"
)

func loadIndexInputPipelineFixture(t *testing.T) indexInputPipelineFixture {
	t.Helper()
	var fixture indexInputPipelineFixture
	decoder := yaml.NewDecoder(bytes.NewReader(indexInputPipelineYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatal("index input pipeline fixtures require one document")
	}
	names := make(map[string]bool)
	for _, row := range fixture.Cases {
		if row.Name == "" || names[row.Name] || (row.Change != indexInputNoChange && row.Change != indexInputArtifactChange && row.Change != indexInputSQLChange) {
			t.Fatalf("invalid pipeline input fixture %+v", row)
		}
		names[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("index input pipeline", "case", fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	modes := make(map[indexInputRunMode]bool)
	for _, mode := range fixture.EligibilityModes {
		if modes[mode] || (mode != indexInputNormalRun && mode != indexInputIndexRun) {
			t.Fatalf("invalid eligibility mode %q", mode)
		}
		modes[mode] = true
	}
	if !modes[indexInputNormalRun] || !modes[indexInputIndexRun] {
		t.Fatal("normal and index-only eligibility cases are required")
	}
	return fixture
}

type capturedInputIndexer struct {
	ingest.TranscriptIndexer
	afterParse func(context.Context) error
	fileParses int
	byteParses int
}

var _ ingest.VersionedTranscriptIndexer = (*capturedInputIndexer)(nil)

func (indexer *capturedInputIndexer) IndexTranscriptResult(ctx context.Context, session ingest.DiscoveredSession) (indexformat.Result, error) {
	indexer.fileParses++
	result, err := indexer.TranscriptIndexer.(ingest.VersionedTranscriptIndexer).IndexTranscriptResult(ctx, session)
	if err == nil && indexer.afterParse != nil {
		err = indexer.afterParse(ctx)
	}
	return result, err
}

func (indexer *capturedInputIndexer) IndexTranscriptBytesResult(ctx context.Context, session ingest.DiscoveredSession, data []byte) (indexformat.Result, error) {
	indexer.byteParses++
	result, err := indexer.TranscriptIndexer.(ingest.VersionedTranscriptIndexer).IndexTranscriptBytesResult(ctx, session, data)
	if err == nil && indexer.afterParse != nil {
		err = indexer.afterParse(ctx)
	}
	return result, err
}

func publishIndexInputFixture(ctx context.Context, publisher *ingest.ArtifactPublisher, artifact *ingest.ManagedArtifact, metadataPath string) error {
	session := ingest.DiscoveredSession{SessionID: artifact.Metadata.SessionID, Harness: artifact.Metadata.ModelHarness}
	observation, err := publisher.Observe(ctx, session, metadataPath)
	if err != nil {
		return err
	}
	committed, err := publisher.Publish(ctx, ingest.ArtifactPublication{Artifact: artifact, Observation: observation})
	if err != nil {
		return err
	}
	_, err = publisher.Reconcile(ctx, committed)
	return err
}

func TestPipelineCommitsOnlyItsCapturedIndexInput(t *testing.T) {
	fixture := loadIndexInputPipelineFixture(t)
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			output := t.TempDir()
			filesystem := &ingest.OSFileSystem{}
			database, err := store.Open(filepath.Join(t.TempDir(), "index.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, ingest.ArtifactPublisherOptions{Mirror: database})
			if err != nil {
				t.Fatal(err)
			}
			transcript := fixture.Transcript
			if row.Empty {
				transcript = ""
			}
			artifact := publicationTestArtifact(t, transcript)
			sid := artifact.Metadata.SessionID
			session := ingest.DiscoveredSession{SessionID: sid, Harness: artifact.Metadata.ModelHarness}
			metadataPath := ingest.SessionMetadataPath(output, string(artifact.Metadata.HostSlug), string(sid), "")
			publish := func(candidate *ingest.ManagedArtifact) error {
				return publishIndexInputFixture(ctx, publisher, candidate, metadataPath)
			}
			if err := publish(artifact); err != nil {
				t.Fatal(err)
			}
			oldEntries := []schema.SessionEntry{{SessionID: sid, Harness: session.Harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &fixture.PreviousPreview}}
			seed := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: oldEntries}, IndexVersion: 1, IndexerVersion: 14, IndexedAtMs: 100}})[0]
			if seed.Err != nil {
				t.Fatal(seed.Err)
			}
			beforeEntries, err := database.ListEntries(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			indexer := &capturedInputIndexer{TranscriptIndexer: ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[session.Harness]}
			var actorError error
			var afterActor *ingest.SessionIndexState
			if row.Change != indexInputNoChange {
				candidate := publicationTestArtifact(t, fixture.Replacement)
				indexer.afterParse = func(context.Context) error {
					if row.Change == indexInputArtifactChange {
						actorError = publish(candidate)
					} else {
						actorError = database.UpdateIndexState(ctx, sid, ingest.HarvesterVersionRegistry[session.Harness].IndexerVersion, 200)
					}
					if actorError == nil {
						afterActor, actorError = database.ReadIndexState(ctx, sid)
					}
					return actorError
				}
			}
			config := makePipelineConfig(output)
			config.Reindex, config.Force = true, true
			pipeline, err := ingest.NewPipeline(filesystem, testutil.NoGitResolver(), ingest.DefaultAdapterRegistry, config, ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{session.Harness: indexer}))
			if err != nil {
				t.Fatal(err)
			}
			result, err := pipeline.Run(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if actorError != nil {
				t.Fatalf("competing actor could not finish outside parser ownership: %v", actorError)
			}
			state, err := database.ReadIndexState(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			if row.Change != indexInputNoChange {
				entries, err := database.ListEntries(ctx, sid)
				if err != nil || result.Summary.Indexed != 0 || afterActor == nil || !reflect.DeepEqual(afterActor, state) || !reflect.DeepEqual(beforeEntries, entries) {
					t.Fatalf("stale parser replaced current state: summary=%+v state=%+v actor=%+v error=%v", result.Summary, state, afterActor, err)
				}
			} else if result.Summary.Indexed != 1 || state.IndexedInputHash == nil || state.ArtifactHash == nil || *state.IndexedInputHash == *state.ArtifactHash {
				t.Fatalf("successful index lacks its actual distinct input proof: summary=%+v state=%+v", result.Summary, state)
			}
			if row.Change == indexInputNoChange {
				entries, err := database.ListEntries(ctx, sid)
				if err != nil || row.Empty && len(entries) != 0 || !row.Empty && (len(entries) == 0 || entries[0].ContentPreview == nil || *entries[0].ContentPreview != fixture.ExpectedPreview) {
					t.Fatalf("stored entries do not represent the captured transcript: %+v %v", entries, err)
				}
			}
			if indexer.fileParses != 0 || indexer.byteParses != 1 {
				t.Fatalf("parser reopened uncaptured source: files=%d bytes=%d", indexer.fileParses, indexer.byteParses)
			}
		})
	}
}

func TestPipelineRetriesAndSkipsByActualIndexInput(t *testing.T) {
	fixture := loadIndexInputPipelineFixture(t)
	for _, mode := range fixture.EligibilityModes {
		t.Run(string(mode), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			output := t.TempDir()
			filesystem := &ingest.OSFileSystem{}
			database, err := store.Open(filepath.Join(t.TempDir(), "index.db"), store.WithPoolSize(1))
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			publisher, err := ingest.NewArtifactPublisher(filesystem, output, ingest.ArtifactPublisherOptions{Mirror: database})
			if err != nil {
				t.Fatal(err)
			}
			artifact := publicationTestArtifact(t, fixture.Transcript)
			sid, harness := artifact.Metadata.SessionID, artifact.Metadata.ModelHarness
			metadataPath := ingest.SessionMetadataPath(output, string(artifact.Metadata.HostSlug), string(sid), "")
			if err := publishIndexInputFixture(ctx, publisher, artifact, metadataPath); err != nil {
				t.Fatal(err)
			}
			oldEntries := []schema.SessionEntry{{SessionID: sid, Harness: harness, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &fixture.PreviousPreview}}
			seed := database.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{SessionID: sid, Result: indexformat.V1{Entries: oldEntries}, IndexVersion: 1, IndexerVersion: ingest.HarvesterVersionRegistry[harness].IndexerVersion, IndexedAtMs: 100}})[0]
			if seed.Err != nil {
				t.Fatal(seed.Err)
			}
			indexer := &capturedInputIndexer{TranscriptIndexer: ingest.NewIndexerRegistry(filesystem, ingest.IndexerRegistryOptions{})[harness]}
			config := makePipelineConfig(output)
			config.Reindex = mode == indexInputIndexRun
			pipeline, err := ingest.NewPipeline(filesystem, testutil.NoGitResolver(), map[ingest.Harness]ingest.AdapterFactory{harness: makeStubAdapter(nil, nil)}, config, ingest.WithStore(database), ingest.WithMetricsStore(database), ingest.WithIndexers(map[ingest.Harness]ingest.TranscriptIndexer{harness: indexer}))
			if err != nil {
				t.Fatal(err)
			}
			first, err := pipeline.Run(ctx)
			if err != nil || first.Summary.Indexed != 1 || indexer.byteParses != 1 {
				t.Fatalf("current producer with unknown input was not verified: %+v parses=%d err=%v", first, indexer.byteParses, err)
			}
			state, err := database.ReadIndexState(ctx, sid)
			if err != nil || state.IndexedInputHash == nil {
				t.Fatalf("successful verification did not persist input: %+v %v", state, err)
			}
			second, err := pipeline.Run(ctx)
			after, readErr := database.ReadIndexState(ctx, sid)
			if err != nil || readErr != nil || second.Summary.Indexed != 0 || indexer.byteParses != 1 || !reflect.DeepEqual(state, after) {
				t.Fatalf("unchanged proven input reran: %+v parses=%d err=%v", second, indexer.byteParses, err)
			}
			if err := publishIndexInputFixture(ctx, publisher, publicationTestArtifact(t, fixture.Malformed), metadataPath); err != nil {
				t.Fatal(err)
			}
			lastGood, err := database.ReadIndexState(ctx, sid)
			if err != nil {
				t.Fatal(err)
			}
			failed, err := pipeline.Run(ctx)
			after, readErr = database.ReadIndexState(ctx, sid)
			if err != nil || readErr != nil || failed.Summary.Indexed != 0 || indexer.byteParses != 2 || !reflect.DeepEqual(lastGood, after) {
				t.Fatalf("changed input failure changed last-good proof or retried within this invocation: %+v parses=%d err=%v", failed, indexer.byteParses, err)
			}
			retry, err := pipeline.Run(ctx)
			after, readErr = database.ReadIndexState(ctx, sid)
			if err != nil || readErr != nil || retry.Summary.Indexed != 0 || indexer.byteParses != 3 || !reflect.DeepEqual(lastGood, after) {
				t.Fatalf("next invocation did not retry changed input: %+v parses=%d err=%v", retry, indexer.byteParses, err)
			}
			if err := publishIndexInputFixture(ctx, publisher, publicationTestArtifact(t, fixture.Replacement), metadataPath); err != nil {
				t.Fatal(err)
			}
			repaired, err := pipeline.Run(ctx)
			after, readErr = database.ReadIndexState(ctx, sid)
			if err != nil || readErr != nil || repaired.Summary.Indexed != 1 || indexer.byteParses != 4 || after.IndexedInputHash == nil || *after.IndexedInputHash == *lastGood.IndexedInputHash {
				t.Fatalf("repaired input did not complete current-version indexing: %+v parses=%d state=%+v err=%v", repaired, indexer.byteParses, after, err)
			}
		})
	}
}
