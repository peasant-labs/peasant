package store_test

import (
	"context"
	_ "embed"
	"sort"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
)

//go:embed testdata/session_source_reader.yaml
var sessionSourceReaderYAML []byte

//go:embed testdata/session_source_reader.manifest.yaml
var sessionSourceReaderManifestYAML []byte

type sourceSessionSpec struct {
	Name                 string `yaml:"name"`
	ID                   string `yaml:"id"`
	Harness              string `yaml:"harness"`
	Project              string `yaml:"project"`
	SourcePath           string `yaml:"sourcePath"`
	Parent               string `yaml:"parent"`
	CacheParent          bool   `yaml:"cacheParent"`
	RelationshipEvidence bool   `yaml:"relationshipEvidence"`
	PublicationParent    string `yaml:"publicationParent"`
}

type sourceWant struct {
	Session string `yaml:"session"`
	Harness string `yaml:"harness"`
	Path    string `yaml:"path"`
	Parent  string `yaml:"parent"`
}

type sourceReaderCase struct {
	Name                string              `yaml:"name"`
	Kind                string              `yaml:"kind"`
	Seed                []string            `yaml:"seed"`
	IDs                 []string            `yaml:"ids"`
	Parents             []string            `yaml:"parents"`
	WantSources         []sourceWant        `yaml:"wantSources"`
	WantChildren        map[string][]string `yaml:"wantChildren"`
	WantChildCacheEmpty []string            `yaml:"wantChildCacheEmpty"`
}

type sourceReaderFixture struct {
	Sessions []sourceSessionSpec `yaml:"sessions"`
	Cases    []sourceReaderCase  `yaml:"cases"`
}

