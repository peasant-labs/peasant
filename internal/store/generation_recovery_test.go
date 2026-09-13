package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
	"zombiezen.com/go/sqlite/sqlitex"
)

//go:embed testdata/projection_commit_recovery.yaml
var projectionRecoveryYAML []byte

//go:embed testdata/projection_commit_recovery.manifest.yaml
var projectionRecoveryManifestYAML []byte

type projectionRecoveryFixture struct {
	Session struct {
		ID      string `yaml:"id"`
		Harness string `yaml:"harness"`
	} `yaml:"session"`
	Generation struct {
		CompleteID      string `yaml:"complete_id"`
		FailedID        string `yaml:"failed_id"`
		LongTextPadding int    `yaml:"long_text_padding"`
	} `yaml:"generation"`
	Cases []struct {
		Name     string `yaml:"name"`
		Seam     string `yaml:"seam"`
		Visible  string `yaml:"visible"`
		Recovery string `yaml:"recovery"`
	} `yaml:"cases"`
	CorruptArtifact struct {
		Name     string `yaml:"name"`
		EntryRef string `yaml:"entry_ref"`
	} `yaml:"corrupt_artifact"`
}

func loadProjectionRecoveryFixture(t *testing.T) projectionRecoveryFixture {
	t.Helper()
	var fixture projectionRecoveryFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(projectionRecoveryYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode projection_commit_recovery.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(projectionRecoveryManifestYAML)
	if err != nil {
		t.Fatalf("decode projection_commit_recovery manifest: %v", err)
	}
	actual := make([]string, 0, len(fixture.Cases)+1)
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
	}
	if fixture.CorruptArtifact.Name != "" {
		actual = append(actual, fixture.CorruptArtifact.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "projection commit recovery"); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// decodeRecoveryRequiredNames strictly decodes the name-only manifest. The
// store package cannot import internal/testutil (it imports store), so the same
// exact-set rule is restated here over the same YAML shape.
func decodeRecoveryRequiredNames(data []byte) ([]string, error) {
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&manifest); err != nil {
		return nil, err
	}
	if len(manifest.RequiredNames) == 0 {
		return nil, errors.New("required-names manifest declares no requiredNames")
	}
	return manifest.RequiredNames, nil
}

func validateRecoveryRequiredNames(required, actual []string, label string) error {
	seenActual := make(map[string]struct{}, len(actual))
	for _, name := range actual {
		if _, duplicate := seenActual[name]; duplicate {
			return fmt.Errorf("%s fixture repeats case name %q", label, name)
		}
		seenActual[name] = struct{}{}
	}
	seenRequired := make(map[string]struct{}, len(required))
	for _, name := range required {
		seenRequired[name] = struct{}{}
	}
	for _, name := range required {
		if _, found := seenActual[name]; !found {
			return fmt.Errorf("%s fixture is missing required case %q", label, name)
		}
	}
	for _, name := range actual {
		if _, declared := seenRequired[name]; !declared {
			return fmt.Errorf("%s fixture carries undeclared case %q; add it to the required-names manifest", label, name)
		}
	}
	return nil
}

// openGenerationStore opens a real store with the V2 format, the owned-artifact
// file store and the OS session lock. It returns the store and the artifact root.
func openGenerationStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
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
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

func execGenerationSQL(t *testing.T, s *Store, script string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Pool.Take: %v", err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec store SQL: %v", err)
	}
}

func seedGenerationSession(t *testing.T, s *Store, sid string) {
	t.Helper()
	execGenerationSQL(t, s, `
INSERT OR IGNORE INTO host_slugs(opaque_id, host_slug) VALUES('host-gen','host-gen');
INSERT OR IGNORE INTO projects(project_hash, canonical_cwd, canonical_remote) VALUES('proj-gen','/tmp/gen','github.com/gen/gen');
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version)
VALUES('`+sid+`','claude-code','model-gen','host-gen','proj-gen',1,2,3,'/tmp/gen/source.jsonl','jsonl',11);
`)
}

// generationRefs are the deterministic refs the test generation emits.
var generationRefs = []string{"e_u1", "e_call1", "e_result1"}

