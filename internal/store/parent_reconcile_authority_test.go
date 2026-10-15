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
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/parent_reconcile_authority.yaml
var parentReconcileAuthorityYAML []byte

//go:embed testdata/parent_reconcile_authority.manifest.yaml
var parentReconcileAuthorityManifest []byte

// The generation identifier the authority cases activate, and the two
// generation scopes a case can put that generation in: the active authority, or
// (with no active generation) the session's latest attempt.
const (
	authorityRelationshipGeneration = "authority-generation"
	authorityScopeActive            = "active"
	authorityScopeLatestAttempt     = "latest-attempt"
)

type parentReconcileAuthorityCase struct {
	Name              string `yaml:"name"`
	LegacyParent      string `yaml:"legacyParent"`
	AuthorityState    string `yaml:"authorityState"`
	AuthorityTarget   string `yaml:"authorityTarget"`
	Scope             string `yaml:"scope"`
	HarvestTarget     string `yaml:"harvestTarget"`
	ExpectParentCache string `yaml:"expectParentCache"`
	ExpectUpdates     int    `yaml:"expectUpdates"`
	// Arm selects the driver: empty or "reconcile" runs the authority
	// reconcile driver, "heal" runs the heal-reset branch, and "invalidate"
	// runs the bounded origin invalidation branch, which needs no harvest
	// target.
	Arm string `yaml:"arm"`
	// Harness is the stuck child's harness on the heal and invalidate arms
	// (codex or opencode); it is empty on reconcile-arm cases.
	Harness string `yaml:"harness"`
	// ExpectInvalidated is the invalidate arm's expected re-queued row count on
	// the first application.
	ExpectInvalidated int64 `yaml:"expectInvalidated"`
}

const (
	authorityArmReconcile  = "reconcile"
	authorityArmHeal       = "heal"
	authorityArmInvalidate = "invalidate"
	authorityArmWriteGuard = "write-guard"
)

// authorityArm normalizes the case's arm token; the empty token is the
// reconcile driver.
func authorityArm(tc parentReconcileAuthorityCase) string {
	if tc.Arm == "" {
		return authorityArmReconcile
	}
	return tc.Arm
}

