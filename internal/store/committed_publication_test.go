package store_test

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

//go:embed testdata/committed_publication.yaml
var committedPublicationYAML []byte

type committedPublicationCase struct {
	Name                  string `yaml:"name"`
	Managed               bool   `yaml:"managed"`
	CaptureCount          *int64 `yaml:"captureCount"`
	CaptureGraph          bool   `yaml:"captureGraph"`
	GenerationCount       *int64 `yaml:"generationCount"`
	GenerationGraph       bool   `yaml:"generationGraph"`
	WithoutContentSupport bool   `yaml:"withoutContentSupport"`
	Corrupt               bool   `yaml:"corrupt"`
	WantError             string `yaml:"wantError"`
}

type committedPublicationFixture struct {
	RequiredNames []string                   `yaml:"requiredNames"`
	Cases         []committedPublicationCase `yaml:"cases"`
	Concurrency   struct {
		FirstModel  string `yaml:"firstModel"`
		SecondModel string `yaml:"secondModel"`
		FirstText   string `yaml:"firstText"`
		SecondText  string `yaml:"secondText"`
		Count       int64  `yaml:"count"`
	} `yaml:"concurrency"`
}

func loadCommittedPublicationFixtures(t *testing.T) committedPublicationFixture {
	t.Helper()
	var f committedPublicationFixture
	if err := yaml.Unmarshal(committedPublicationYAML, &f); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range f.Cases {
		if c.Name == "" || seen[c.Name] {
			t.Fatalf("blank or duplicate fixture %q", c.Name)
		}
		seen[c.Name] = true
	}
	if err := testutil.RequireFixtureNames("committed publication", "case", f.RequiredNames, seen); err != nil {
		t.Fatal(err)
	}
	return f
}

func openCommittedPublicationStore(t *testing.T, path, root string, supported bool) (*store.Store, store.SessionLocker) {
	t.Helper()
	options := []store.OpenOption{store.WithPoolSize(1), store.WithIndexFormats(store.V2IndexFormat())}
	locker, err := store.NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	if supported {
		artifacts, err := store.NewOSGenerationArtifactStore(root)
		if err != nil {
			t.Fatal(err)
		}
		options = append(options, store.WithGenerationArtifacts(artifacts, locker))
	}
	db, err := store.Open(path, options...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, locker
}

func committedPublicationGraph(meta *schema.UnifiedMetadata, present bool) {
	meta.RootSessionID, meta.Purpose, meta.Relationships = nil, "", nil
	if present {
		id := meta.SessionID
		meta.RootSessionID = &id
		meta.Purpose = schema.SessionPurposeInteraction
		meta.Relationships = []schema.SessionRelationship{{Kind: schema.SessionRelationshipContextFrom, TargetState: schema.RelationshipTargetUnknown, Evidence: schema.EvidenceNativeTyped}}
	}
}

func seedCommittedPublication(t *testing.T, db *store.Store, c committedPublicationCase, model, text string) *schema.UnifiedMetadata {
	t.Helper()
	entry := publicationEntry(t, testutil.TestSessionUUID)
	meta := entry.Metadata
	meta.Model = schema.ModelID(model)
	meta.Stats.InputSubmissionCount = c.CaptureCount
	committedPublicationGraph(meta, c.CaptureGraph)
	entries := batchTestEntries(meta.SessionID, text, 1)
	if !c.Managed {
		testutil.SeedReadyPublication(t, db, meta, entries)
		return meta
	}
	projection := *meta
	projection.Stats.TurnCount = len(entries)
	projection.Stats.InputSubmissionCount = c.GenerationCount
	committedPublicationGraph(&projection, c.GenerationGraph)
	storetest.SeedGenerationPublication(t, db, meta, indexformat.V2{Generation: indexformat.Generation{
		ID: "g-committed", Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: projection, Main: indexformat.Partition{Entries: entries},
		SourceEvidenceDigest: strings.Repeat("a", 64),
	}}, nil)
	return meta
}

func TestCommittedPublicationInput(t *testing.T) {
	f := loadCommittedPublicationFixtures(t)
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			dir := t.TempDir()
			path, root := filepath.Join(dir, "publication.db"), filepath.Join(dir, "artifacts")
			db, _ := openCommittedPublicationStore(t, path, root, true)
			meta := seedCommittedPublication(t, db, c, f.Concurrency.FirstModel, f.Concurrency.FirstText)
			before, err := db.LoadPublicationInput(t.Context(), meta.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			if c.Corrupt {
				publicationSQL(t, db, `UPDATE session_projection_generations SET metadata_json='{}' WHERE session_id=?`, string(meta.SessionID))
			}
			if c.WithoutContentSupport {
				db, _ = openCommittedPublicationStore(t, path, root, false)
			}
			called := false
			err = db.WithCommittedPublicationInput(t.Context(), meta.SessionID, func(input ingest.PublicationInputBundle) error {
				called = true
				if input.Readiness != ingest.PublicationReady {
					return fmt.Errorf("unexpected readiness %s", input.Readiness)
				}
				if !c.Managed {
					if input.Generation != nil || !reflect.DeepEqual(input, before) {
						return fmt.Errorf("legacy capture changed")
					}
					return nil
				}
				if input.Generation == nil || input.Generation.GenerationID != "g-committed" {
					return fmt.Errorf("committed generation missing")
				}
				want := input.Generation.Metadata
				if !reflect.DeepEqual(input.Metadata.Stats.InputSubmissionCount, c.GenerationCount) ||
					!reflect.DeepEqual(input.Metadata.RootSessionID, want.RootSessionID) || input.Metadata.Purpose != want.Purpose ||
					!reflect.DeepEqual(input.Metadata.Relationships, want.Relationships) {
					return fmt.Errorf("publication metadata is not the committed projection")
				}
				if input.Metadata.Project != before.Metadata.Project || input.Metadata.Source != before.Metadata.Source ||
					input.CaptureRevision != before.CaptureRevision || input.Metadata.MetadataHash != before.Metadata.MetadataHash {
					return fmt.Errorf("capture identity or evidence changed")
				}
				return nil
			})
			if c.WantError != "" {
				if err == nil || !strings.Contains(err.Error(), c.WantError) || called {
					t.Fatalf("expected refusal before callback, got called=%t err=%v", called, err)
				}
				return
			}
			if err != nil || !called {
				t.Fatalf("committed read called=%t err=%v", called, err)
			}
			after, err := db.LoadPublicationInput(t.Context(), meta.SessionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("committed read modified stored capture: %v", err)
			}
		})
	}
}

