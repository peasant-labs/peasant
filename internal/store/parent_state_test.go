package store

import (
	"context"
	"strconv"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// insertParentStateSession stores one synthetic session through the real
// metadata writer. A non-nil logical parent is the V1 ParentUUID evidence the
// writer turns into the availability cache when the parent is stored; it is the
// production cache-fallback path.
func insertParentStateSession(t *testing.T, db *Store, id schema.SessionID, logicalParent *ingest.SessionID) {
	t.Helper()
	meta := planLibraryMeta(ingest.SessionID(id), ingest.HarnessCodex, logicalParent)
	if err := db.InsertSessions(context.Background(), []ingest.StoreEntry{{
		Metadata:           &meta,
		PublicationCapture: true,
		CWDProvenance:      ingest.CWDSourceAbsent,
	}}); err != nil {
		t.Fatalf("insert parent-state session %s: %v", id, err)
	}
}

// execParentStateSQL runs one arrangement statement through the store pool.
func execParentStateSQL(t *testing.T, db *Store, query string, args ...any) {
	t.Helper()
	conn, err := db.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer db.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("exec parent-state arrangement: %v", err)
	}
}

// TestParentStateDriftAgainstLogicalParentRule stores every fixture state
// through the production writers and holds the stored projection to the live
// parent rule: a row is a root exactly when the rule names no child lane, is
// resolved exactly when the rule names a parent and is not unresolved, and
// carries the logical parent scalar only in the resolved state.
func TestParentStateDriftAgainstLogicalParentRule(t *testing.T) {
	t.Parallel()
	fixture := loadParentStateFixture(t)
	target, err := schema.NewSessionID(fixture.Target)
	if err != nil {
		t.Fatalf("fixture target: %v", err)
	}
	unstored, err := schema.NewSessionID(fixture.Unstored)
	if err != nil {
		t.Fatalf("fixture unstored parent: %v", err)
	}
	snapshotParent, err := schema.NewSessionID(fixture.Snapshot)
	if err != nil {
		t.Fatalf("fixture snapshot parent: %v", err)
	}
	db, _ := openGenerationStore(t)
	insertParentStateSession(t, db, target, nil)
	for _, tc := range fixture.Cases {
		child, err := schema.NewSessionID(tc.Child)
		if err != nil {
			t.Fatalf("case %q child: %v", tc.Name, err)
		}
		var snapshot *ingest.SessionID
		switch tc.Snapshot {
		case "":
		case "snapshot":
			value := ingest.SessionID(snapshotParent)
			snapshot = &value
		default:
			t.Fatalf("case %q names the unknown snapshot token %q", tc.Name, tc.Snapshot)
		}
		insertParentStateSession(t, db, child, snapshot)
		if tc.CacheParent == "target" {
			execParentStateSQL(t, db, `UPDATE sessions SET parent_id = ? WHERE session_id = ?`, string(target), string(child))
		}
		if tc.InactiveEvidence {
			execParentStateSQL(t, db, `UPDATE sessions SET active_generation_id = 'active' WHERE session_id = ?`, string(child))
			execParentStateSQL(t, db, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES(?, 'old-one', 0, 'started_by', 'explicit_none', NULL, 'native_typed', '', '', ''),
       (?, 'old-two', 0, 'started_by', 'target_known', ?, 'native_typed', '', '', '')`, string(child), string(child), string(target))
		} else if tc.EvidenceState != "" {
			kind := tc.EvidenceKind
			if kind == "" {
				kind = "started_by"
			}
			if kind == "context_from" {
				execParentStateSQL(t, db, `UPDATE sessions SET active_generation_id = 'gen-context' WHERE session_id = ?`, string(child))
				execParentStateSQL(t, db, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES(?, 'gen-context', 0, 'context_from', ?, ?, 'native_typed', '', '', '')`, string(child), tc.EvidenceState, string(target))
			} else {
				var evidenceTarget *schema.SessionID
				switch tc.EvidenceTarget {
				case "target":
					evidenceTarget = &target
				case "unstored":
					evidenceTarget = &unstored
				}
				activateLogicalParentEvidence(t, db, child, "gen-parent-state-"+tc.Name, tc.EvidenceState, evidenceTarget)
			}
		}
		state, resolved := parentStateForSession(t, db, string(child))
		if state != tc.WantState {
			t.Errorf("%s: parent_state = %q, want %q", tc.Name, state, tc.WantState)
		}
		wantResolved := ""
		switch tc.WantResolvedParent {
		case "target":
			wantResolved = fixture.Target
		case "unstored":
			wantResolved = fixture.Unstored
		case "snapshot":
			wantResolved = fixture.Snapshot
		}
		if wantResolved == "" {
			if resolved != nil {
				t.Errorf("%s: resolved_parent_id = %q, want NULL", tc.Name, *resolved)
			}
		} else if resolved == nil || *resolved != wantResolved {
			t.Errorf("%s: resolved_parent_id = %v, want %q", tc.Name, resolved, wantResolved)
		}
	}
	mismatches := parentStateDriftCount(t, db)
	if mismatches != 0 {
		t.Fatalf("%d stored session(s) disagree with the live parent rule; the trigger projection and the renderer have drifted", mismatches)
	}
	// The page's membership (parent_state) and the row's parent markers
	// (SyncParentIdentities) must ship ONE answer for every stored child.
	sessions, err := db.AllPushableSessions(context.Background())
	if err != nil {
		t.Fatalf("read pushable sessions: %v", err)
	}
	identities, err := db.SyncParentIdentities(context.Background(), sessions)
	if err != nil {
		t.Fatalf("read parent identities: %v", err)
	}
	identityByID := make(map[string]ParentIdentityRow, len(identities))
	for _, identity := range identities {
		identityByID[identity.SessionID] = identity
	}
	for _, tc := range fixture.Cases {
		state, resolved := parentStateForSession(t, db, tc.Child)
		identity, known := identityByID[tc.Child]
		if !known {
			t.Errorf("%s: SyncParentIdentities has no row for the stored child", tc.Name)
			continue
		}
		rowParent := ""
		if resolved != nil {
			rowParent = *resolved
		}
		if identity.ParentIdentity != rowParent {
			t.Errorf("%s: row parent = %q, projection resolved_parent_id = %q; the page and the row disagree", tc.Name, identity.ParentIdentity, rowParent)
		}
		if identity.ParentUnresolved != (state == string(ParentStateUnresolved)) {
			t.Errorf("%s: row parentUnresolved = %t, projection state = %q; the page and the row disagree", tc.Name, identity.ParentUnresolved, state)
		}
	}
}