type parentReconcileAuthorityFixture struct {
	Child     string                         `yaml:"child"`
	Legacy    string                         `yaml:"legacy"`
	Alternate string                         `yaml:"alternate"`
	Unstored  string                         `yaml:"unstored"`
	Uncached  string                         `yaml:"uncached"`
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
			switch authorityArm(tc) {
			case authorityArmReconcile:
				runParentReconcileAuthorityCase(t, fixture, tc)
			case authorityArmHeal:
				runParentReconcileHealResetCase(t, fixture, tc)
			case authorityArmInvalidate:
				runParentReconcileInvalidateCase(t, fixture, tc)
			case authorityArmWriteGuard:
				runParentReconcileWriteGuardCase(t, fixture, tc)
			}
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
		switch authorityArm(tc) {
		case authorityArmReconcile:
			if tc.HarvestTarget == "" {
				t.Fatalf("authority case %q names no harvest target", tc.Name)
			}
			if tc.Harness != "" || tc.ExpectInvalidated != 0 {
				t.Fatalf("authority reconcile case %q must not declare harness or expectInvalidated", tc.Name)
			}
		case authorityArmHeal:
			if tc.HarvestTarget == "" {
				t.Fatalf("authority heal case %q names no harvest target", tc.Name)
			}
			authorityIndependentHarness(t, tc)
		case authorityArmInvalidate:
			authorityIndependentHarness(t, tc)
		case authorityArmWriteGuard:
			if tc.HarvestTarget != "" || tc.ExpectUpdates != 0 || tc.ExpectInvalidated != 0 || tc.AuthorityState != "" || tc.Scope != "" {
				t.Fatalf("authority write-guard case %q declares a field only another arm uses", tc.Name)
			}
			authorityIndependentHarness(t, tc)
			stored := authorityOptionalID(t, fixture, tc.LegacyParent)
			logical := authorityOptionalID(t, fixture, tc.AuthorityTarget)
			want := authorityOptionalID(t, fixture, tc.ExpectParentCache)
			if stored == nil || logical == nil || want == nil {
				t.Fatalf("authority write-guard case %q must name legacyParent, authorityTarget and expectParentCache", tc.Name)
			}
			if *stored == *logical || *want != *stored {
				t.Fatalf("authority write-guard case %q must hand the writer a logical parent that differs from the populated cache it expects to survive", tc.Name)
			}
		default:
			t.Fatalf("authority case %q names unknown arm %q (want empty, %q, %q, %q, or %q)", tc.Name, tc.Arm, authorityArmReconcile, authorityArmHeal, authorityArmInvalidate, authorityArmWriteGuard)
		}
		switch tc.Scope {
		case "", authorityScopeActive, authorityScopeLatestAttempt:
		default:
			t.Fatalf("authority case %q declares the unknown generation scope %q", tc.Name, tc.Scope)
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
		if tc.Scope == authorityScopeLatestAttempt {
			// The generation's evidence is the session's only relationship
			// authority: no active generation exists, so the reverse lookup
			// must consult the latest attempt's own evidence.
			execGenerationSQL(t, db, `UPDATE sessions SET active_generation_id = NULL, latest_attempt_generation_id = '`+authorityRelationshipGeneration+`', latest_attempt_indexer_version = 1 WHERE session_id = '`+string(child)+`';`)
		}
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

// authorityIndependentHarness resolves the case's stuck-row harness, which must
// be one of the independently admitted harnesses the invalidation scopes.
func authorityIndependentHarness(t *testing.T, tc parentReconcileAuthorityCase) ingest.Harness {
	t.Helper()
	switch harness := ingest.Harness(tc.Harness); harness {
	case ingest.HarnessCodex, ingest.HarnessOpenCode:
		return harness
	default:
		t.Fatalf("authority case %q harness %q must be %q or %q", tc.Name, tc.Harness, ingest.HarnessCodex, ingest.HarnessOpenCode)
		return ""
	}
}

// runParentReconcileHealResetCase proves the reconcile heal re-queues the
// healed child's origin verdict: the child settles unknown at the current rule
// version while its parent is absent, the parent is stored and settled, and the
// real reverse lookup and reconciliation heal the cache. The healed child must
// come back at origin version 0 with its verdict unchanged, while the parent,
// which the heal does not touch, keeps its settled watermark. The result must
// survive a SQLite reopen.
func runParentReconcileHealResetCase(t *testing.T, fixture parentReconcileAuthorityFixture, tc parentReconcileAuthorityCase) {
	t.Helper()
	ctx := t.Context()
	db, dir := openAuthorityStore(t)
	harness := authorityIndependentHarness(t, tc)
	child := authorityRequiredID(t, "child", fixture.Child)
	legacyParent := authorityOptionalID(t, fixture, tc.LegacyParent)
	harvest := authorityOptionalID(t, fixture, tc.HarvestTarget)
	if harvest == nil {
		t.Fatalf("authority heal case %q harvest target %q is unknown", tc.Name, tc.HarvestTarget)
	}

	insertAuthoritySession(t, ctx, db, child, harness, legacyParent)
	settleAuthorityOrigin(t, ctx, db, child)
	assertAuthorityParentCache(t, ctx, db, child, nil)
	insertAuthoritySession(t, ctx, db, *harvest, harness, nil)
	settleAuthorityOrigin(t, ctx, db, *harvest)

	updates, err := db.ListUncachedChildrenOfParents(ctx, []ingest.SessionID{*harvest}, []ingest.Harness{harness})
	if err != nil {
		t.Fatalf("ListUncachedChildrenOfParents: %v", err)
	}
	if len(updates) != tc.ExpectUpdates {
		t.Fatalf("authority heal case %q reverse lookup returned %d updates (%+v), want %d", tc.Name, len(updates), updates, tc.ExpectUpdates)
	}
	if err := db.ReconcileParentCache(ctx, updates); err != nil {
		t.Fatalf("ReconcileParentCache: %v", err)
	}
	want := authorityOptionalID(t, fixture, tc.ExpectParentCache)
	expectedOrigins := map[ingest.SessionID]int{child: 0, *harvest: ingest.OriginRuleVersion}
	assertAuthorityParentCache(t, ctx, db, child, want)
	assertAuthorityOrigins(t, ctx, db, expectedOrigins)

	if err := db.Close(); err != nil {
		t.Fatalf("close before reopen: %v", err)
	}
	reopened := reopenAuthorityStore(t, dir)
	t.Cleanup(func() { _ = reopened.Close() })
	assertAuthorityParentCache(t, ctx, reopened, child, want)
	assertAuthorityOrigins(t, ctx, reopened, expectedOrigins)
}

// runParentReconcileInvalidateCase proves the bounded origin invalidation's
// scope directly on the store. The shared parent is stored first so each
// child's parent link lands; the stuck child (the case's harness), a Claude
// Code control under the same parent, and the parent root itself all settle
// unknown at the current rule version. Only the parent-linked independently
// admitted child is re-queued: the Claude Code control keeps its watermark, and
// the parent root keeps its settled verdict because it carries no parent link.
// A second application re-queues nothing. A case that declares authorityState
// activates that relationship for the child first, so the child is parent-linked
// by evidence while its cache follows expectParentCache: the re-queue set is the
// logical-parent answer, never the raw cache.
func runParentReconcileInvalidateCase(t *testing.T, fixture parentReconcileAuthorityFixture, tc parentReconcileAuthorityCase) {
	t.Helper()
	ctx := t.Context()
	db, _ := openAuthorityStore(t)
	harness := authorityIndependentHarness(t, tc)
	child := authorityRequiredID(t, "child", fixture.Child)
	parent := authorityRequiredID(t, "legacy", fixture.Legacy)
	control := authorityRequiredID(t, "alternate", fixture.Alternate)

	insertAuthoritySession(t, ctx, db, parent, harness, nil)
	insertAuthoritySession(t, ctx, db, child, harness, &parent)
	insertAuthoritySession(t, ctx, db, control, ingest.HarnessClaudeCode, &parent)
	wantCache := &parent
	if tc.AuthorityState != "" {
		// An authoritative activation rewrites the child's cache from its
		// relationship evidence, so the fixture names what it expects (nil when
		// the evidence target is not stored).
		activateAuthorityRelationship(t, db, child, tc.AuthorityState, authorityOptionalID(t, fixture, tc.AuthorityTarget))
		wantCache = authorityOptionalID(t, fixture, tc.ExpectParentCache)
	}
	for _, id := range []ingest.SessionID{parent, child, control} {
		settleAuthorityOrigin(t, ctx, db, id)
	}
	assertAuthorityParentCache(t, ctx, db, child, wantCache)
	assertAuthorityParentCache(t, ctx, db, control, &parent)
	assertAuthorityParentCache(t, ctx, db, parent, nil)

	requeued, err := db.InvalidateStaleParentOrigins(ctx)
	if err != nil {
		t.Fatalf("InvalidateStaleParentOrigins: %v", err)
	}
	if requeued != tc.ExpectInvalidated {
		t.Fatalf("authority invalidate case %q re-queued %d rows, want %d", tc.Name, requeued, tc.ExpectInvalidated)
	}
	assertAuthorityOrigins(t, ctx, db, map[ingest.SessionID]int{
		child:   0,
		control: ingest.OriginRuleVersion,
		parent:  ingest.OriginRuleVersion,
	})

	again, err := db.InvalidateStaleParentOrigins(ctx)
	if err != nil {
		t.Fatalf("InvalidateStaleParentOrigins second application: %v", err)
	}
	if again != 0 {
		t.Fatalf("authority invalidate case %q second application re-queued %d rows, want 0 (the invalidation must be idempotent)", tc.Name, again)
	}
}

// runParentReconcileWriteGuardCase drives the shared cache writer's own
// contract directly: the NULL-only statement the reverse heal and the migration
// conversion both call. Both production callers already decide NULL-only in Go
// before they call it, so this is the one arm that observes the declaration's
// additional invariant; it hands the writer a logical parent that DIFFERS from
// the child's populated cache, so a clause-free statement overwrites the cache
// and moves the watermark. The NULL-cached session in the same arm is the
// positive control that the writer is live: a disabled writer heals nothing.
// The requeue that rides the same statement is pinned by the heal arm, on the
// production path.
func runParentReconcileWriteGuardCase(t *testing.T, fixture parentReconcileAuthorityFixture, tc parentReconcileAuthorityCase) {
	t.Helper()
	ctx := t.Context()
	db, _ := openAuthorityStore(t)
	harness := authorityIndependentHarness(t, tc)
	child := authorityRequiredID(t, "child", fixture.Child)
	uncached := authorityRequiredID(t, "uncached", fixture.Uncached)
	stored := authorityOptionalID(t, fixture, tc.LegacyParent)
	logical := authorityOptionalID(t, fixture, tc.AuthorityTarget)
	if stored == nil || logical == nil {
		t.Fatalf("authority write-guard case %q names no stored parent or logical parent", tc.Name)
	}

	insertAuthoritySession(t, ctx, db, *stored, harness, nil)
	insertAuthoritySession(t, ctx, db, *logical, harness, nil)
	insertAuthoritySession(t, ctx, db, child, harness, stored)
	insertAuthoritySession(t, ctx, db, uncached, harness, nil)
	settleAuthorityOrigin(t, ctx, db, child)
	settleAuthorityOrigin(t, ctx, db, uncached)

	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatalf("authority write-guard case %q: take connection: %v", tc.Name, err)
	}
	populatedErr := writeParentCacheOnConn(conn, child, logical)
	uncachedErr := writeParentCacheOnConn(conn, uncached, logical)
	db.pool.Put(conn)
	if populatedErr != nil {
		t.Fatalf("authority write-guard case %q: write past a populated cache: %v", tc.Name, populatedErr)
	}
	if uncachedErr != nil {
		t.Fatalf("authority write-guard case %q: heal a nil cache: %v", tc.Name, uncachedErr)
	}

	// The populated cache is authoritative: neither it nor its settled
	// watermark may move.
	assertAuthorityParentCache(t, ctx, db, child, authorityOptionalID(t, fixture, tc.ExpectParentCache))
	assertAuthorityOrigins(t, ctx, db, map[ingest.SessionID]int{child: ingest.OriginRuleVersion})
	// The writer is live: the NULL-cached session must be healed.
	assertAuthorityParentCache(t, ctx, db, uncached, logical)
}

// settleAuthorityOrigin records the unknown verdict at the current rule
// version, the state a resolve pass leaves a row it judged without evidence.
func settleAuthorityOrigin(t *testing.T, ctx context.Context, db *Store, id ingest.SessionID) {
	t.Helper()
	if err := db.UpdateOriginState(ctx, id, "unknown", ingest.OriginRuleVersion); err != nil {
		t.Fatalf("settle origin of %s: %v", id, err)
	}
}

// assertAuthorityOrigins re-reads origin state through the resolve pass's own
// stale-row listing and requires each named row at the given version with its
// verdict still unknown.
func assertAuthorityOrigins(t *testing.T, ctx context.Context, db *Store, want map[ingest.SessionID]int) {
	t.Helper()
	rows, err := db.ListStaleOriginSessions(ctx, ingest.OriginRuleVersion+1)
	if err != nil {
		t.Fatalf("ListStaleOriginSessions: %v", err)
	}
	got := make(map[ingest.SessionID]ingest.StoredOriginRow, len(rows))
	for _, row := range rows {
		got[row.SessionID] = row
	}
	for id, version := range want {
		row, ok := got[id]
		if !ok {
			t.Fatalf("origin state of %s missing from the stale-origin listing", id)
		}
		if row.OriginVersion != version || row.StoredOrigin != "unknown" {
			t.Fatalf("origin state of %s = %q@%d, want \"unknown\"@%d", id, row.StoredOrigin, row.OriginVersion, version)
		}
	}
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
	case "unstored":
		id := authorityRequiredID(t, "unstored", fixture.Unstored)
		return &id
	case "uncached":
		id := authorityRequiredID(t, "uncached", fixture.Uncached)
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
	v2, blobs := authorityGeneration(schema.SessionID(child), authorityRelationshipGeneration, []schema.SessionRelationship{relationship})
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

// assertAuthorityParentCache reads the availability cache directly. It is a
// test-local statement by design: the store no longer exposes a method that
// returns the raw column as a parent answer, and this assertion needs the raw
// column to prove the cache stayed NULL while the accessor answers evidence.
func assertAuthorityParentCache(t *testing.T, ctx context.Context, db *Store, child ingest.SessionID, want *ingest.SessionID) {
	t.Helper()
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection to read the parent cache of %s: %v", child, err)
	}
	defer db.pool.Put(conn)
	var got *string
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(child)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				v := stmt.ColumnText(0)
				got = &v
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read the parent cache of %s: %v", child, err)
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
