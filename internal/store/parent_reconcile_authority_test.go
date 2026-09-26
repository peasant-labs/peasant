package store

import (
	"bytes"
	"context"
	_ "embed"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/parent_reconcile_authority.yaml
var parentReconcileAuthorityYAML []byte

//go:embed testdata/parent_reconcile_authority.manifest.yaml
var parentReconcileAuthorityManifest []byte

type parentReconcileAuthorityCase struct {
	Name              string `yaml:"name"`
	LegacyParent      string `yaml:"legacyParent"`
	AuthorityState    string `yaml:"authorityState"`
	AuthorityTarget   string `yaml:"authorityTarget"`
	HarvestTarget     string `yaml:"harvestTarget"`
	ExpectParentCache string `yaml:"expectParentCache"`
	ExpectUpdates     int    `yaml:"expectUpdates"`
}

type parentReconcileAuthorityFixture struct {
	Child     string                         `yaml:"child"`
	Legacy    string                         `yaml:"legacy"`
	Alternate string                         `yaml:"alternate"`
	Cases     []parentReconcileAuthorityCase `yaml:"cases"`
}

type parentReconcileAuthorityManifestFile struct {
	RequiredNames []string `yaml:"requiredNames"`
}

// TestParentReconcileAuthority proves that the reverse cache reconciliation
// honors active managed-generation authority over the legacy publication
// snapshot. The child is stored with a legacy parent while the target is
// absent, a real managed generation is activated with one relationship record,
// and the harvest target is then stored. The real reverse lookup and the real
// transaction-scoped cache update run for that target, and the child cache must
// reflect only the active authority: a legacy snapshot may never resurrect a
// parent the active generation cleared, marked unknown or conflicting, or
// pointed at another target. The result must survive a SQLite reopen.
//
// Active relationship authority is established by managed-generation
// activation, which only a store opened with generation support can perform;
// the pipeline's independently admitted harnesses are V1 pairs. The
// pipeline-level legacy control therefore stays in the ingest orphan corpus,
// and the authority precedence is proven here against the real activation and
// the real reconciliation query it gates.
func TestParentReconcileAuthority(t *testing.T) {
	t.Parallel()
	fixture := loadParentReconcileAuthorityFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			runParentReconcileAuthorityCase(t, fixture, tc)
		})
	}
}

func loadParentReconcileAuthorityFixture(t *testing.T) parentReconcileAuthorityFixture {
	t.Helper()
	var fixture parentReconcileAuthorityFixture
	decoder := yaml.NewDecoder(bytes.NewReader(parentReconcileAuthorityYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode parent reconcile authority fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("parent reconcile authority fixture must carry exactly one document, got %v", err)
	}
	var manifest parentReconcileAuthorityManifestFile
	decoder = yaml.NewDecoder(bytes.NewReader(parentReconcileAuthorityManifest))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		t.Fatalf("decode parent reconcile authority manifest: %v", err)
	}
	if len(fixture.Cases) == 0 || len(manifest.RequiredNames) != len(fixture.Cases) {
		t.Fatalf("authority fixture/manifest size mismatch: %d cases, %d required names", len(fixture.Cases), len(manifest.RequiredNames))
	}
	seen := make(map[string]struct{}, len(fixture.Cases))
	for i, tc := range fixture.Cases {
		if tc.Name == "" {
			t.Fatal("authority fixture case with empty name")
		}
		if manifest.RequiredNames[i] != tc.Name {
			t.Fatalf("authority manifest order changed at %d: manifest=%q fixture=%q", i, manifest.RequiredNames[i], tc.Name)
		}
		if _, duplicate := seen[tc.Name]; duplicate {
			t.Fatalf("duplicate authority fixture case %q", tc.Name)
		}
		seen[tc.Name] = struct{}{}
		if tc.HarvestTarget == "" {
			t.Fatalf("authority case %q names no harvest target", tc.Name)
		}
	}
	return fixture
}

func runParentReconcileAuthorityCase(t *testing.T, fixture parentReconcileAuthorityFixture, tc parentReconcileAuthorityCase) {
	t.Helper()
	ctx := t.Context()
	db, dir := openAuthorityStore(t)
	child := authorityRequiredID(t, "child", fixture.Child)
	legacyParent := authorityOptionalID(t, fixture, tc.LegacyParent)
	harvest := authorityOptionalID(t, fixture, tc.HarvestTarget)
	if harvest == nil {
		t.Fatalf("authority case %q harvest target %q is unknown", tc.Name, tc.HarvestTarget)
	}

	// The child is admitted first, while the target is absent, so its FK cache
	// stays NULL and its legacy snapshot retains the logical parent.
	insertAuthoritySession(t, ctx, db, child, ingest.HarnessCodex, legacyParent)
	if tc.AuthorityState != "" {
		authorityTarget := authorityOptionalID(t, fixture, tc.AuthorityTarget)
		activateAuthorityRelationship(t, db, child, tc.AuthorityState, authorityTarget)
	}
	insertAuthoritySession(t, ctx, db, *harvest, ingest.HarnessCodex, nil)

	updates, err := db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{*harvest}, []ingest.Harness{ingest.HarnessCodex})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents: %v", err)
	}
	if len(updates) != tc.ExpectUpdates {
		t.Fatalf("authority case %q reverse lookup returned %d updates (%+v), want %d", tc.Name, len(updates), updates, tc.ExpectUpdates)
	}
	if err := db.ReconcileParentCache(ctx, updates); err != nil {
		t.Fatalf("ReconcileParentCache: %v", err)
	}
	want := authorityOptionalID(t, fixture, tc.ExpectParentCache)
	assertAuthorityParentCache(t, ctx, db, child, want)

	if err := db.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	reopened := reopenAuthorityStore(t, dir)
	t.Cleanup(func() { _ = reopened.Close() })
	assertAuthorityParentCache(t, ctx, reopened, child, want)
}

