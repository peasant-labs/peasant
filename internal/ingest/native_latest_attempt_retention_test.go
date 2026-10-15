package ingest_test

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

// The read authority a latest-attempt retention case starts from. A managed
// authority is a complete V2 generation installed as the active generation; a
// certificate authority is the stored V1 projection with its full capture and
// no active generation; the managed-legacy-null shape is a managed authority
// whose row predates the artifact-hash column, so the verified pair identity is
// supplied by the write rather than the row.
const (
	latestAttemptAuthorityManaged           = "managed"
	latestAttemptAuthorityCertificate       = "certificate"
	latestAttemptAuthorityManagedLegacyNull = "managed_legacy_null"
)

// latestAttemptHeldReason is the substring every retention case asserts in the
// durable held reason. It is the production reason's own opening clause, so a
// reason that stops naming the latest attempt fails instead of drifting.
const latestAttemptHeldReason = "latest attempt kept as an incomplete preview"

// latestAttemptFlagWarning is the stored metadata warning that makes a refresh's
// capture the assessed unaccounted-SourceOmitted preview: the stored pair
// records that a source record was left out, and the stored transcript carries
// no positional stand-in for it, so the output cannot account for the omission.
func latestAttemptFlagWarning() schema.DiagnosticEntry {
	return schema.DiagnosticEntry{
		ErrorType: "record_too_large",
		Location:  "line 4",
		Message:   "a source record over the size limit was left out before the stored transcript was written",
	}
}

// latestAttemptAuthorityState is the comparable read authority a retention case
// snapshots before and after the harvest, so A's byte-identity is asserted
// rather than assumed.
type latestAttemptAuthorityState struct {
	format       int
	entries      []schema.SessionEntry
	certificate  latestAttemptCertificate
	stamps       storedStamps
	generationID string
	indexVersion int
}

// latestAttemptCertificate is the stored authority certificate read directly from
// its own row. The display-effective capture read resolves the latest attempt
// after the attempt was kept, so the byte-identity assertion reads the row the authority
// owns instead.
type latestAttemptCertificate struct {
	status         string
	captureFormat  string
	failureCode    string
	failureMessage string
	entryCount     int
	capturedAtMs   int64
}

// readAuthorityCertificate reads the authority's own capture row.
func readAuthorityCertificate(t *testing.T, db *store.Store, sid ingest.SessionID) latestAttemptCertificate {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var certificate latestAttemptCertificate
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT status, capture_format, COALESCE(failure_code, ''), COALESCE(failure_message, ''), COALESCE(entry_count, 0), COALESCE(captured_at_ms, 0) FROM session_content_captures WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			certificate.status = stmt.ColumnText(0)
			certificate.captureFormat = stmt.ColumnText(1)
			certificate.failureCode = stmt.ColumnText(2)
			certificate.failureMessage = stmt.ColumnText(3)
			certificate.entryCount = stmt.ColumnInt(4)
			certificate.capturedAtMs = stmt.ColumnInt64(5)
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("read the authority certificate: %v", err)
	}
	if !found {
		t.Fatalf("session %s has no stored authority certificate", sid)
	}
	return certificate
}