func loadSourceReaderFixture(t *testing.T) sourceReaderFixture {
	t.Helper()
	var fixture sourceReaderFixture
	if err := testutil.DecodeFixtureYAML(sessionSourceReaderYAML, &fixture); err != nil {
		t.Fatalf("decode session source reader fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(sessionSourceReaderManifestYAML, "session source reader")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		names = append(names, testCase.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "session source reader"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestStoredSessionReaderReadsBothChildHalves drives the store capability the
// scoped LOCATE reads through. A cached parent is found through the parent
// index, an uncached child stored before its parent is found through each
// durable evidence index, and an unrelated stored child is absent.
func TestStoredSessionReaderReadsBothChildHalves(t *testing.T) {
	t.Parallel()
	fixture := loadSourceReaderFixture(t)
	specs := make(map[string]sourceSessionSpec, len(fixture.Sessions))
	for _, spec := range fixture.Sessions {
		specs[spec.Name] = spec
	}
	for _, testCase := range fixture.Cases {
		t.Run(testCase.Name, func(t *testing.T) {
			t.Parallel()
			db := storetest.Open(t)
			seedSourceSessions(t, db, specs, testCase.Seed)
			switch testCase.Kind {
			case "sources":
				assertStoredSources(t, db, specs, testCase)
			case "children":
				assertStoredChildren(t, db, specs, testCase)
			default:
				t.Fatalf("fixture case %q has unknown kind %q", testCase.Name, testCase.Kind)
			}
		})
	}
}

func assertStoredSources(t *testing.T, db *store.Store, specs map[string]sourceSessionSpec, testCase sourceReaderCase) {
	t.Helper()
	ids := make([]ingest.SessionID, 0, len(testCase.IDs))
	for _, name := range testCase.IDs {
		ids = append(ids, ingest.SessionID(specs[name].ID))
	}
	got, err := db.StoredSessionSources(context.Background(), ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(testCase.WantSources) {
		t.Fatalf("StoredSessionSources returned %d rows, want %d", len(got), len(testCase.WantSources))
	}
	for _, want := range testCase.WantSources {
		spec := specs[want.Session]
		source, ok := got[ingest.SessionID(spec.ID)]
		if !ok {
			t.Fatalf("StoredSessionSources omitted %s", want.Session)
		}
		if string(source.Harness) != want.Harness {
			t.Fatalf("%s harness = %q, want %q", want.Session, source.Harness, want.Harness)
		}
		if source.Path.String() != want.Path {
			t.Fatalf("%s path = %q, want %q", want.Session, source.Path, want.Path)
		}
		if want.Parent == "" {
			if source.Parent != nil {
				t.Fatalf("%s parent = %q, want nil", want.Session, *source.Parent)
			}
		} else if source.Parent == nil || string(*source.Parent) != specs[want.Parent].ID {
			t.Fatalf("%s parent = %v, want %s", want.Session, source.Parent, specs[want.Parent].ID)
		}
	}
}

func assertStoredChildren(t *testing.T, db *store.Store, specs map[string]sourceSessionSpec, testCase sourceReaderCase) {
	t.Helper()
	parents := make([]ingest.SessionID, 0, len(testCase.Parents))
	for _, name := range testCase.Parents {
		parents = append(parents, ingest.SessionID(specs[name].ID))
	}
	got, err := db.StoredChildren(context.Background(), parents)
	if err != nil {
		t.Fatal(err)
	}
	for parentName, wantChildren := range testCase.WantChildren {
		parentID := ingest.SessionID(specs[parentName].ID)
		children := got[parentID]
		names := make([]string, 0, len(children))
		for _, child := range children {
			names = append(names, child.ID.String())
		}
		wantIDs := make([]string, 0, len(wantChildren))
		for _, name := range wantChildren {
			wantIDs = append(wantIDs, specs[name].ID)
		}
		sort.Strings(names)
		sort.Strings(wantIDs)
		if len(names) != len(wantIDs) {
			t.Fatalf("StoredChildren(%s) = %v, want %v", parentName, names, wantIDs)
		}
		for index := range wantIDs {
			if names[index] != wantIDs[index] {
				t.Fatalf("StoredChildren(%s) = %v, want %v", parentName, names, wantIDs)
			}
		}
	}
	for _, name := range testCase.WantChildCacheEmpty {
		if cached := storedParentCache(t, db, ingest.SessionID(specs[name].ID)); cached != "" {
			t.Fatalf("%s has a cached parent %q; the case must exercise the uncached durable half", name, cached)
		}
	}
}

// seedSourceSessions inserts the named fixture sessions in order. Each row is
// inserted through the production write path; the durable evidence a case needs
// is planted afterward on the same store, because it represents a child stored
// before its parent was available.
func seedSourceSessions(t *testing.T, db *store.Store, specs map[string]sourceSessionSpec, names []string) {
	t.Helper()
	ctx := context.Background()
	for _, name := range names {
		spec, ok := specs[name]
		if !ok {
			t.Fatalf("fixture seed names unknown session %q", name)
		}
		harness := ingest.Harness(spec.Harness)
		if !harness.IsKnown() {
			t.Fatalf("fixture session %s names unknown harness %q", name, spec.Harness)
		}
		entry := scopeSessionEntry(t, spec.ID, spec.Project, harness)
		entry.Metadata.Source.FilePath = spec.SourcePath
		entry.Session.SourcePath = ingest.ResolvedPath(spec.SourcePath)
		if spec.CacheParent {
			parentSpec, ok := specs[spec.Parent]
			if !ok {
				t.Fatalf("fixture session %s names unknown cached parent %q", name, spec.Parent)
			}
			parentID := ingest.SessionID(parentSpec.ID)
			entry.SchedulingParentResolved = true
			entry.SchedulingParentID = &parentID
		}
		if err := db.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
			t.Fatalf("seed session %s: %v", name, err)
		}
		if spec.RelationshipEvidence {
			plantRelationshipEvidence(t, db, spec.ID, specs[spec.Parent].ID)
		}
		if spec.PublicationParent != "" {
			plantPublicationParent(t, db, spec.ID, specs[spec.PublicationParent].ID)
		}
	}
}

func plantRelationshipEvidence(t *testing.T, db *store.Store, child, parent string) {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	const generation = "source-reader-generation"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence
(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES (?, ?, 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{
		Args: []any{child, generation, parent},
	}); err != nil {
		t.Fatalf("seed relationship evidence for %s: %v", child, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET latest_attempt_generation_id = ?, parent_id = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{generation, child},
	}); err != nil {
		t.Fatalf("seed latest attempt for %s: %v", child, err)
	}
}

func plantPublicationParent(t *testing.T, db *store.Store, child, parent string) {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	hash := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_publication_metadata
(session_id, capture_revision, schema_version, metadata_json, metadata_hash, content_hash, captured_at)
VALUES (?, 1, ?, json_object('parentUuid', ?), ?, ?, ?)`, &sqlitex.ExecOptions{
		Args: []any{child, ingest.CurrentSchemaVersion, parent, hash, hash, 1700000000000},
	}); err != nil {
		t.Fatalf("seed publication metadata for %s: %v", child, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET parent_id = NULL WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{child},
	}); err != nil {
		t.Fatalf("clear parent cache for %s: %v", child, err)
	}
}

func storedParentCache(t *testing.T, db *store.Store, id ingest.SessionID) string {
	t.Helper()
	conn, err := db.PoolForTest().Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	cached := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(parent_id, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			cached = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return cached
}