// parentStateDriftCount counts rows whose stored projection disagrees with the
// live LogicalParentSQL/ParentUnresolvedSQL rule in any of the four ways the
// projection defines.
func parentStateDriftCount(t *testing.T, db *Store) int64 {
	t.Helper()
	conn, err := db.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer db.pool.Put(conn)
	query := `SELECT COUNT(*) FROM sessions s
 WHERE (s.parent_state = 'root') <> (NOT (` + ParentUnresolvedSQL("s") + `) AND ` + LogicalParentSQL("s") + ` IS NULL)
    OR (s.parent_state = 'resolved') <> (NOT (` + ParentUnresolvedSQL("s") + `) AND ` + LogicalParentSQL("s") + ` IS NOT NULL)
    OR (s.parent_state = 'resolved' AND s.resolved_parent_id IS NOT ` + LogicalParentSQL("s") + `)
    OR (s.parent_state <> 'resolved' AND s.resolved_parent_id IS NOT NULL)`
	var count int64
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count parent-state drift: %v", err)
	}
	return count
}

func parentStateForSession(t *testing.T, db *Store, sessionID string) (string, *string) {
	t.Helper()
	conn, err := db.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer db.pool.Put(conn)
	return readParentState(t, conn, sessionID)
}

// TestParentStateTriggersBumpRevisionOncePerWrite proves the trigger contract
// the page caches depend on: one page-relevant write advances the one-row
// revision by exactly one, whether the write is a session insert, a session
// update, or an evidence-only insert/update/delete. It also proves the
// connection keeps SQLite's recursive triggers off, which is what stops a
// trigger's own projection UPDATE from re-entering.
func TestParentStateTriggersBumpRevisionOncePerWrite(t *testing.T) {
	t.Parallel()
	db, _ := openGenerationStore(t)
	sid := "b7b7b7b7-b7b7-47b7-87b7-b7b7b7b7b7b7"
	seedGenerationSession(t, db, sid)
	conn, err := db.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer db.pool.Put(conn)
	var recursive int64 = -1
	if err := sqlitex.ExecuteTransient(conn, `PRAGMA recursive_triggers`, &sqlitex.ExecOptions{
		ResultFunc: func(stmt *sqlite.Stmt) error {
			recursive = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read recursive_triggers: %v", err)
	}
	if recursive != 0 {
		t.Fatalf("recursive_triggers = %d, want 0; a trigger's projection UPDATE must not re-enter", recursive)
	}
	revision := func() int64 {
		t.Helper()
		value, err := db.Revision(context.Background())
		if err != nil {
			t.Fatalf("read revision: %v", err)
		}
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			t.Fatalf("revision %q is not a decimal integer: %v", value, err)
		}
		return parsed
	}
	bumps := func(name string, write func()) {
		t.Helper()
		before := revision()
		write()
		if after := revision(); after != before+1 {
			t.Fatalf("%s advanced the revision from %d to %d, want exactly %d", name, before, after, before+1)
		}
	}
	newSessionID := "c8c8c8c8-c8c8-48c8-88c8-c8c8c8c8c8c8"
	bumps("a session insert", func() {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code','state-model','host-gen','proj-gen',1,2,3,'/synthetic/new.jsonl','jsonl')`, &sqlitex.ExecOptions{Args: []any{newSessionID}}); err != nil {
			t.Fatalf("insert session: %v", err)
		}
	})
	bumps("a session update", func() {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE sessions SET tags = 'tagged' WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{sid}}); err != nil {
			t.Fatalf("update session: %v", err)
		}
	})
	bumps("an evidence insert", func() {
		if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_relationship_evidence(session_id, generation_id, ordinal, kind, target_state, target_local_id, evidence, anchor_kind, anchor_source_entry_ref, anchor_source_revision_ref)
VALUES(?, 'gen-revision', 0, 'started_by', 'unknown', NULL, 'native_typed', '', '', '')`, &sqlitex.ExecOptions{Args: []any{sid}}); err != nil {
			t.Fatalf("insert evidence: %v", err)
		}
	})
	bumps("an evidence update", func() {
		if err := sqlitex.ExecuteTransient(conn, `UPDATE session_relationship_evidence SET target_state = 'target_known', target_local_id = '00000000-0000-4000-8000-000000000001' WHERE session_id = ? AND generation_id = 'gen-revision'`, &sqlitex.ExecOptions{Args: []any{sid}}); err != nil {
			t.Fatalf("update evidence: %v", err)
		}
	})
	bumps("an evidence delete", func() {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_relationship_evidence WHERE session_id = ? AND generation_id = 'gen-revision'`, &sqlitex.ExecOptions{Args: []any{sid}}); err != nil {
			t.Fatalf("delete evidence: %v", err)
		}
	})
	prunedRoot := "d1d1d1d1-d1d1-41d1-81d1-d1d1d1d1d1d1"
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format)
VALUES(?, 'claude-code','state-model','host-gen','proj-gen',1,2,3,'/synthetic/pruned-root.jsonl','jsonl')`, &sqlitex.ExecOptions{Args: []any{prunedRoot}}); err != nil {
		t.Fatalf("insert a session to prune: %v", err)
	}
	bumps("a pruned evidence-less root", func() {
		if err := sqlitex.ExecuteTransient(conn, `DELETE FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{Args: []any{prunedRoot}}); err != nil {
			t.Fatalf("delete the evidence-less root: %v", err)
		}
	})
	if state, resolved := parentStateForSession(t, db, sid); state != string(ParentStateRoot) || resolved != nil {
		t.Fatalf("after the evidence deletion the parent lane = %q/%v, want root/NULL", state, resolved)
	}
}