// runLatestAttemptCase drives the REAL harvest over a real store holding a
// full read authority whose stored pair records an unaccounted source omission.
// The refresh's candidate is the assessed unaccounted-SourceOmitted preview, so
// the harvest must retain it as the latest attempt, leave the authority
// byte-identical, and report the held outcome with literal zero counts.
func runLatestAttemptCase(t *testing.T, tc nativeRefreshRepairCase) {
	t.Helper()
	sid, err := ingest.NewSessionID(tc.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	root := filepath.Join(dir, "artifacts")
	dbPath := storetest.CopyGoldenDB(t)
	db := nativeRepairStore(t, dbPath, root)
	defer func() { _ = db.Close() }()
	fs := testutil.NewCountingFS(testutil.NewMemFS())
	const outputDir = "/output"

	meta := nativeRepairMeta(t, tc.SessionID, tc.Harness, "/native/"+tc.SessionID+".jsonl")
	meta.Diagnostics.Warnings = append(meta.Diagnostics.Warnings, latestAttemptFlagWarning())
	storetest.SeedManagedInput(t, db, fs, outputDir, *meta, []byte(tc.Source))

	switch tc.Authority {
	case latestAttemptAuthorityManaged, latestAttemptAuthorityManagedLegacyNull:
		installLatestAttemptAuthority(t, db, sid, tc.Harness)
	case latestAttemptAuthorityCertificate:
		// No active managed generation: the stored V1 projection is the authority.
	default:
		t.Fatalf("latest-attempt retention case %q declares unknown authority %q", tc.Name, tc.Authority)
	}
	if tc.Authority == latestAttemptAuthorityManagedLegacyNull {
		execBridgeSQL(t, db, `UPDATE sessions SET artifact_hash = NULL WHERE session_id = ?`, string(sid))
	}
	// The session is already at the adapter target, so the refresh does not
	// restamp it; the authority's own adapter stamp stays put.
	setSessionAdapterVersion(t, db, sid, ingest.GenerationRepairTargets[tc.Harness].AdapterVersion)
	// The stored producer revision is behind this build's managed target, so the
	// ordinary harvest has real work and refreshes the session from its stored
	// pair. The representation the authority installed is left untouched.
	execBridgeSQL(t, db, `UPDATE sessions SET index_version = ? WHERE session_id = ?`, tc.StoredIndexerVersion, string(sid))

	before := readLatestAttemptAuthority(t, db, sid)
	transcriptPath := storedTranscriptPath(outputDir, sid)

	result := runNativeRepairPipeline(t, db, fs, outputDir)
	assertLatestAttemptRetained(t, result, db, sid, before, tc.Authority)

	if tc.RetentionRepeat {
		fs.ResetCounts()
		repeat := runNativeRepairPipeline(t, db, fs, outputDir)
		if repeat.Summary.Indexed != 0 {
			t.Fatalf("the repeated harvest indexed %d sessions, want 0", repeat.Summary.Indexed)
		}
		if reads := fs.ReadCount(transcriptPath); reads != 0 {
			t.Fatalf("the repeated harvest read the stored transcript %d time(s), want 0", reads)
		}
		if after := readLatestAttemptAuthority(t, db, sid); !reflect.DeepEqual(before, after) {
			t.Fatalf("the repeated harvest moved the read authority:\nbefore=%+v\nafter=%+v", before, after)
		}
	}

	if tc.ExplicitRebuild {
		forced := runNativeRepairPipeline(t, db, fs, outputDir, func(cfg *ingest.PipelineConfig) { cfg.Reindex = true })
		if forced.Summary.Indexed == 0 {
			t.Fatalf("the explicit rebuild reported no indexed session: %+v", forced.IndexLog)
		}
		if pointer := readLatestAttemptPointer(t, db, sid); pointer.generationID != "" {
			t.Fatalf("the explicit rebuild left the latest-attempt pointer at %q, want it cleared", pointer.generationID)
		}
		callbackRan := false
		err := db.WithSessionSnapshotSelected(t.Context(), sid, ingest.SessionReadSelection{LastGood: true}, func(_ indexformat.ReadSnapshot) error {
			callbackRan = true
			return nil
		})
		if !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
			t.Fatalf("last-good after the explicit rebuild = %v, want the active-preview refusal", err)
		}
		if callbackRan {
			t.Fatal("last-good served the active preview installed by the explicit rebuild")
		}
	}
}

// installLatestAttemptAuthority installs a complete V2 generation as the active
// read authority through the store's real activation, exactly as a completed
// harvest leaves one.
func installLatestAttemptAuthority(t *testing.T, db *store.Store, sid ingest.SessionID, harness ingest.Harness) {
	t.Helper()
	authority, blobs := buildSettlingGeneration(t, schema.SessionID(sid), "gen_authority_a", "authority text", "authority input", "authority output")
	assessment, err := ingest.AssessCapture(ingest.CaptureFacts{
		Harness: harness, Result: authority, Policy: ingest.CaptureFreshCandidate, Authoritative: true,
	})
	if err != nil {
		t.Fatalf("assess the read authority: %v", err)
	}
	capture, err := assessment.ContentCapture(ingest.ContentSourceNewIngest, ingest.TranscriptOriginFile, 1)
	if err != nil {
		t.Fatalf("build the read authority's capture: %v", err)
	}
	outcome, err := db.ActivateGeneration(t.Context(), store.GenerationActivation{
		Generation: authority, Blobs: blobs, IndexerVersion: 19, IndexedAtMs: 1, ContentCapture: capture,
	})
	if err != nil {
		t.Fatalf("install the read authority: %v", err)
	}
	if outcome.Disposition != ingest.ActivationCommittedNow {
		t.Fatalf("read authority disposition = %v, want ActivationCommittedNow", outcome.Disposition)
	}
}