// buildTestGeneration builds a valid, self-contained V2 candidate whose full
// text and tool result are longer than the preview limit by more than 1024
// characters. Content records carry only their refs; staging fills the managed
// relative path, byte length and integrity digest.
func buildTestGeneration(t *testing.T, sid schema.SessionID, genID, text, toolInput, toolOutput string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	entries := []schema.SessionEntry{
		{
			SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeText,
			Role: ingest.RoleUser, ContentPreview: &text, SourceEntryRef: schema.SourceEntryRef(generationRefs[0]),
		},
		{
			SessionID: sid, EntryIndex: 1, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolUse,
			Role: ingest.RoleAssistant, ToolInput: &toolInput, SourceEntryRef: schema.SourceEntryRef(generationRefs[1]),
		},
		{
			SessionID: sid, EntryIndex: 2, Harness: defaults.HarnessClaudeCode, EntryType: ingest.EntryTypeToolResult,
			Role: ingest.RoleTool, ToolOutput: &toolOutput, SourceEntryRef: schema.SourceEntryRef(generationRefs[2]),
		},
	}
	inputCount := int64(1)
	generation := indexformat.Generation{
		ID:           genID,
		Completeness: indexformat.GenerationCompletenessComplete,
		Metadata: schema.UnifiedMetadata{
			SchemaVersion: ingest.CurrentSchemaVersion,
			SessionID:     sid,
			ModelHarness:  defaults.HarnessClaudeCode,
			Stats:         schema.SessionStats{TurnCount: len(entries), InputSubmissionCount: &inputCount},
		},
		Main:                 indexformat.Partition{Entries: entries},
		Content:              []indexformat.ContentRecord{{Ref: schema.SourceEntryRef(generationRefs[0])}, {Ref: schema.SourceEntryRef(generationRefs[1])}, {Ref: schema.SourceEntryRef(generationRefs[2])}},
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: schema.SourceEntryRef(generationRefs[0])}},
		SourceEvidenceDigest: strings.Repeat("a", 64),
		TitleRefs:            []schema.SourceEntryRef{schema.SourceEntryRef(generationRefs[0])},
	}
	blobs := map[schema.SourceEntryRef][]byte{
		schema.SourceEntryRef(generationRefs[0]): []byte(text),
		schema.SourceEntryRef(generationRefs[1]): []byte(toolInput),
		schema.SourceEntryRef(generationRefs[2]): []byte(toolOutput),
	}
	return indexformat.V2{Generation: generation}, blobs
}

// filledCandidateForValidation returns the candidate in the shape staging
// produces: every content record carries its owned relative blob path, byte
// length and payload digest. Generation.Validate rejects an unfilled candidate,
// so a test that needs to prove a candidate is otherwise valid must validate
// the staged shape rather than the pre-stage value.
func filledCandidateForValidation(t *testing.T, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) indexformat.V2 {
	t.Helper()
	filled := append([]indexformat.ContentRecord(nil), v2.Generation.Content...)
	for i := range filled {
		payload, ok := blobs[filled[i].Ref]
		if !ok {
			t.Fatalf("candidate content blob for ref %q is missing", filled[i].Ref)
		}
		sum := sha256.Sum256(payload)
		filled[i].RelativeBlob = blobName(filled[i].Ref)
		filled[i].ByteLength = int64(len(payload))
		filled[i].Digest = hex.EncodeToString(sum[:])
	}
	v2.Generation.Content = filled
	return v2
}

type faultArtifacts struct {
	GenerationArtifactStore
	failRepair bool
	failClear  bool
}

func (f faultArtifacts) RepairMetadata(ctx context.Context, id schema.SessionID, metadata []byte) error {
	if f.failRepair {
		return errors.New("injected fault after activation before metadata repair")
	}
	return f.GenerationArtifactStore.RepairMetadata(ctx, id, metadata)
}

func (f faultArtifacts) ClearIntent(ctx context.Context, id schema.SessionID) error {
	if f.failClear {
		return errors.New("injected fault after metadata before intent clear")
	}
	return f.GenerationArtifactStore.ClearIntent(ctx, id)
}