// A captured metadata read starts the transaction. Commit a different capture
// through a second real connection when the generation SELECT is prepared.
// The new method must not combine that capture with its older entries/generation.
func TestCommittedPublicationInputPinsOneDatabaseSnapshot(t *testing.T) {
	f := loadCommittedPublicationFixtures(t).Concurrency
	dir := t.TempDir()
	path, root := filepath.Join(dir, "publication.db"), filepath.Join(dir, "artifacts")
	reader, _ := openCommittedPublicationStore(t, path, root, true)
	writer, _ := openCommittedPublicationStore(t, path, root, true)
	meta := seedCommittedPublication(t, reader, committedPublicationCase{Managed: true, GenerationCount: &f.Count}, f.FirstModel, f.FirstText)
	replacement := *meta
	replacement.Model = schema.ModelID(f.SecondModel)
	replacement.MetadataHash = schema.ComputeMetadataHash(&replacement)
	triggered := false
	observeFullReads(t, reader, func(a sqlite.Action) {
		if triggered || a.Type() != sqlite.OpRead || a.Table() != "session_projection_generations" {
			return
		}
		triggered = true
		if _, err := writer.InsertSessionsWithRevisions(t.Context(), []ingest.StoreEntry{{Metadata: &replacement, PublicationCapture: true, CWDProvenance: ingest.CWDSourceAbsent}}); err != nil {
			t.Error(err)
		}
	})
	err := reader.WithCommittedPublicationInput(t.Context(), meta.SessionID, func(input ingest.PublicationInputBundle) error {
		if !triggered || input.Generation == nil || input.Metadata.Model != schema.ModelID(f.FirstModel) ||
			input.Generation.Metadata.Model != schema.ModelID(f.FirstModel) || input.Readiness != ingest.PublicationReady {
			return fmt.Errorf("mixed publication snapshot")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	latest, err := writer.LoadPublicationInput(t.Context(), meta.SessionID)
	if err != nil || latest.Metadata.Model != schema.ModelID(f.SecondModel) || latest.Readiness == ingest.PublicationReady {
		t.Fatalf("replacement capture was not committed and held for indexing: %+v %v", latest, err)
	}
}

func TestCommittedPublicationInputReleasesConnectionAndRetainsLock(t *testing.T) {
	f := loadCommittedPublicationFixtures(t).Concurrency
	dir := t.TempDir()
	db, locker := openCommittedPublicationStore(t, filepath.Join(dir, "publication.db"), filepath.Join(dir, "artifacts"), true)
	meta := seedCommittedPublication(t, db, committedPublicationCase{Managed: true, GenerationCount: &f.Count}, f.FirstModel, f.FirstText)
	sentinel := errors.New("callback failure")
	err := db.WithCommittedPublicationInput(t.Context(), meta.SessionID, func(input ingest.PublicationInputBundle) error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		// Pool size one: this only works if the reader released its connection.
		if _, err := db.LoadPublicationInput(ctx, meta.SessionID); err != nil {
			return err
		}
		lockCtx, lockCancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer lockCancel()
		unlock, err := locker.LockExclusive(lockCtx, meta.SessionID)
		if err == nil {
			_ = unlock()
			return fmt.Errorf("generation was not protected during callback")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error not returned: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	unlock, err := locker.LockExclusive(ctx, meta.SessionID)
	if err != nil {
		t.Fatalf("callback retained generation lock after return: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}