// readLatestAttemptAuthority snapshots the parts of the read authority a retention
// case must find unchanged: the stored representation, its canonical entries,
// its certificate, its producer/adapter/format stamps, and the generation the
// explicit last-good read resolves.
func readLatestAttemptAuthority(t *testing.T, db *store.Store, sid ingest.SessionID) latestAttemptAuthorityState {
	t.Helper()
	entries, err := db.ListEntries(t.Context(), sid)
	if err != nil {
		t.Fatalf("read the authority entries: %v", err)
	}
	generationID := ""
	if err := db.WithSessionSnapshotSelected(t.Context(), sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		generationID = snapshot.GenerationID
		return nil
	}); err != nil {
		t.Fatalf("read the authority snapshot: %v", err)
	}
	return latestAttemptAuthorityState{
		format:       rawIndexFormat(t, db, sid),
		entries:      entries,
		certificate:  readAuthorityCertificate(t, db, sid),
		stamps:       readStoredStamps(t, db, sid),
		generationID: generationID,
		indexVersion: readIndexVersion(t, db, sid),
	}
}

// assertLatestAttemptRetained asserts the literal retention observables: the pipeline
// summary and the durable log carry zero indexed counts with the held reason,
// the authority is byte-identical, the latest attempt is readable as a bounded
// partial preview, the default full reads refuse, and the explicit last-good
// read still serves the authority.
func assertLatestAttemptRetained(t *testing.T, result *ingest.PipelineResult, db *store.Store, sid ingest.SessionID, before latestAttemptAuthorityState, authorityKind string) {
	t.Helper()
	if result.Summary.Indexed != 0 {
		t.Fatalf("retention indexed %d sessions, want 0: %+v", result.Summary.Indexed, result.IndexLog)
	}
	entry := latestAttemptLogEntryFor(t, result, sid)
	if entry.Outcome != ingest.IndexOutcomeSkipped {
		t.Fatalf("retention outcome = %q, want skipped", entry.Outcome)
	}
	if entry.EntriesCount != 0 {
		t.Fatalf("retention reported %d entries, want the literal 0", entry.EntriesCount)
	}
	if entry.Reason == nil || !strings.Contains(*entry.Reason, latestAttemptHeldReason) {
		t.Fatalf("retention reason = %v, want it to name the latest attempt", entry.Reason)
	}

	durable := readDurableIndexLog(t, db, sid)
	if durable.outcome != "skipped" || durable.entriesCount != 0 || !strings.Contains(durable.reason, latestAttemptHeldReason) {
		t.Fatalf("durable index_log = %+v, want skipped with zero entries and the held reason", durable)
	}

	after := readLatestAttemptAuthority(t, db, sid)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the write changed the read authority:\nbefore=%+v\nafter=%+v", before, after)
	}

	available, err := db.ReadSessionAvailable(t.Context(), sid)
	if err != nil {
		t.Fatalf("read the retained available snapshot: %v", err)
	}
	if !store.PartialPreviewNeeded(available.Capture) {
		t.Fatalf("the latest attempt available capture = %+v, want the partial preview state", available.Capture)
	}
	if available.Capture.FailureCode != ingest.ContentCaptureSourceRecordsOmitted {
		t.Fatalf("the latest attempt available failure code = %q, want source_records_omitted", available.Capture.FailureCode)
	}
	if len(available.Entries) == 0 {
		t.Fatal("the latest attempt serves no entries")
	}

	callbackRan := false
	if err := db.WithSessionSnapshot(t.Context(), sid, func(_ indexformat.ReadSnapshot) error {
		callbackRan = true
		return nil
	}); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("default full read = %v, want the latest-attempt sentinel", err)
	}
	if callbackRan {
		t.Fatal("the default full read ran the callback for a session with a latest attempt")
	}
	if _, err := db.ReadSessionContent(t.Context(), string(sid)); !errors.Is(err, indexformat.ErrLatestAttemptIncomplete) {
		t.Fatalf("default content read = %v, want the latest-attempt sentinel", err)
	}

	lastGood := latestAttemptAuthorityState{}
	if err := db.WithSessionSnapshotSelected(t.Context(), sid, ingest.SessionReadSelection{LastGood: true}, func(snapshot indexformat.ReadSnapshot) error {
		lastGood.generationID = snapshot.GenerationID
		lastGood.indexVersion = snapshot.IndexVersion
		return nil
	}); err != nil {
		t.Fatalf("explicit last-good read after the attempt was kept: %v", err)
	}
	if lastGood.generationID != before.generationID || lastGood.indexVersion != before.indexVersion {
		t.Fatalf("last-good served generation %q/index %d, want the authority's %q/%d", lastGood.generationID, lastGood.indexVersion, before.generationID, before.indexVersion)
	}

	pointer := readLatestAttemptPointer(t, db, sid)
	if pointer.generationID == "" {
		t.Fatal("retention recorded no latest-attempt generation")
	}
	if pointer.artifactHash == "" {
		t.Fatal("retention recorded no latest-attempt pair identity")
	}
	if authorityKind == latestAttemptAuthorityManagedLegacyNull {
		// The write supplied the verified pair identity, so the bootstrap
		// establishes the row's artifact hash and the pointer records it.
		storedHash := readSessionArtifactHash(t, db, sid)
		if storedHash == "" {
			t.Fatal("retention did not bootstrap the legacy row's artifact hash")
		}
		if storedHash != pointer.artifactHash {
			t.Fatalf("bootstrapped artifact hash %q disagrees with the recorded latest-attempt identity %q", storedHash, pointer.artifactHash)
		}
	}
}