func installRecoveryFault(t *testing.T, s *Store, seam string) {
	t.Helper()
	switch seam {
	case "before-temp-fsync", "after-fsync-before-rename", "after-rename-before-db":
		osStore, ok := s.generationArtifacts.(*osGenerationArtifactStore)
		if !ok {
			t.Fatalf("stage seam requires the production artifact store, got %T", s.generationArtifacts)
		}
		osStore.seam = func(at string) error {
			if at == seam {
				return fmt.Errorf("injected fault at %s", at)
			}
			return nil
		}
	case "during-activation-transaction":
		execGenerationSQL(t, s, `CREATE TRIGGER test_fail_activation BEFORE INSERT ON session_projection_generations BEGIN SELECT RAISE(ABORT, 'injected activation transaction fault'); END;`)
	case "after-db-before-metadata":
		s.generationArtifacts = faultArtifacts{GenerationArtifactStore: s.generationArtifacts, failRepair: true}
	case "after-metadata-before-intent-clear":
		s.generationArtifacts = faultArtifacts{GenerationArtifactStore: s.generationArtifacts, failClear: true}
	default:
		t.Fatalf("unknown recovery seam %q", seam)
	}
}

func clearRecoveryFault(t *testing.T, s *Store, seam string) {
	t.Helper()
	switch seam {
	case "before-temp-fsync", "after-fsync-before-rename", "after-rename-before-db":
		osStore := s.generationArtifacts.(*osGenerationArtifactStore)
		osStore.seam = nil
	case "during-activation-transaction":
		execGenerationSQL(t, s, `DROP TRIGGER IF EXISTS test_fail_activation;`)
	case "after-db-before-metadata", "after-metadata-before-intent-clear":
		s.generationArtifacts = s.generationArtifacts.(faultArtifacts).GenerationArtifactStore
	}
}

// TestSessionSnapshotLegacyControl proves the snapshot API keeps the unchanged
// V1 read: a session with no active generation yields a legacy snapshot that
// names the retained transcript and carries no generation partitions, so the
// caller uses the existing overlay callback and never a V2 path.
func TestSessionSnapshotLegacyControl(t *testing.T) {
	fixture := loadProjectionRecoveryFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, fixture.Session.ID)
	err = s.WithSessionSnapshot(context.Background(), id, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.IndexVersion != 1 {
			return fmt.Errorf("index version %d, want the legacy 1", snapshot.IndexVersion)
		}
		if snapshot.GenerationID != "" || len(snapshot.Main.Entries) != 0 || len(snapshot.Content) != 0 {
			return fmt.Errorf("legacy snapshot carried generation state: %+v", snapshot)
		}
		if snapshot.LegacySource.IsZero() || snapshot.LegacySource.Path == "" {
			return fmt.Errorf("legacy snapshot did not name its retained transcript: %+v", snapshot.LegacySource)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func visibleGeneration(t *testing.T, s *Store, sid schema.SessionID) string {
	t.Helper()
	var generation string
	err := s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.IndexVersion != 2 {
			return fmt.Errorf("snapshot index version %d, want 2", snapshot.IndexVersion)
		}
		generation = snapshot.GenerationID
		return nil
	})
	if err != nil {
		t.Fatalf("WithSessionSnapshot: %v", err)
	}
	return generation
}

func activateTestGeneration(t *testing.T, s *Store, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) error {
	t.Helper()
	return s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
}

