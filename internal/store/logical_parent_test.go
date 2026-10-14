package store

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/logical_parent.yaml
var logicalParentYAML []byte

//go:embed testdata/logical_parent.manifest.yaml
var logicalParentManifestYAML []byte

// The logical-parent answer family. Every case stores real sessions, activates
// real generations with real relationship evidence, and reads through the
// production readers; only the latest-attempt pointer is arranged directly,
// because no production path installs a latest attempt beside no active
// generation (the eligibility predicate always keeps the active authority).
// The evidence rows themselves are always production-written.
type logicalParentCase struct {
	Name                  string `yaml:"name"`
	State                 string `yaml:"state"`
	ActiveEvidence        string `yaml:"activeEvidence"`
	LatestAttemptEvidence string `yaml:"latestAttemptEvidence"`
	CacheParent           string `yaml:"cacheParent"`
	DocumentParent        string `yaml:"documentParent"`
}

type logicalParentFixture struct {
	Child                   string              `yaml:"child"`
	Target                  string              `yaml:"target"`
	Alternate               string              `yaml:"alternate"`
	ActiveGeneration        string              `yaml:"activeGeneration"`
	LatestAttemptGeneration string              `yaml:"latestAttemptGeneration"`
	RequiredNames           []string            `yaml:"requiredNames"`
	Cases                   []logicalParentCase `yaml:"cases"`
}

// The closed state set. A state outside it is a fixture error, never a
// silently skipped row.
const (
	logicalParentStateLatestAttemptOnly        = "latest-attempt-only"
	logicalParentStateActiveAndLatestAttempt   = "active-and-latest-attempt"
	logicalParentStateActiveWithoutEvidence    = "active-without-evidence"
	logicalParentStateNoEvidence               = "no-evidence"
	logicalParentStateManagedTargetUnstored    = "managed-target-unstored"
	logicalParentStateCacheAgrees              = "cache-agrees"
	logicalParentStateConvertedPair            = "converted-pair"
	logicalParentStatePublicationBundle        = "publication-bundle"
	logicalParentStateAvailabilityRaw          = "availability-raw"
	logicalParentStateOriginListing            = "origin-listing"
	logicalParentEvidenceTargetKnown           = "target_known"
	logicalParentPublicationEvidenceGeneration = "gen_logical_parent_publication_evidence"
)

// TestLogicalParentAnswer drives the whole family.
func TestLogicalParentAnswer(t *testing.T) {
	t.Parallel()
	fixture := loadLogicalParentFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			runLogicalParentCase(t, fixture, tc)
		})
	}
}