// latestAttemptLogEntryFor returns the pipeline log entry for one session.
func latestAttemptLogEntryFor(t *testing.T, result *ingest.PipelineResult, sid ingest.SessionID) ingest.IndexLogEntry {
	t.Helper()
	for _, entry := range result.IndexLog {
		if entry.SessionID == sid {
			return entry
		}
	}
	t.Fatalf("pipeline reported no log entry for session %s: %+v", sid, result.IndexLog)
	return ingest.IndexLogEntry{}
}

// durableIndexLogRow is the persisted index_log row a retention case reads back
// through SQL, the durability point the pipeline summary is not.
type durableIndexLogRow struct {
	outcome      string
	entriesCount int
	reason       string
}

// readDurableIndexLog reads the newest persisted index_log row for one session.
func readDurableIndexLog(t *testing.T, db *store.Store, sid ingest.SessionID) durableIndexLogRow {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var row durableIndexLogRow
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT outcome, entries_count, COALESCE(reason, '') FROM index_log WHERE session_id = ? ORDER BY id DESC LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			row.outcome = stmt.ColumnText(0)
			row.entriesCount = stmt.ColumnInt(1)
			row.reason = stmt.ColumnText(2)
			found = true
			return nil
		},
	}); err != nil {
		t.Fatalf("read the durable index_log: %v", err)
	}
	if !found {
		t.Fatalf("no durable index_log row for session %s", sid)
	}
	return row
}

// latestAttemptPointer is the persisted latest-attempt pointer a retention case
// reads back directly.
type latestAttemptPointer struct {
	generationID string
	artifactHash string
}

// readLatestAttemptPointer reads the latest-attempt pointer columns.
func readLatestAttemptPointer(t *testing.T, db *store.Store, sid ingest.SessionID) latestAttemptPointer {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	var pointer latestAttemptPointer
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(latest_attempt_generation_id, ''), COALESCE(latest_attempt_artifact_hash, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			pointer.generationID = stmt.ColumnText(0)
			pointer.artifactHash = stmt.ColumnText(1)
			return nil
		},
	}); err != nil {
		t.Fatalf("read the latest-attempt pointer: %v", err)
	}
	return pointer
}

// readSessionArtifactHash reads the session's own stored pair identity.
func readSessionArtifactHash(t *testing.T, db *store.Store, sid ingest.SessionID) string {
	t.Helper()
	conn, err := db.Pool().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Pool().Put(conn)
	hash := ""
	if err := sqlitex.ExecuteTransient(conn, `SELECT COALESCE(artifact_hash, '') FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			hash = stmt.ColumnText(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("read the session artifact hash: %v", err)
	}
	return hash
}

// storedTranscriptPath is the saved transcript path a harvest reads when it
// refreshes a managed session.
func storedTranscriptPath(outputDir string, sid ingest.SessionID) string {
	metadataPath := ingest.SessionMetadataPath(outputDir, testutil.TestHostSlug, string(sid), "")
	return filepath.Join(filepath.Dir(metadataPath), string(sid)+"--transcript.jsonl")
}