// TestProjectionCommitRecovery drives the real activation through all six
// crash seams. After the interruption exactly G1 or G2 is visible, no success
// is stamped before the commit, and recovery or retry settles on G2 with the
// committed long content intact.
func TestProjectionCommitRecovery(t *testing.T) {
	fixture := loadProjectionRecoveryFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	padding := fixture.Generation.LongTextPadding
	longText := "user input " + strings.Repeat("x", padding)
	longToolInput := "rg parser " + strings.Repeat("y", padding)
	longToolResult := "parser.go:12 " + strings.Repeat("z", padding)

	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			s, root := openGenerationStore(t)
			seedGenerationSession(t, s, fixture.Session.ID)

			complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, longText+" G1", longToolInput+" G1", longToolResult+" G1")
			if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
				t.Fatalf("G1 visible = %q, want %q", got, fixture.Generation.CompleteID)
			}

			installRecoveryFault(t, s, tc.Seam)
			failed, failedBlobs := buildTestGeneration(t, id, fixture.Generation.FailedID, longText+" G2", longToolInput+" G2", longToolResult+" G2")
			if err := activateTestGeneration(t, s, failed, failedBlobs); err == nil {
				t.Fatalf("activation across seam %s succeeded; a crash seam must interrupt it", tc.Seam)
			}

			want := fixture.Generation.CompleteID
			if tc.Visible == "g2" {
				want = fixture.Generation.FailedID
			}
			if got := visibleGeneration(t, s, id); got != want {
				t.Fatalf("after seam %s visible = %q, want %q", tc.Seam, got, want)
			}
			intent, err := s.generationArtifacts.ReadIntent(context.Background(), id)
			if err != nil {
				t.Fatalf("read pending intent after seam %s: %v", tc.Seam, err)
			}
			if intent == nil || intent.GenerationID != fixture.Generation.FailedID {
				t.Fatalf("after seam %s the pending activation intent was not retained: %+v", tc.Seam, intent)
			}

			clearRecoveryFault(t, s, tc.Seam)
			switch tc.Recovery {
			case "recover":
				if err := s.RecoverGenerationActivation(context.Background(), id); err != nil {
					t.Fatalf("recover after seam %s: %v", tc.Seam, err)
				}
			case "retry":
				if err := activateTestGeneration(t, s, failed, failedBlobs); err != nil {
					t.Fatalf("retry after seam %s: %v", tc.Seam, err)
				}
			case "mismatched-digest":
				// The durable envelope was altered after the candidate was
				// renamed into place. Recovery must refuse it without changing
				// the last-good read authority.
				assertIntentDigestMismatchRefused(t, s, id, fixture)
				return
			default:
				t.Fatalf("unknown recovery %q", tc.Recovery)
			}
			if got := visibleGeneration(t, s, id); got != fixture.Generation.FailedID {
				t.Fatalf("after recovery visible = %q, want %q", got, fixture.Generation.FailedID)
			}
			if intent, err := s.generationArtifacts.ReadIntent(context.Background(), id); err != nil || intent != nil {
				t.Fatalf("after recovery the activation intent was not cleared: %+v (err %v)", intent, err)
			}
			assertFullContent(t, s, id, fixture.Generation.FailedID, schema.SourceEntryRef(generationRefs[0]), longText+" G2")
			assertDurableRecoveryOutcome(t, root, id, fixture.Generation.FailedID, longText+" G2", longToolInput+" G2", longToolResult+" G2")
		})
	}

	t.Run(fixture.CorruptArtifact.Name, func(t *testing.T) {
		s, root := openGenerationStore(t)
		seedGenerationSession(t, s, fixture.Session.ID)
		complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, longText, longToolInput, longToolResult)
		if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
			t.Fatalf("activate G1: %v", err)
		}
		ref := schema.SourceEntryRef(fixture.CorruptArtifact.EntryRef)
		blobPath := filepath.Join(root, fixture.Session.ID, "generations", fixture.Generation.CompleteID, blobName(ref))
		if err := os.Remove(blobPath); err != nil {
			t.Fatalf("damage committed blob: %v", err)
		}
		content, err := s.ReadFullContent(context.Background(), id, fixture.Generation.CompleteID, indexformat.ContentRecord{
			Ref: ref, RelativeBlob: blobName(ref), ByteLength: int64(len(longText)), Digest: strings.Repeat("0", 64),
		})
		if err == nil {
			t.Fatal("damaged committed artifact resolved without an error")
		}
		if len(content) != 0 {
			t.Fatalf("damaged artifact returned %d bytes; partial content must never be served", len(content))
		}
		if !strings.Contains(err.Error(), "managed recovery") || !strings.Contains(err.Error(), "corrupt") {
			t.Fatalf("damaged artifact error is not actionable: %v", err)
		}
	})
}

