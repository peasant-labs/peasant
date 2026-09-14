package store

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/generation_inherited_content.yaml
var generationInheritedContentYAML []byte

//go:embed testdata/generation_inherited_content.manifest.yaml
var generationInheritedContentManifestYAML []byte

type inheritedContentFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		ID            string `yaml:"id"`
		InheritedRef  string `yaml:"inherited_ref"`
		InheritedText string `yaml:"inherited_text"`
	} `yaml:"generation"`
	Cases []struct {
		Name      string `yaml:"name"`
		WithAlias bool   `yaml:"with_alias"`
	} `yaml:"cases"`
}

func loadInheritedContentFixture(t *testing.T) inheritedContentFixture {
	t.Helper()
	var fixture inheritedContentFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(generationInheritedContentYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode generation_inherited_content.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(generationInheritedContentManifestYAML)
	if err != nil {
		t.Fatalf("decode generation_inherited_content manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "generation inherited content"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// openGenerationStoreAt opens the real V2 store, owned-artifact file store and
// OS session lock under a caller-owned directory so a test can close and reopen
// the same durable state.
func openGenerationStoreAt(t *testing.T, dir string) *Store {
	t.Helper()
	root := filepath.Join(dir, "artifacts")
	artifacts, err := NewOSGenerationArtifactStore(root)
	if err != nil {
		t.Fatal(err)
	}
	locker, err := NewFileSessionLocker(root)
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(dir, "generations.db"), WithPoolSize(2), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// buildInheritedContentGeneration extends the standard valid candidate with an
// inherited-only context segment: the captured ref has content and, optionally,
// a native alias but no emitted main or earlier entry.
func buildInheritedContentGeneration(t *testing.T, sid schema.SessionID, fixture inheritedContentFixture, withAlias bool) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	v2, blobs := buildTestGeneration(t, sid, fixture.Generation.ID, "owned text", "owned input", "owned output")
	inherited := schema.SourceEntryRef(fixture.Generation.InheritedRef)
	v2.Generation.Segments = []indexformat.ContextSegment{{
		Ordinal:          0,
		PhysicalSourceID: "source-inherited",
		Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
		Inclusion:        indexformat.SegmentInclusionInherited,
		CapturedRefs:     []schema.SourceEntryRef{inherited},
	}}
	v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: inherited})
	blobs[inherited] = []byte(fixture.Generation.InheritedText)
	if withAlias {
		v2.Generation.Aliases = append(v2.Generation.Aliases, indexformat.NativeAlias{NativeKey: "native-inherited", Ref: inherited})
	}
	return v2, blobs
}

// TestGenerationInheritedContentSnapshot proves the reconcile boundary between
// a retained generation and a read snapshot: a generation with inherited-only
// content activates, survives reopening, and reads through WithSessionSnapshot.
// The snapshot display content map is filtered to emitted refs, while the
// inherited record, its blob and its alias stay in the generation catalog.
func TestGenerationInheritedContentSnapshot(t *testing.T) {
	fixture := loadInheritedContentFixture(t)
	sid, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	inheritedRef := schema.SourceEntryRef(fixture.Generation.InheritedRef)

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			dir := t.TempDir()
			s := openGenerationStoreAt(t, dir)
			defer func() { _ = s.Close() }()
			seedGenerationSession(t, s, fixture.Session.ID)

			candidate, blobs := buildInheritedContentGeneration(t, sid, fixture, tc.WithAlias)
			if err := activateTestGeneration(t, s, candidate, blobs); err != nil {
				t.Fatalf("activate a generation with retained inherited content: %v", err)
			}
			// The staged manifest is the self-contained durable projection:
			// the generation validator must accept retained inherited content,
			// and the installed manifest must keep that evidence.
			manifest, err := s.generationArtifacts.ReadManifest(context.Background(), sid, fixture.Generation.ID)
			if err != nil {
				t.Fatalf("read staged generation manifest: %v", err)
			}
			if err := manifest.Validate(); err != nil {
				t.Fatalf("staged generation with retained inherited content is invalid: %v", err)
			}
			manifestRefs := make(map[schema.SourceEntryRef]struct{}, len(manifest.Content))
			for _, record := range manifest.Content {
				manifestRefs[record.Ref] = struct{}{}
			}
			if _, ok := manifestRefs[inheritedRef]; !ok {
				t.Fatalf("staged manifest dropped retained inherited content ref %q", inheritedRef)
			}
			// Reopen the real store so the assertion runs against durable rows
			// and blobs, not the in-memory candidate.
			if err := s.Close(); err != nil {
				t.Fatalf("close store before reopen: %v", err)
			}
			s = openGenerationStoreAt(t, dir)

			ctx := context.Background()
			emitted := make(map[schema.SourceEntryRef]struct{}, len(generationRefs))
			for _, ref := range generationRefs {
				emitted[schema.SourceEntryRef(ref)] = struct{}{}
			}
			err = s.WithSessionSnapshot(ctx, sid, func(snapshot indexformat.ReadSnapshot) error {
				if snapshot.GenerationID != fixture.Generation.ID {
					return fmt.Errorf("snapshot generation = %q, want %q", snapshot.GenerationID, fixture.Generation.ID)
				}
				if err := snapshot.Validate(); err != nil {
					return fmt.Errorf("reopened snapshot is not a coherent read: %w", err)
				}
				seen := make(map[schema.SourceEntryRef]struct{}, len(snapshot.Content))
				for _, record := range snapshot.Content {
					seen[record.Ref] = struct{}{}
					if record.Ref == inheritedRef {
						return fmt.Errorf("display content map carries inherited-only ref %q; it is not owned by an emitted partition", inheritedRef)
					}
				}
				for ref := range emitted {
					if _, ok := seen[ref]; !ok {
						return fmt.Errorf("display content map is missing emitted ref %q", ref)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("read reopened generation with retained inherited content: %v", err)
			}

			// The inherited record stays in the generation catalog and its
			// immutable blob stays resolvable after the reopen, so retained
			// evidence is not erased by filtering the display map.
			record := catalogContentRecord(t, s, sid, fixture.Generation.ID, inheritedRef)
			content, err := s.ReadFullContent(ctx, sid, fixture.Generation.ID, record)
			if err != nil {
				t.Fatalf("resolve retained inherited blob: %v", err)
			}
			if string(content) != fixture.Generation.InheritedText {
				t.Fatalf("retained inherited blob = %q, want %q", string(content), fixture.Generation.InheritedText)
			}
			if tc.WithAlias {
				target := catalogAliasTarget(t, s, sid, fixture.Generation.ID, "native-inherited")
				if target != inheritedRef {
					t.Fatalf("retained alias target = %q, want %q", target, inheritedRef)
				}
			}
		})
	}
}

// catalogContentRecord reads one immutable content record from the stored
// generation catalog, independent of the snapshot display map.
func catalogContentRecord(t *testing.T, s *Store, sid schema.SessionID, generationID string, ref schema.SourceEntryRef) indexformat.ContentRecord {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	record := indexformat.ContentRecord{Ref: ref}
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT relative_blob, byte_length, integrity_digest FROM session_projection_content WHERE session_id = ? AND generation_id = ? AND source_entry_ref = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID, string(ref)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			record.RelativeBlob = stmt.ColumnText(0)
			record.ByteLength = stmt.ColumnInt64(1)
			record.Digest = stmt.ColumnText(2)
			return nil
		},
	}); err != nil {
		t.Fatalf("read generation catalog content for %q: %v", ref, err)
	}
	if !found {
		t.Fatalf("inherited content record %q is missing from the generation catalog; retained evidence was erased", ref)
	}
	return record
}

// catalogAliasTarget reads one retained native alias target from the stored
// generation catalog.
func catalogAliasTarget(t *testing.T, s *Store, sid schema.SessionID, generationID, nativeKey string) schema.SourceEntryRef {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	var target schema.SourceEntryRef
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT source_entry_ref FROM session_projection_aliases WHERE session_id = ? AND generation_id = ? AND native_key = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID, nativeKey},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			target = schema.SourceEntryRef(stmt.ColumnText(0))
			return nil
		},
	}); err != nil {
		t.Fatalf("read generation catalog alias %q: %v", nativeKey, err)
	}
	if !found {
		t.Fatalf("inherited alias %q is missing from the generation catalog; retained evidence was erased", nativeKey)
	}
	return target
}