// TestParentStateEvidenceOnlyWritesLandUnresolved drives the two evidence-only
// write paths: a latest-attempt generation installed without a parent-cache
// write, and an active generation installed through the managed index writer.
// Both write session_relationship_evidence and never the cache, so only the
// triggers can put the child in the unresolved lane. The latest-attempt arm
// also proves it is absent from the root set and that the revision advanced.
func TestParentStateEvidenceOnlyWritesLandUnresolved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	t.Run("latest attempt", func(t *testing.T) {
		db, _ := openGenerationStore(t)
		child, err := schema.NewSessionID("d9d9d9d9-d9d9-49d9-89d9-d9d9d9d9d9d9")
		if err != nil {
			t.Fatalf("child identifier: %v", err)
		}
		seedLogicalParentPublicationSession(t, ctx, db, child)
		setStoredArtifactHash(t, db, child, hexIdentity('e'))
		before, err := db.Revision(ctx)
		if err != nil {
			t.Fatalf("read the revision before the attempt: %v", err)
		}
		candidate, blobs := authorityGeneration(child, "gen_evidence_only", []schema.SessionRelationship{{
			Kind:        schema.SessionRelationshipStartedBy,
			TargetState: schema.RelationshipTargetUnknown,
			Evidence:    schema.EvidenceNativeTyped,
		}})
		versions, registered := ingest.HarvesterVersionRegistry[ingest.HarnessCodex]
		if !registered {
			t.Fatal("the evidence-only fixture needs a registered codex harness")
		}
		outcome, err := db.ActivateGeneration(ctx, GenerationActivation{
			Generation:     filledCandidateForValidation(t, candidate, blobs),
			Blobs:          blobs,
			IndexerVersion: versions.IndexerVersion,
			IndexedAtMs:    2,
			ContentCapture: sourceOmittedPreviewWrite(),
		})
		if err != nil {
			t.Fatalf("retain the evidence-only latest attempt: %v", err)
		}
		if outcome.Disposition != ingest.ActivationLatestAttemptPreview {
			t.Fatalf("disposition = %v, want ActivationLatestAttemptPreview; the test must exercise the latest-attempt write path", outcome.Disposition)
		}
		assertEvidenceOnlyChild(t, db, child)
		after, err := db.Revision(ctx)
		if err != nil {
			t.Fatalf("read the revision after the attempt: %v", err)
		}
		if after == before {
			t.Fatalf("revision stayed %q across the evidence-only latest attempt; the page cache would serve a stale page", before)
		}
	})
	t.Run("active generation", func(t *testing.T) {
		db, _ := openGenerationStore(t)
		child, err := schema.NewSessionID("e1e1e1e1-e1e1-41e1-81e1-e1e1e1e1e1e1")
		if err != nil {
			t.Fatalf("child identifier: %v", err)
		}
		insertParentStateSession(t, db, child, nil)
		activateLogicalParentEvidence(t, db, child, "gen_evidence_active", "unknown", nil)
		assertEvidenceOnlyChild(t, db, child)
	})
}

func assertEvidenceOnlyChild(t *testing.T, db *Store, child schema.SessionID) {
	t.Helper()
	state, resolved := parentStateForSession(t, db, string(child))
	if state != string(ParentStateUnresolved) {
		t.Fatalf("parent_state = %q, want unresolved for an evidence-only child", state)
	}
	if resolved != nil {
		t.Fatalf("resolved_parent_id = %q, want NULL for an unresolved child", *resolved)
	}
	conn, err := db.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer db.pool.Put(conn)
	var roots int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM sessions WHERE session_id = ? AND parent_state = 'root'`, &sqlitex.ExecOptions{
		Args: []any{string(child)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			roots = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("count the child in the root set: %v", err)
	}
	if roots != 0 {
		t.Fatalf("the evidence-only child appears in the root set; the Home page would show it")
	}
}