// assertIntentDigestMismatchRefused alters only the durable intent's candidate
// binding after the staged candidate was renamed into place, then replays it
// through the real recovery path. Recovery must refuse the mismatched binding:
// the last-good generation and its success stamps stay unchanged, and the
// staged candidate and the intent are retained for a verified retry.
func assertIntentDigestMismatchRefused(t *testing.T, s *Store, id schema.SessionID, fixture projectionRecoveryFixture) {
	t.Helper()
	intent, err := s.generationArtifacts.ReadIntent(context.Background(), id)
	if err != nil {
		t.Fatalf("read durable intent before mutation: %v", err)
	}
	if intent == nil || intent.GenerationID != fixture.Generation.FailedID {
		t.Fatalf("pending intent = %+v, want generation %s", intent, fixture.Generation.FailedID)
	}
	before := readIndexStateForTest(t, s, id)
	intent.CandidateDigest = strings.Repeat("0", 64)
	if err := s.generationArtifacts.WriteIntent(context.Background(), *intent); err != nil {
		t.Fatalf("rewrite the mismatched durable intent: %v", err)
	}
	if err := s.RecoverGenerationActivation(context.Background(), id); err == nil {
		t.Fatal("recovery accepted a mismatched durable candidate binding; it must be refused")
	}
	if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
		t.Fatalf("after refused recovery visible = %q, want the last-good generation %q", got, fixture.Generation.CompleteID)
	}
	retained, err := s.generationArtifacts.ReadManifest(context.Background(), id, fixture.Generation.FailedID)
	if err != nil {
		t.Fatalf("staged candidate was not retained after refused recovery: %v", err)
	}
	if retained.ID != fixture.Generation.FailedID {
		t.Fatalf("retained candidate manifest = %q, want %q", retained.ID, fixture.Generation.FailedID)
	}
	stillPending, err := s.generationArtifacts.ReadIntent(context.Background(), id)
	if err != nil {
		t.Fatalf("read intent after refused recovery: %v", err)
	}
	if stillPending == nil || stillPending.GenerationID != fixture.Generation.FailedID {
		t.Fatalf("pending intent was not retained after refused recovery: %+v", stillPending)
	}
	after := readIndexStateForTest(t, s, id)
	if after.IndexerVersion != before.IndexerVersion {
		t.Fatalf("refused recovery changed index_version from %d to %d", before.IndexerVersion, after.IndexerVersion)
	}
	if (before.IndexedAt == nil) != (after.IndexedAt == nil) || (before.IndexedAt != nil && *before.IndexedAt != *after.IndexedAt) {
		t.Fatalf("refused recovery changed indexed_at from %v to %v", before.IndexedAt, after.IndexedAt)
	}
}