// authorityOptionalID resolves a fixture token to a session id, or nil for the
// empty token. A non-empty token that no id declares is a fixture error.
func authorityOptionalID(t *testing.T, fixture parentReconcileAuthorityFixture, token string) *ingest.SessionID {
	t.Helper()
	switch token {
	case "":
		return nil
	case "child":
		id := authorityRequiredID(t, "child", fixture.Child)
		return &id
	case "legacy":
		id := authorityRequiredID(t, "legacy", fixture.Legacy)
		return &id
	case "alternate":
		id := authorityRequiredID(t, "alternate", fixture.Alternate)
		return &id
	default:
		t.Fatalf("authority fixture names unknown id token %q", token)
		return nil
	}
}

func authorityRequiredID(t *testing.T, label, raw string) ingest.SessionID {
	t.Helper()
	if raw == "" {
		t.Fatalf("authority fixture id %q is empty", label)
	}
	id, err := ingest.NewSessionID(raw)
	if err != nil {
		t.Fatalf("authority fixture id %q: %v", label, err)
	}
	return id
}

func insertAuthoritySession(t *testing.T, ctx context.Context, db *Store, id ingest.SessionID, harness ingest.Harness, logicalParent *ingest.SessionID) {
	t.Helper()
	meta := planLibraryMeta(id, harness, logicalParent)
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{{
		Metadata:           &meta,
		PublicationCapture: true,
		CWDProvenance:      ingest.CWDSourceAbsent,
	}}); err != nil {
		t.Fatalf("insert authority session %s: %v", id, err)
	}
}

func activateAuthorityRelationship(t *testing.T, db *Store, child ingest.SessionID, state string, target *ingest.SessionID) {
	t.Helper()
	if !authorityTargetStateKnown(state) {
		t.Fatalf("authority fixture state %q is outside the relationship target-state set", state)
	}
	relationship := schema.SessionRelationship{
		Kind:        schema.SessionRelationshipStartedBy,
		TargetState: schema.RelationshipTargetState(state),
		Evidence:    schema.EvidenceNativeTyped,
	}
	switch schema.RelationshipTargetState(state) {
	case schema.RelationshipTargetUnknown:
		relationship.Evidence = schema.EvidenceUnknown
	case schema.RelationshipTargetConflictingCurrentNativeEvidence:
		relationship.Evidence = schema.EvidenceConflict
	}
	if target != nil {
		value := schema.SessionID(*target)
		relationship.TargetLocalID = &value
	}
	v2, blobs := authorityGeneration(schema.SessionID(child), "authority-generation", []schema.SessionRelationship{relationship})
	if err := activateTestGeneration(t, db, v2, blobs); err != nil {
		t.Fatalf("activate %s relationship for %s: %v", state, child, err)
	}
}

func authorityTargetStateKnown(state string) bool {
	for _, candidate := range schema.AllRelationshipTargetStates {
		if string(candidate) == state {
			return true
		}
	}
	return false
}

func assertAuthorityParentCache(t *testing.T, ctx context.Context, db *Store, child ingest.SessionID, want *ingest.SessionID) {
	t.Helper()
	got, err := db.ParentCacheForSession(ctx, child)
	if err != nil {
		t.Fatalf("ParentCacheForSession(%s): %v", child, err)
	}
	if want == nil {
		if got != nil {
			t.Fatalf("parent cache of %s = %q, want nil (active authority must not resurrect a legacy parent)", child, *got)
		}
		return
	}
	if got == nil || *got != string(*want) {
		t.Fatalf("parent cache of %s = %v, want %q", child, got, string(*want))
	}
}

// authorityGeneration builds the minimal valid complete managed generation for
// one authority relationship. It is synthetic and carries no private content.
func authorityGeneration(sid schema.SessionID, genID string, relationships []schema.SessionRelationship) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	refs := []string{"e_authority_0", "e_authority_1"}
	preview := "synthetic authority child input"
	output := "synthetic authority child tool output"
	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: schema.HarnessCodex, EntryType: schema.EntryTypeText,
			Role: schema.RoleUser, ContentPreview: &preview, SourceEntryRef: schema.SourceEntryRef(refs[0]),
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: schema.HarnessCodex, EntryType: schema.EntryTypeToolResult,
			Role: schema.RoleTool, ToolOutput: &output, SourceEntryRef: schema.SourceEntryRef(refs[1]),
		},
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  schema.HarnessCodex,
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
			Relationships: relationships,
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              []indexformat.ContentRecord{{Ref: schema.SourceEntryRef(refs[0])}, {Ref: schema.SourceEntryRef(refs[1])}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{schema.SourceEntryRef(refs[0])},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		schema.SourceEntryRef(refs[0]): []byte(preview),
		schema.SourceEntryRef(refs[1]): []byte(output),
	}
	return indexformat.V2{Generation: generation}, blobs
}

// openAuthorityStore opens a real SQLite store with managed-generation support
// and returns its directory so the case can reopen the same database.
func openAuthorityStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	db := openAuthorityStoreAt(t, dir)
	return db, dir
}

func reopenAuthorityStore(t *testing.T, dir string) *Store {
	t.Helper()
	return openAuthorityStoreAt(t, dir)
}

func openAuthorityStoreAt(t *testing.T, dir string) *Store {
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
	db, err := Open(filepath.Join(dir, "authority.db"), WithPoolSize(2), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker))
	if err != nil {
		t.Fatalf("open authority store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}