func loadLogicalParentFixture(t *testing.T) logicalParentFixture {
	t.Helper()
	var fixture logicalParentFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(logicalParentYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode logical_parent.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(logicalParentManifestYAML)
	if err != nil {
		t.Fatalf("decode logical_parent manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases))
	seen := make(map[string]struct{}, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		if tc.Name == "" {
			t.Fatal("logical parent fixture case with empty name")
		}
		if _, duplicate := seen[tc.Name]; duplicate {
			t.Fatalf("logical parent fixture repeats case %q", tc.Name)
		}
		seen[tc.Name] = struct{}{}
		switch tc.State {
		case logicalParentStateLatestAttemptOnly,
			logicalParentStateActiveAndLatestAttempt,
			logicalParentStateActiveWithoutEvidence,
			logicalParentStateNoEvidence,
			logicalParentStateManagedTargetUnstored,
			logicalParentStateCacheAgrees,
			logicalParentStateConvertedPair,
			logicalParentStatePublicationBundle,
			logicalParentStateAvailabilityRaw,
			logicalParentStateOriginListing:
		default:
			t.Fatalf("logical parent case %q declares the unknown state %q", tc.Name, tc.State)
		}
		for _, token := range []string{tc.ActiveEvidence, tc.LatestAttemptEvidence, tc.CacheParent, tc.DocumentParent} {
			switch token {
			case "", "target", "alternate":
			default:
				t.Fatalf("logical parent case %q names the unknown id token %q", tc.Name, token)
			}
		}
		actual = append(actual, tc.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "logical parent"); err != nil {
		t.Fatal(err)
	}
	if fixture.ActiveGeneration == "" || fixture.LatestAttemptGeneration == "" {
		t.Fatal("logical parent fixture must name both generation identifiers")
	}
	return fixture
}

// logicalParentTokenID resolves one fixture id token, or nil for the empty
// token.
func logicalParentTokenID(t *testing.T, fixture logicalParentFixture, token string) *schema.SessionID {
	t.Helper()
	var raw string
	switch token {
	case "":
		return nil
	case "target":
		raw = fixture.Target
	case "alternate":
		raw = fixture.Alternate
	default:
		t.Fatalf("logical parent fixture names the unknown id token %q", token)
	}
	if raw == "" {
		t.Fatalf("logical parent fixture token %q resolves to no identifier", token)
	}
	id, err := schema.NewSessionID(raw)
	if err != nil {
		t.Fatalf("logical parent fixture identifier %q: %v", raw, err)
	}
	return &id
}

func runLogicalParentCase(t *testing.T, fixture logicalParentFixture, tc logicalParentCase) {
	t.Helper()
	ctx := t.Context()
	db, root := openGenerationStore(t)
	child, err := schema.NewSessionID(fixture.Child)
	if err != nil {
		t.Fatalf("logical parent fixture child identifier: %v", err)
	}

	switch tc.State {
	case logicalParentStateLatestAttemptOnly:
		runLogicalParentLatestAttemptOnly(t, ctx, db, fixture, tc, child)
	case logicalParentStateActiveAndLatestAttempt:
		runLogicalParentActiveAndLatestAttempt(t, ctx, db, fixture, tc, child)
	case logicalParentStateActiveWithoutEvidence:
		runLogicalParentActiveWithoutEvidence(t, ctx, db, fixture, tc, child)
	case logicalParentStateNoEvidence:
		runLogicalParentNoEvidence(t, ctx, db, fixture, tc, child)
	case logicalParentStateManagedTargetUnstored:
		runLogicalParentManagedTargetUnstored(t, ctx, db, fixture, tc, child)
	case logicalParentStateCacheAgrees:
		runLogicalParentCacheAgrees(t, ctx, db, fixture, tc, child)
	case logicalParentStateConvertedPair:
		runLogicalParentConvertedPair(t, ctx, db, root, fixture, tc, child)
	case logicalParentStatePublicationBundle:
		runLogicalParentPublicationBundle(t, ctx, db, fixture, tc, child)
	case logicalParentStateAvailabilityRaw:
		runLogicalParentAvailabilityRaw(t, ctx, db, fixture, tc, child)
	case logicalParentStateOriginListing:
		runLogicalParentOriginListing(t, ctx, db, fixture, tc, child)
	default:
		t.Fatalf("logical parent case %q declares the unknown state %q", tc.Name, tc.State)
	}
}

// --- seeding -----------------------------------------------------------------

// insertLogicalParentSession stores one synthetic session row through the real
// metadata writer with no logical parent of its own.
func insertLogicalParentSession(t *testing.T, ctx context.Context, db *Store, id schema.SessionID) {
	t.Helper()
	meta := planLibraryMeta(ingest.SessionID(id), ingest.HarnessCodex, nil)
	if err := db.InsertSessions(ctx, []ingest.StoreEntry{{
		Metadata:           &meta,
		PublicationCapture: true,
		CWDProvenance:      ingest.CWDSourceAbsent,
	}}); err != nil {
		t.Fatalf("insert logical parent session %s: %v", id, err)
	}
}

// activateLogicalParentEvidence activates one managed generation for the child
// carrying at most one started_by relationship. An empty state activates a
// generation with NO started_by row at all, which is the state the active
// generation must be in for the "active without evidence" boundary.
func activateLogicalParentEvidence(t *testing.T, db *Store, child schema.SessionID, generationID, state string, target *schema.SessionID) {
	t.Helper()
	var relationships []schema.SessionRelationship
	if state != "" {
		relationship := schema.SessionRelationship{
			Kind:        schema.SessionRelationshipStartedBy,
			TargetState: schema.RelationshipTargetState(state),
			Evidence:    schema.EvidenceNativeTyped,
		}
		if target != nil {
			value := schema.SessionID(*target)
			relationship.TargetLocalID = &value
		}
		relationships = []schema.SessionRelationship{relationship}
	}
	v2, blobs := authorityGeneration(child, generationID, relationships)
	if err := activateTestGeneration(t, db, v2, blobs); err != nil {
		t.Fatalf("activate generation %s with evidence state %q: %v", generationID, state, err)
	}
}

// arrangeLogicalParentPointers writes the generation pointers and the raw
// availability cache directly. Only this arrangement is synthetic; every
// generation, evidence row and cache value it names was produced by the real
// writers above (or is deliberately cleared to express the state under test).
func arrangeLogicalParentPointers(t *testing.T, db *Store, child schema.SessionID, active, latestAttempt, cache *string) {
	t.Helper()
	sqlValue := func(value *string) string {
		if value == nil {
			return "NULL"
		}
		return "'" + *value + "'"
	}
	// A latest-attempt pointer always carries a producer stats document in
	// production; the readers decode it when they serve that attempt, so the
	// arrangement supplies a valid one instead of an empty string.
	statsSQL := "NULL"
	if latestAttempt != nil {
		inputCount := int64(1)
		stats, err := json.Marshal(schema.SessionStats{TurnCount: 2, ToolCallCount: 1, InputSubmissionCount: &inputCount})
		if err != nil {
			t.Fatalf("encode the arranged latest-attempt stats: %v", err)
		}
		statsSQL = "'" + string(stats) + "'"
	}
	execGenerationSQL(t, db, `UPDATE sessions SET active_generation_id = `+sqlValue(active)+
		`, latest_attempt_generation_id = `+sqlValue(latestAttempt)+
		`, latest_attempt_indexer_version = 1, latest_attempt_captured_at_ms = 2, latest_attempt_stats_json = `+statsSQL+
		`, parent_id = `+sqlValue(cache)+
		` WHERE session_id = '`+string(child)+`';`)
}

// --- assertions --------------------------------------------------------------

// logicalParentCacheColumn reads the availability cache directly. The store no
// longer exposes a method that returns the raw column as a parent answer, and
// this family needs the raw column to prove the cache stayed NULL while the
// answer came from evidence.
func logicalParentCacheColumn(t *testing.T, ctx context.Context, db *Store, child schema.SessionID) *schema.SessionID {
	t.Helper()
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection to read the parent cache of %s: %v", child, err)
	}
	defer db.pool.Put(conn)
	var got *schema.SessionID
	if err := sqlitex.ExecuteTransient(conn, `SELECT parent_id FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(child)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			if stmt.ColumnType(0) != sqlite.TypeNull {
				value := schema.SessionID(stmt.ColumnText(0))
				got = &value
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read the parent cache of %s: %v", child, err)
	}
	return got
}

func assertLogicalParentCache(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	got := logicalParentCacheColumn(t, ctx, db, child)
	if !logicalParentEqual(got, want) {
		t.Fatalf("availability cache of %s = %s, want %s", child, logicalParentString(got), logicalParentString(want))
	}
}

func logicalParentEqual(left, right *schema.SessionID) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func logicalParentString(value *schema.SessionID) string {
	if value == nil {
		return "nil"
	}
	return string(*value)
}

func assertLogicalParentAnswer(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	got, err := db.LogicalParentForSession(ctx, ingest.SessionID(child))
	if err != nil {
		t.Fatalf("LogicalParentForSession(%s): %v", child, err)
	}
	if !logicalParentEqual(logicalParentFromString(got), want) {
		t.Fatalf("logical parent of %s = %v, want %s", child, got, logicalParentString(want))
	}
}

func logicalParentFromString(value *string) *schema.SessionID {
	if value == nil {
		return nil
	}
	id := schema.SessionID(*value)
	return &id
}

func assertLogicalParentListRow(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	rows, err := db.SessionsByIDs(ctx, []string{string(child)})
	if err != nil {
		t.Fatalf("SessionsByIDs(%s): %v", child, err)
	}
	if len(rows) != 1 {
		t.Fatalf("SessionsByIDs(%s) returned %d rows, want 1", child, len(rows))
	}
	if !logicalParentEqual(logicalParentFromString(rows[0].ParentID), want) {
		t.Fatalf("list row parent of %s = %v, want %s", child, rows[0].ParentID, logicalParentString(want))
	}
}

func assertLogicalParentDetailRow(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	row, err := db.SessionDetailByID(ctx, string(child))
	if err != nil {
		t.Fatalf("SessionDetailByID(%s): %v", child, err)
	}
	if row == nil {
		t.Fatalf("SessionDetailByID(%s) returned no row", child)
	}
	if !logicalParentEqual(logicalParentFromString(row.ParentID), want) {
		t.Fatalf("detail row parent of %s = %v, want %s", child, row.ParentID, logicalParentString(want))
	}
}

// assertLogicalParentChildList asserts whether the child appears under the
// named parent's reverse filter. The filter embeds the same rendering every
// other answer uses, so a child whose generation names a different parent must
// not appear here.
func assertLogicalParentChildList(t *testing.T, ctx context.Context, db *Store, parent, child schema.SessionID, wantListed bool) {
	t.Helper()
	rows, err := db.ChildSessionsForParent(ctx, string(parent))
	if err != nil {
		t.Fatalf("ChildSessionsForParent(%s): %v", parent, err)
	}
	found := false
	for _, row := range rows {
		if row.SessionID == string(child) {
			found = true
		}
	}
	if found != wantListed {
		t.Fatalf("child list of %s lists %s = %t, want %t (rows=%d)", parent, child, found, wantListed, len(rows))
	}
}

func assertLogicalParentGrouping(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, wantState string, wantTarget *schema.SessionID) {
	t.Helper()
	rows, err := db.GroupingEvidenceForSessions(ctx, []string{string(child)})
	if err != nil {
		t.Fatalf("GroupingEvidenceForSessions(%s): %v", child, err)
	}
	row, found := rows[string(child)]
	if !found {
		t.Fatalf("GroupingEvidenceForSessions(%s) returned no row", child)
	}
	if string(row.OwnerState) != wantState {
		t.Fatalf("grouping owner state of %s = %q, want %q", child, row.OwnerState, wantState)
	}
	gotTarget := (*schema.SessionID)(nil)
	if row.OwnerTargetLocalID != nil {
		value := schema.SessionID(*row.OwnerTargetLocalID)
		gotTarget = &value
	}
	if !logicalParentEqual(gotTarget, wantTarget) {
		t.Fatalf("grouping owner target of %s = %s, want %s", child, logicalParentString(gotTarget), logicalParentString(wantTarget))
	}
}

// assertLogicalParentLegacySnapshot reads the preserved legacy snapshot path
// under an explicit last-good selection. For a session with no active
// generation that path is the certificate/file-backed authority, and its
// parent comes from the same rendering as every other answer.
func assertLogicalParentLegacySnapshot(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	served := (*schema.SessionID)(nil)
	if err := db.WithSessionSnapshotSelected(ctx, child, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		served = snapshot.Session.ParentSessionID
		return nil
	}); err != nil {
		t.Fatalf("WithSessionSnapshotSelected(LastGood, %s): %v", child, err)
	}
	if !logicalParentEqual(served, want) {
		t.Fatalf("legacy snapshot parent of %s = %s, want %s", child, logicalParentString(served), logicalParentString(want))
	}
}

// assertLogicalParentStoredMetadata reads the recovered document's parentUuid.
func assertLogicalParentStoredMetadata(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, want *schema.SessionID) {
	t.Helper()
	data, err := db.ReadStoredMetadata(ctx, ingest.SessionID(child))
	if err != nil {
		t.Fatalf("ReadStoredMetadata(%s): %v", child, err)
	}
	var document struct {
		ParentUUID *string `json:"parentUuid"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("decode recovered metadata of %s: %v", child, err)
	}
	got := (*schema.SessionID)(nil)
	if document.ParentUUID != nil {
		value := schema.SessionID(*document.ParentUUID)
		got = &value
	}
	if !logicalParentEqual(got, want) {
		t.Fatalf("recovered parentUuid of %s = %s, want %s", child, logicalParentString(got), logicalParentString(want))
	}
}

// assertLogicalParentEvidenceRow proves the durable evidence row exists on the
// named generation: the family's states are only meaningful when the evidence
// is really stored.
func assertLogicalParentEvidenceRow(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, generationID string, want *schema.SessionID) {
	t.Helper()
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection to read relationship evidence of %s: %v", child, err)
	}
	defer db.pool.Put(conn)
	var got *schema.SessionID
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT target_local_id FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ? AND kind = 'started_by'`, &sqlitex.ExecOptions{
		Args: []any{string(child), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			if stmt.ColumnType(0) != sqlite.TypeNull && stmt.ColumnText(0) != "" {
				value := schema.SessionID(stmt.ColumnText(0))
				got = &value
			}
			return nil
		},
	}); err != nil {
		t.Fatalf("read relationship evidence of %s generation %s: %v", child, generationID, err)
	}
	if !found {
		t.Fatalf("session %s has no started_by evidence row on generation %s; the state under test is not stored", child, generationID)
	}
	if !logicalParentEqual(got, want) {
		t.Fatalf("stored evidence target of %s = %s, want %s", child, logicalParentString(got), logicalParentString(want))
	}
}

// --- states ------------------------------------------------------------------

func runLogicalParentLatestAttemptOnly(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	target := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if target == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *target)
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.LatestAttemptGeneration, logicalParentEvidenceTargetKnown, target)
	arrangeLogicalParentPointers(t, db, child, nil, &fixture.LatestAttemptGeneration, nil)

	assertLogicalParentEvidenceRow(t, ctx, db, child, fixture.LatestAttemptGeneration, target)
	assertLogicalParentAnswer(t, ctx, db, child, target)
	assertLogicalParentListRow(t, ctx, db, child, target)
	assertLogicalParentDetailRow(t, ctx, db, child, target)
	assertLogicalParentChildList(t, ctx, db, *target, child, true)
	assertLogicalParentGrouping(t, ctx, db, child, logicalParentEvidenceTargetKnown, target)
	assertLogicalParentLegacySnapshot(t, ctx, db, child, target)
	assertLogicalParentStoredMetadata(t, ctx, db, child, target)
	// The documented exception: the retained-only state never writes the cache,
	// so the answer comes from evidence while the cache stays NULL.
	assertLogicalParentCache(t, ctx, db, child, nil)
}

func runLogicalParentActiveAndLatestAttempt(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	activeTarget := logicalParentTokenID(t, fixture, tc.ActiveEvidence)
	latestTarget := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if activeTarget == nil || latestTarget == nil {
		t.Fatalf("case %q must name both an active and a latest-attempt evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *activeTarget)
	insertLogicalParentSession(t, ctx, db, *latestTarget)
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.ActiveGeneration, logicalParentEvidenceTargetKnown, activeTarget)
	activateLogicalParentEvidence(t, db, child, fixture.LatestAttemptGeneration, logicalParentEvidenceTargetKnown, latestTarget)
	active := fixture.ActiveGeneration
	latest := fixture.LatestAttemptGeneration
	cache := string(*activeTarget)
	arrangeLogicalParentPointers(t, db, child, &active, &latest, &cache)

	// The active generation wins at session level, even though the cache and
	// the latest attempt both name something else.
	assertLogicalParentAnswer(t, ctx, db, child, activeTarget)
	assertLogicalParentListRow(t, ctx, db, child, activeTarget)
	assertLogicalParentDetailRow(t, ctx, db, child, activeTarget)
	assertLogicalParentChildList(t, ctx, db, *activeTarget, child, true)
	assertLogicalParentChildList(t, ctx, db, *latestTarget, child, false)

	// The generation-scoped read of the latest attempt answers its OWN target
	// and stays coherent, which is what the mirror validator refused before the
	// document took its parent from the served generation.
	served := (*schema.SessionID)(nil)
	if err := db.WithSessionPreviewSnapshot(ctx, child, func(snapshot indexformat.ReadSnapshot) error {
		served = snapshot.Session.ParentSessionID
		if err := snapshot.Validate(); err != nil {
			t.Fatalf("the latest-attempt snapshot of %s is not a coherent read: %v", child, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("WithSessionPreviewSnapshot(%s): %v", child, err)
	}
	if !logicalParentEqual(served, latestTarget) {
		t.Fatalf("latest-attempt snapshot parent of %s = %s, want %s", child, logicalParentString(served), logicalParentString(latestTarget))
	}
}

func runLogicalParentActiveWithoutEvidence(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	latestTarget := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if latestTarget == nil {
		t.Fatalf("case %q names no latest-attempt evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *latestTarget)
	insertLogicalParentSession(t, ctx, db, child)
	// The active generation carries NO started_by row at all: the latest
	// attempt is not consulted, and the answer falls back to a NULL cache.
	activateLogicalParentEvidence(t, db, child, fixture.ActiveGeneration, "", nil)
	activateLogicalParentEvidence(t, db, child, fixture.LatestAttemptGeneration, logicalParentEvidenceTargetKnown, latestTarget)
	active := fixture.ActiveGeneration
	latest := fixture.LatestAttemptGeneration
	arrangeLogicalParentPointers(t, db, child, &active, &latest, nil)

	assertLogicalParentAnswer(t, ctx, db, child, nil)
	assertLogicalParentChildList(t, ctx, db, *latestTarget, child, false)
	assertLogicalParentGrouping(t, ctx, db, child, "", nil)
	assertLogicalParentCache(t, ctx, db, child, nil)
}

func runLogicalParentNoEvidence(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	cacheParent := logicalParentTokenID(t, fixture, tc.CacheParent)
	if cacheParent == nil {
		t.Fatalf("case %q names no cache parent", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *cacheParent)
	insertLogicalParentSession(t, ctx, db, child)
	cache := string(*cacheParent)
	arrangeLogicalParentPointers(t, db, child, nil, nil, &cache)
	assertLogicalParentAnswer(t, ctx, db, child, cacheParent)

	// A NULL cache with no evidence answers nil: the cache is the answer here,
	// and a nil answer never proves a root by itself.
	arrangeLogicalParentPointers(t, db, child, nil, nil, nil)
	assertLogicalParentAnswer(t, ctx, db, child, nil)
}

func runLogicalParentManagedTargetUnstored(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	target := logicalParentTokenID(t, fixture, tc.ActiveEvidence)
	if target == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	// The target is deliberately NOT stored: the cache must stay NULL while the
	// answer still names the target.
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.ActiveGeneration, logicalParentEvidenceTargetKnown, target)

	assertLogicalParentCache(t, ctx, db, child, nil)
	assertLogicalParentAnswer(t, ctx, db, child, target)

	prior, err := db.ReadGenerationPrior(ctx, child)
	if err != nil {
		t.Fatalf("ReadGenerationPrior(%s): %v", child, err)
	}
	if prior == nil || prior.Metadata == nil {
		t.Fatalf("ReadGenerationPrior(%s) returned no metadata document", child)
	}
	if !logicalParentEqual(prior.Metadata.ParentUUID, target) {
		t.Fatalf("prior document parent of %s = %s, want %s", child, logicalParentString(prior.Metadata.ParentUUID), logicalParentString(target))
	}
}

func runLogicalParentCacheAgrees(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	target := logicalParentTokenID(t, fixture, tc.ActiveEvidence)
	if target == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *target)
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.ActiveGeneration, logicalParentEvidenceTargetKnown, target)

	// Wherever the cache is written it equals the accessor's answer. The
	// latest-attempt-only case above is the documented exception.
	assertLogicalParentAnswer(t, ctx, db, child, target)
	assertLogicalParentCache(t, ctx, db, child, target)
}

func runLogicalParentConvertedPair(t *testing.T, ctx context.Context, db *Store, root string, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	documentParent := logicalParentTokenID(t, fixture, tc.DocumentParent)
	if documentParent == nil {
		t.Fatalf("case %q names no document parent", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *documentParent)
	seedGenerationSession(t, db, string(child))

	// A V1 pair: the document carries the logical parent and the generation
	// carries no relationship evidence at all.
	v2, blobs := buildTestGeneration(t, child, "gen_logical_parent_v1", "v1 text", "v1 input", "v1 output")
	parent := schema.SessionID(*documentParent)
	v2.Generation.Metadata.ParentUUID = &parent
	stampSeedMetadataHash(&v2)
	seedFileBackedGeneration(t, db, root, child, v2, blobs, true)
	// The row never captured the linkage: the document is the pair's only
	// durable logical record, which is the branch the conversion heals. The
	// active generation stays in place; only the availability cache is cleared.
	execGenerationSQL(t, db, `UPDATE sessions SET parent_id = NULL WHERE session_id = '`+string(child)+`';`)

	outcome, err := db.MigrateSession(ctx, child)
	if err != nil {
		t.Fatalf("MigrateSession(%s): %v", child, err)
	}
	if outcome != MigrateOutcomeConverted {
		t.Fatalf("MigrateSession(%s) outcome = %q, want converted", child, outcome)
	}
	assertLogicalParentAnswer(t, ctx, db, child, documentParent)
	assertLogicalParentCache(t, ctx, db, child, documentParent)
}

func runLogicalParentPublicationBundle(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	evidenceTarget := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if evidenceTarget == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *evidenceTarget)
	seedLogicalParentPublicationSession(t, ctx, db, child)

	// The generation's evidence answers the target while the availability cache
	// stays NULL, so the no-active publication read must take its parent from
	// the evidence rather than from the stored capture document.
	seedLogicalParentPublicationEvidence(t, ctx, db, child, evidenceTarget)

	bundleParent := (*schema.SessionID)(nil)
	if err := db.WithCommittedPublicationInputSelected(ctx, ingest.SessionID(child), ingest.SessionReadSelection{LastGood: true}, func(input ingest.PublicationInputBundle) error {
		if input.Readiness != ingest.PublicationReady {
			return fmt.Errorf("logical parent fixture: the publication bundle of %s was not ready, readiness=%s", child, input.Readiness)
		}
		bundleParent = input.Metadata.ParentUUID
		return nil
	}); err != nil {
		t.Fatalf("WithCommittedPublicationInputSelected(LastGood, %s): %v", child, err)
	}
	if !logicalParentEqual(bundleParent, evidenceTarget) {
		t.Fatalf("publication bundle parent of %s = %s, want the evidence target %s", child, logicalParentString(bundleParent), logicalParentString(evidenceTarget))
	}
}

func runLogicalParentAvailabilityRaw(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	target := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if target == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *target)
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.LatestAttemptGeneration, logicalParentEvidenceTargetKnown, target)
	arrangeLogicalParentPointers(t, db, child, nil, &fixture.LatestAttemptGeneration, nil)

	// The evidence answers the target...
	assertLogicalParentAnswer(t, ctx, db, child, target)
	// ...while the availability readers keep the raw cache: they answer which
	// stored row exists, not who the parent is.
	_, parentID, err := db.LookupSessionLocation(ctx, ingest.SessionID(child))
	if err != nil {
		t.Fatalf("LookupSessionLocation(%s): %v", child, err)
	}
	if parentID != "" {
		t.Fatalf("LookupSessionLocation parent of %s = %q, want the raw cache (empty)", child, parentID)
	}
	locations, err := db.BulkLookupSessionLocations(ctx, []ingest.SessionID{ingest.SessionID(child)})
	if err != nil {
		t.Fatalf("BulkLookupSessionLocations(%s): %v", child, err)
	}
	location, found := locations[ingest.SessionID(child)]
	if !found {
		t.Fatalf("BulkLookupSessionLocations(%s) returned no row", child)
	}
	if location.ParentID != "" {
		t.Fatalf("bulk location parent of %s = %q, want the raw cache (empty)", child, location.ParentID)
	}
}

func runLogicalParentOriginListing(t *testing.T, ctx context.Context, db *Store, fixture logicalParentFixture, tc logicalParentCase, child schema.SessionID) {
	t.Helper()
	target := logicalParentTokenID(t, fixture, tc.LatestAttemptEvidence)
	if target == nil {
		t.Fatalf("case %q names no evidence target", tc.Name)
	}
	insertLogicalParentSession(t, ctx, db, *target)
	insertLogicalParentSession(t, ctx, db, child)
	activateLogicalParentEvidence(t, db, child, fixture.LatestAttemptGeneration, logicalParentEvidenceTargetKnown, target)
	arrangeLogicalParentPointers(t, db, child, nil, &fixture.LatestAttemptGeneration, nil)

	rows, err := db.ListStaleOriginSessions(ctx, 1)
	if err != nil {
		t.Fatalf("ListStaleOriginSessions: %v", err)
	}
	var listed *ingest.StoredOriginRow
	for i := range rows {
		if string(rows[i].SessionID) == string(child) {
			listed = &rows[i]
		}
	}
	if listed == nil {
		t.Fatalf("ListStaleOriginSessions did not list session %s", child)
	}
	if listed.ParentID != string(*target) {
		t.Fatalf("origin listing parent of %s = %q, want the evidence target %q while the cache stays NULL", child, listed.ParentID, string(*target))
	}
	assertLogicalParentCache(t, ctx, db, child, nil)
}

// seedLogicalParentPublicationSession stores one publication-ready legacy
// capture through the production writers, so the publication read reaches its
// composition instead of refusing an unready session.
func seedLogicalParentPublicationSession(t *testing.T, ctx context.Context, db *Store, child schema.SessionID) {
	t.Helper()
	meta := planLibraryMeta(ingest.SessionID(child), ingest.HarnessCodex, nil)
	entries := logicalParentPublicationEntries(child)
	content, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("encode logical parent publication entries: %v", err)
	}
	meta.ContentHash = schema.ComputeTranscriptHash(content)
	meta.MetadataHash = schema.ComputeMetadataHash(&meta)
	revisions, err := db.InsertSessionsWithRevisions(ctx, []ingest.StoreEntry{{
		Metadata:           &meta,
		PublicationCapture: true,
		CWDProvenance:      ingest.CWDSourceAbsent,
	}})
	if err != nil {
		t.Fatalf("insert logical parent publication session %s: %v", child, err)
	}
	versions, registered := ingest.HarvesterVersionRegistry[ingest.HarnessCodex]
	if !registered {
		t.Fatal("the logical parent publication fixture needs a registered codex harness")
	}
	results := db.IndexSessionEntryBatch(ctx, []ingest.SessionEntryWrite{{
		SessionID:          ingest.SessionID(child),
		Result:             indexformat.V1{Entries: entries},
		IndexVersion:       versions.IndexVersion,
		RequireFullContent: true,
		CaptureRevision:    revisions[ingest.SessionID(child)],
		IndexerVersion:     versions.IndexerVersion,
		IndexedAtMs:        meta.Timestamp.Start,
	}})
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("index logical parent publication capture: %+v", results)
	}
}

func logicalParentPublicationEntries(child schema.SessionID) []schema.SessionEntry {
	preview := "synthetic logical parent publication input"
	output := "synthetic logical parent publication output"
	return []schema.SessionEntry{
		{
			SessionID: child, EntryIndex: 0, Harness: schema.HarnessCodex, EntryType: schema.EntryTypeText,
			Role: schema.RoleUser, ContentPreview: &preview, SourceEntryRef: schema.SourceEntryRef("e_lp_publication_0"),
		},
		{
			SessionID: child, EntryIndex: 1, Harness: schema.HarnessCodex, EntryType: schema.EntryTypeToolResult,
			Role: schema.RoleTool, ToolOutput: &output, SourceEntryRef: schema.SourceEntryRef("e_lp_publication_1"),
		},
	}
}

// seedLogicalParentPublicationEvidence stores the started_by evidence row and
// the latest-attempt pointer directly, without activating a generation: the
// publication read must reach its composition with its recorded capture
// agreement intact, and activating a generation would clear it. This is the
// same direct-evidence seeding the generation-scoped document family uses.
func seedLogicalParentPublicationEvidence(t *testing.T, ctx context.Context, db *Store, child schema.SessionID, target *schema.SessionID) {
	t.Helper()
	conn, err := db.pool.Take(ctx)
	if err != nil {
		t.Fatalf("take connection to seed publication evidence for %s: %v", child, err)
	}
	defer db.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref) VALUES (?, ?, 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{
		Args: []any{string(child), logicalParentPublicationEvidenceGeneration, string(*target)},
	}); err != nil {
		t.Fatalf("seed started_by evidence for %s: %v", child, err)
	}
	if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET latest_attempt_generation_id = ?, latest_attempt_indexer_version = 1, latest_attempt_captured_at_ms = 2 WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{logicalParentPublicationEvidenceGeneration, string(child)},
	}); err != nil {
		t.Fatalf("seed latest-attempt pointer for %s: %v", child, err)
	}
}