func assertFullContent(t *testing.T, s *Store, sid schema.SessionID, generationID string, ref schema.SourceEntryRef, want string) {
	t.Helper()
	var record indexformat.ContentRecord
	err := s.WithSessionSnapshot(context.Background(), sid, func(snapshot indexformat.ReadSnapshot) error {
		for _, candidate := range snapshot.Content {
			if candidate.Ref == ref {
				record = candidate
				return nil
			}
		}
		return fmt.Errorf("content ref %q is not in the captured snapshot", ref)
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.ByteLength <= int64(defaults.ContentPreviewLimit)+1024 {
		t.Fatalf("committed content for %q is %d bytes, not longer than preview+1024", ref, record.ByteLength)
	}
	content, err := s.ReadFullContent(context.Background(), sid, generationID, record)
	if err != nil {
		t.Fatalf("ReadFullContent: %v", err)
	}
	if string(content) != want {
		t.Fatalf("resolved content for %q = %q, want %q", ref, string(content), want)
	}
}

// reopenGenerationStore opens a second real SQLite/artifact store over the same
// on-disk state, so a crash-recovery assertion proves the durable outcome a
// later process would observe rather than the live process's cache.
func reopenGenerationStore(t *testing.T, root string) *Store {
	t.Helper()
	dir := filepath.Dir(root)
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

// assertDurableRecoveryOutcome reopens the real store and asserts the repaired
// metadata, success stamps, complete row/ref sets and every long blob survive
// the activation. It never trusts the live process state.
func assertDurableRecoveryOutcome(t *testing.T, root string, id schema.SessionID, generationID, longText, longToolInput, longToolResult string) {
	t.Helper()
	reopened := reopenGenerationStore(t, root)
	defer reopened.Close()

	err := reopened.WithSessionSnapshot(context.Background(), id, func(snapshot indexformat.ReadSnapshot) error {
		if snapshot.IndexVersion != 2 || snapshot.GenerationID != generationID {
			return fmt.Errorf("reopened snapshot = version %d generation %q, want 2/%q", snapshot.IndexVersion, snapshot.GenerationID, generationID)
		}
		if snapshot.Completeness != indexformat.GenerationCompletenessComplete {
			return fmt.Errorf("reopened completeness = %q, want complete", snapshot.Completeness)
		}
		if len(snapshot.Main.Entries) != len(generationRefs) {
			return fmt.Errorf("reopened main entries = %d, want %d", len(snapshot.Main.Entries), len(generationRefs))
		}
		entryRefs := map[schema.SourceEntryRef]struct{}{}
		for _, entry := range snapshot.Main.Entries {
			entryRefs[entry.SourceEntryRef] = struct{}{}
		}
		for _, want := range generationRefs {
			if _, ok := entryRefs[schema.SourceEntryRef(want)]; !ok {
				return fmt.Errorf("reopened main entries are missing ref %q", want)
			}
		}
		if len(snapshot.Content) != len(generationRefs) {
			return fmt.Errorf("reopened content records = %d, want %d", len(snapshot.Content), len(generationRefs))
		}
		if snapshot.Session.TurnCount != len(generationRefs) {
			return fmt.Errorf("reopened turn count = %d, want %d", snapshot.Session.TurnCount, len(generationRefs))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("reopened snapshot: %v", err)
	}

	// The alias row set survives reopen.
	conn, err := reopened.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	aliases := []string{}
	err = sqlitex.ExecuteTransient(conn, `SELECT native_key FROM session_projection_aliases WHERE session_id = ? AND generation_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(id), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			aliases = append(aliases, stmt.ColumnText(0))
			return nil
		},
	})
	reopened.pool.Put(conn)
	if err != nil {
		t.Fatalf("reopened alias rows: %v", err)
	}
	if len(aliases) != 1 || aliases[0] != "native-0" {
		t.Fatalf("reopened aliases = %v, want one native-0 alias", aliases)
	}

	// Every long blob resolves at its full length after reopen.
	for ref, want := range map[schema.SourceEntryRef]string{
		schema.SourceEntryRef(generationRefs[0]): longText,
		schema.SourceEntryRef(generationRefs[1]): longToolInput,
		schema.SourceEntryRef(generationRefs[2]): longToolResult,
	} {
		assertFullContent(t, reopened, id, generationID, ref, want)
	}

	// The repaired exported metadata is durable and names the committed
	// generation, so a later open needs no repair.
	metadataBytes, err := os.ReadFile(filepath.Join(root, string(id), "metadata.json"))
	if err != nil {
		t.Fatalf("reopened repaired metadata is missing: %v", err)
	}
	var exported schema.UnifiedMetadata
	if err := json.Unmarshal(metadataBytes, &exported); err != nil {
		t.Fatalf("reopened repaired metadata does not decode: %v", err)
	}
	if exported.SessionID != id || exported.Stats.TurnCount != len(generationRefs) {
		t.Fatalf("reopened repaired metadata = session %s turns %d, want session %s turns %d", exported.SessionID, exported.Stats.TurnCount, id, len(generationRefs))
	}

	// Success stamps are durable on the reopened store.
	stamps := readIndexStateForTest(t, reopened, id)
	if stamps.IndexerVersion != 1 || stamps.IndexedAt == nil || *stamps.IndexedAt != 1 {
		t.Fatalf("reopened stamps = (%d,%v), want (1,1)", stamps.IndexerVersion, stamps.IndexedAt)
	}
}

// lockAttemptBarrier is a delegating real SessionLocker that observably
// signals when an exclusive lock attempt starts. It wraps the production
// locker, so the test asserts against a genuine flock contention rather than a
// sleep: the signal proves the waiter reached the lock boundary before the
// test checks that it cannot complete.
type lockAttemptBarrier struct {
	SessionLocker
	exclusiveAttempts chan struct{}
}

func (b *lockAttemptBarrier) LockExclusive(ctx context.Context, id schema.SessionID) (func() error, error) {
	select {
	case b.exclusiveAttempts <- struct{}{}:
	default:
	}
	return b.SessionLocker.LockExclusive(ctx, id)
}

// TestConcurrentReadAcrossActivation pauses a reader inside its snapshot
// callback while holding the shared lock and hydrating the full G1 bodies,
// starts a G2 candidate with changed content, and proves the activation waits
// until the reader releases. The lock attempt is observed through a delegating
// real locker, so the wait is not inferred from a scheduler sleep. The reader
// sees exactly G1; after release the activation commits, the next reader sees
// exactly G2, and the same exclusive-lock barrier applies to cleanup.
func TestConcurrentReadAcrossActivation(t *testing.T) {
	fixture := loadProjectionRecoveryFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, _ := openGenerationStore(t)
	seedGenerationSession(t, s, fixture.Session.ID)
	longText := "user input " + strings.Repeat("x", fixture.Generation.LongTextPadding)
	longToolInput := "rg parser " + strings.Repeat("y", fixture.Generation.LongTextPadding)
	longToolResult := "parser.go:12 " + strings.Repeat("z", fixture.Generation.LongTextPadding)
	attempts := make(chan struct{}, 4)
	s.sessionLocker = &lockAttemptBarrier{SessionLocker: s.sessionLocker, exclusiveAttempts: attempts}

	g1, g1Blobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, longText+" G1", longToolInput, longToolResult)
	if err := activateTestGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}

	wantG1 := map[schema.SourceEntryRef]string{
		schema.SourceEntryRef(generationRefs[0]): longText + " G1",
		schema.SourceEntryRef(generationRefs[1]): longToolInput,
		schema.SourceEntryRef(generationRefs[2]): longToolResult,
	}
	readerEntered := make(chan string, 1)
	readerRelease := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- s.WithSessionSnapshot(context.Background(), id, func(snapshot indexformat.ReadSnapshot) error {
			readerEntered <- snapshot.GenerationID
			// Hydrate every full G1 body while the shared lock is held; the
			// exclusive activation below must therefore wait through serialization.
			for _, record := range snapshot.Content {
				content, err := s.ReadFullContent(context.Background(), id, snapshot.GenerationID, record)
				if err != nil {
					return err
				}
				if want, ok := wantG1[record.Ref]; ok && string(content) != want {
					return fmt.Errorf("G1 content for %q = %q, want %q", record.Ref, string(content), want)
				}
			}
			<-readerRelease
			return nil
		})
	}()
	select {
	case got := <-readerEntered:
		if got != fixture.Generation.CompleteID {
			t.Fatalf("reader entered with generation %q, want G1", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not enter its snapshot callback")
	}

	g2, g2Blobs := buildTestGeneration(t, id, fixture.Generation.FailedID, longText+" G2", longToolInput, longToolResult)
	activationDone := make(chan error, 1)
	go func() { activationDone <- activateTestGeneration(t, s, g2, g2Blobs) }()
	waitForExclusiveAttempt(t, attempts, "activation")
	select {
	case err := <-activationDone:
		close(readerRelease)
		t.Fatalf("activation completed while the reader held the shared lock (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}

	close(readerRelease)
	if err := <-readerDone; err != nil {
		t.Fatalf("reader: %v", err)
	}
	select {
	case err := <-activationDone:
		if err != nil {
			t.Fatalf("activation after reader release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("activation did not complete after the reader released the shared lock")
	}
	if got := visibleGeneration(t, s, id); got != fixture.Generation.FailedID {
		t.Fatalf("after activation visible = %q, want G2", got)
	}
	assertFullContent(t, s, id, fixture.Generation.FailedID, schema.SourceEntryRef(generationRefs[0]), longText+" G2")

	// Cleanup of an inactive generation takes the same exclusive lock, so it
	// also waits for a reader that is still holding the shared lock.
	cleanupReaderEntered := make(chan struct{}, 1)
	cleanupReaderRelease := make(chan struct{})
	cleanupReaderDone := make(chan error, 1)
	go func() {
		cleanupReaderDone <- s.WithSessionSnapshot(context.Background(), id, func(indexformat.ReadSnapshot) error {
			cleanupReaderEntered <- struct{}{}
			<-cleanupReaderRelease
			return nil
		})
	}()
	select {
	case <-cleanupReaderEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("second reader did not enter its snapshot callback")
	}
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.CompleteID)
	}()
	waitForExclusiveAttempt(t, attempts, "cleanup")
	select {
	case err := <-cleanupDone:
		close(cleanupReaderRelease)
		t.Fatalf("cleanup removed the inactive generation while a reader held the shared lock (err=%v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	close(cleanupReaderRelease)
	if err := <-cleanupReaderDone; err != nil {
		t.Fatalf("second reader: %v", err)
	}
	select {
	case err := <-cleanupDone:
		if err != nil {
			t.Fatalf("cleanup after reader release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not complete after the reader released the shared lock")
	}
	// Cleanup must never remove the active generation.
	if err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.FailedID); err == nil {
		t.Fatal("cleanup removed the active generation")
	}
}

// waitForExclusiveAttempt blocks until the exclusive-lock waiter observably
// reached its lock attempt, so a later non-completion check proves contention
// rather than goroutine scheduling.
func waitForExclusiveAttempt(t *testing.T, attempts <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-attempts:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s never attempted the exclusive session lock", label)
	}
}
