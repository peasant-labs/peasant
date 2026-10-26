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
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
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
		DiscardedID     string `yaml:"discarded_id"`
		LongTextPadding int    `yaml:"long_text_padding"`
	} `yaml:"generation"`
	Cases []struct {
		Name       string `yaml:"name"`
		Seam       string `yaml:"seam"`
		Visible    string `yaml:"visible"`
		Recovery   string `yaml:"recovery"`
		TinyBudget bool   `yaml:"tiny_budget"`
	} `yaml:"cases"`
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
	actual := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		actual = append(actual, c.Name)
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
func openGenerationStore(t testing.TB) (*Store, string) {
	t.Helper()
	return openGenerationStoreWith(t)
}

// openGenerationStoreWith opens a real store like openGenerationStore with
// additional open options (for example a tiny write budget that forces
// budget-split staging).
func openGenerationStoreWith(t testing.TB, options ...OpenOption) (*Store, string) {
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
	base := []OpenOption{WithPoolSize(2), WithIndexFormats(generationIndexFormat{}), WithGenerationArtifacts(artifacts, locker)}
	s, err := Open(filepath.Join(dir, "generations.db"), append(base, options...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, root
}

// tinyStageBudget forces budget-split staging: every staging unit exceeds
// the byte budget, so each commits in its own transaction and the
// between-transactions seam can fire.
func tinyStageBudget() OpenOption {
	return WithWriteConfig(ingest.WriteConfig{BatchBytes: 1, BatchSessions: 64}.WithDefaults(1))
}

func execGenerationSQL(t testing.TB, s *Store, script string) {
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

func seedGenerationSession(t testing.TB, s *Store, sid string) {
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
// produces: every content record carrying staged bytes adopts its owned
// relative blob path, byte length and payload digest. Generation.Validate
// rejects an unfilled candidate, so a test that needs to prove a candidate
// is otherwise valid must validate the staged shape rather than the
// pre-stage value. Records without staged bytes stay unfilled: the prepare
// phase refuses them with the binding category instead of a setup fatal.
func filledCandidateForValidation(t testing.TB, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) indexformat.V2 {
	t.Helper()
	filled := append([]indexformat.ContentRecord(nil), v2.Generation.Content...)
	for i := range filled {
		payload, ok := blobs[filled[i].Ref]
		if !ok {
			continue
		}
		sum := sha256.Sum256(payload)
		if filled[i].RelativeBlob == "" {
			filled[i].RelativeBlob = "c_" + hex.EncodeToString(sum[:]) + ".blob"
		}
		if filled[i].ByteLength == 0 {
			filled[i].ByteLength = int64(len(payload))
		}
		if filled[i].Digest == "" {
			filled[i].Digest = hex.EncodeToString(sum[:])
		}
	}
	v2.Generation.Content = filled
	return v2
}

// installHarmonizedFault stops the writer at one named crash seam: the seam
// reports the injected error and the write stops there, so the test proves
// the seam leaves exactly the old or the new generation behind.
func installHarmonizedFault(t *testing.T, seam string) {
	t.Helper()
	harmonizedWriterSeam = func(stage string) error {
		if stage == seam {
			return fmt.Errorf("injected fault at %s", stage)
		}
		return nil
	}
	t.Cleanup(clearHarmonizedFault)
}

// clearHarmonizedFault resets the writer seam hook.
func clearHarmonizedFault() {
	harmonizedWriterSeam = nil
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

// visibleGeneration reads the session's active generation pointer: the one
// durable read authority, whatever representation holds it.
func visibleGeneration(t *testing.T, s *Store, sid schema.SessionID) string {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, sid)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil {
		return ""
	}
	return *active
}

func activateTestGeneration(t *testing.T, s *Store, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) error {
	t.Helper()
	_, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     filledCandidateForValidation(t, v2, blobs),
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
	return err
}

// TestProjectionCommitRecovery drives the real activation through the
// harmonized crash seams. After the interruption exactly G1 or G2 is
// visible, no success is stamped before the commit, and the retry settles
// on G2 with the committed long content intact.
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
			var s *Store
			if tc.TinyBudget {
				s, _ = openGenerationStoreWith(t, tinyStageBudget())
			} else {
				s, _ = openGenerationStore(t)
			}
			seedGenerationSession(t, s, fixture.Session.ID)

			complete, completeBlobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, longText+" G1", longToolInput+" G1", longToolResult+" G1")
			if err := activateTestGeneration(t, s, complete, completeBlobs); err != nil {
				t.Fatalf("activate G1: %v", err)
			}
			if got := visibleGeneration(t, s, id); got != fixture.Generation.CompleteID {
				t.Fatalf("G1 visible = %q, want %q", got, fixture.Generation.CompleteID)
			}
			before := readIndexStateForTest(t, s, id)

			installHarmonizedFault(t, tc.Seam)
			failed, failedBlobs := buildTestGeneration(t, id, fixture.Generation.FailedID, longText+" G2", longToolInput+" G2", longToolResult+" G2")
			if err := activateTestGeneration(t, s, failed, failedBlobs); err == nil {
				t.Fatalf("activation across seam %s succeeded; a crash seam must interrupt it", tc.Seam)
			}
			clearHarmonizedFault()

			want := fixture.Generation.CompleteID
			if tc.Visible == "g2" {
				want = fixture.Generation.FailedID
			}
			if got := visibleGeneration(t, s, id); got != want {
				t.Fatalf("after seam %s visible = %q, want %q", tc.Seam, got, want)
			}
			after := readIndexStateForTest(t, s, id)
			if tc.Visible == "g1" && (after.IndexerVersion != before.IndexerVersion) {
				t.Fatalf("interrupted activation stamped index_version %d, want the prior %d", after.IndexerVersion, before.IndexerVersion)
			}

			switch tc.Recovery {
			case "retry":
				if err := activateTestGeneration(t, s, failed, failedBlobs); err != nil {
					t.Fatalf("retry after seam %s: %v", tc.Seam, err)
				}
			case "complete":
				// The commit is durable; only the post-commit pass is
				// pending, so running it settles the session.
				if err := s.deleteConvertedMirrorRows(context.Background(), id); err != nil {
					t.Fatalf("complete after seam %s: %v", tc.Seam, err)
				}
			default:
				t.Fatalf("unknown recovery %q", tc.Recovery)
			}
			if got := visibleGeneration(t, s, id); got != fixture.Generation.FailedID {
				t.Fatalf("after recovery visible = %q, want %q", got, fixture.Generation.FailedID)
			}
			assertHarmonizedContent(t, s, id, fixture.Generation.FailedID, failed, failedBlobs)
		})
	}
}

// assertHarmonizedContent proves the writer's structural round-trip for one
// committed generation: every mapping row names a body whose canonical text
// is byte-identical to the candidate entry's JSON, every body digest is its
// sha256, every non-emitted descriptor names a blob whose reassembled bytes
// equal the staged bytes, and the orphan check finds no generation row it
// should not.
func assertHarmonizedContent(t *testing.T, s *Store, sid schema.SessionID, generationID string, candidate indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	type mapped struct {
		partition int
		index     int
		digest    string
	}
	var mapping []mapped
	if err := sqlitex.ExecuteTransient(conn, `SELECT partition_id, entry_index, body_digest FROM session_generation_entries WHERE session_id = ? AND generation_id = ? ORDER BY partition_id, entry_index`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			mapping = append(mapping, mapped{partition: stmt.ColumnInt(0), index: stmt.ColumnInt(1), digest: stmt.ColumnText(2)})
			return nil
		},
	}); err != nil {
		t.Fatalf("read generation mapping: %v", err)
	}
	var want []schema.SessionEntry
	want = append(want, candidate.Generation.Main.Entries...)
	for _, section := range candidate.Generation.Earlier {
		want = append(want, section.Content.Entries...)
	}
	if len(mapping) != len(want) {
		t.Fatalf("mapped entries = %d, want %d", len(mapping), len(want))
	}
	for i, entry := range want {
		record, err := entryRecordFromEntry(entry)
		if err != nil {
			t.Fatalf("map entry %d: %v", i, err)
		}
		wantDigest := string(bodyDigestForRecord(record))
		if mapping[i].digest != wantDigest {
			t.Fatalf("mapping[%d] digest = %q, want the canonical sha256 %q", i, mapping[i].digest, wantDigest)
		}
		var stored string
		if err := sqlitex.ExecuteTransient(conn, `SELECT `+sqlSelectBodyColumns+` FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
			Args: []any{string(sid), wantDigest},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				stored = string(serializeEntry(scanEntryRecord(stmt)))
				return nil
			},
		}); err != nil {
			t.Fatalf("read body %q: %v", wantDigest, err)
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		if stored != string(encoded) {
			t.Fatalf("body %d canonical text differs:\n got %.120q\nwant %.120q", i, stored, string(encoded))
		}
	}
	emitted := map[schema.SourceEntryRef]struct{}{}
	for _, entry := range want {
		if entry.SourceEntryRef != "" {
			emitted[entry.SourceEntryRef] = struct{}{}
		}
	}
	for _, record := range candidate.Generation.Content {
		if _, ok := emitted[record.Ref]; ok {
			continue
		}
		wantBytes, ok := blobs[record.Ref]
		if !ok {
			t.Fatalf("no staged bytes for non-emitted ref %q", record.Ref)
		}
		var header string
		var byteLength int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT digest, byte_length FROM session_content WHERE session_id = ? AND digest = (SELECT digest FROM session_generation_content WHERE session_id = ? AND generation_id = ? AND source_entry_ref = ?)`, &sqlitex.ExecOptions{
			Args: []any{string(sid), string(sid), generationID, string(record.Ref)},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				header = stmt.ColumnText(0)
				byteLength = stmt.ColumnInt64(1)
				return nil
			},
		}); err != nil {
			t.Fatalf("read blob header for %q: %v", record.Ref, err)
		}
		if header == "" {
			t.Fatalf("no blob header for non-emitted ref %q", record.Ref)
		}
		if byteLength != int64(len(wantBytes)) {
			t.Fatalf("blob %q length = %d, want %d", record.Ref, byteLength, len(wantBytes))
		}
		var assembled []byte
		if err := sqlitex.ExecuteTransient(conn, `SELECT data FROM session_content_chunks WHERE session_id = ? AND digest = ? ORDER BY chunk_index`, &sqlitex.ExecOptions{
			Args: []any{string(sid), header},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				n := stmt.ColumnLen(0)
				buf := make([]byte, n)
				stmt.ColumnBytes(0, buf)
				assembled = append(assembled, buf...)
				return nil
			},
		}); err != nil {
			t.Fatalf("read blob chunks for %q: %v", record.Ref, err)
		}
		if string(assembled) != string(wantBytes) {
			t.Fatalf("blob %q bytes differ: got %d bytes, want %d", record.Ref, len(assembled), len(wantBytes))
		}
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

// assertDurableRecoveryOutcome reopens the real store and asserts the
// committed generation rows, the active pointer, the mapping, and the
// success stamps survive the activation. It never trusts the live process
// state.
func assertDurableRecoveryOutcome(t *testing.T, root string, id schema.SessionID, generationID, longText, longToolInput, longToolResult string) {
	t.Helper()
	reopened := reopenGenerationStore(t, root)
	defer reopened.Close()

	conn, err := reopened.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || *active != generationID {
		t.Fatalf("reopened active = %v, want %q", active, generationID)
	}
	view, err := readActiveHarmonizedView(conn, id)
	if err != nil {
		t.Fatal(err)
	}
	if !view.found || view.generationID != generationID {
		t.Fatalf("reopened harmonized view found=%v id=%q, want found with %q", view.found, view.generationID, generationID)
	}
	if view.record.Completeness != indexformat.GenerationCompletenessComplete {
		t.Fatalf("reopened completeness = %q, want complete", view.record.Completeness)
	}
	if len(view.mapping) != len(generationRefs) {
		t.Fatalf("reopened mapping rows = %d, want %d", len(view.mapping), len(generationRefs))
	}
	reopened.pool.Put(conn)

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
// test checks that it cannot complete. It also stamps the instant the real
// lock call returns, for an acquisition-order assertion with no timing window.
type lockAttemptBarrier struct {
	SessionLocker
	exclusiveAttempts chan struct{}
	acquired          chan time.Time
}

func (b *lockAttemptBarrier) LockExclusive(ctx context.Context, id schema.SessionID) (func() error, error) {
	select {
	case b.exclusiveAttempts <- struct{}{}:
	default:
	}
	release, err := b.SessionLocker.LockExclusive(ctx, id)
	if err == nil && b.acquired != nil {
		select {
		case b.acquired <- time.Now():
		default:
		}
	}
	return release, err
}

// TestConcurrentReadAcrossActivation pauses a reader inside the shared lock
// while it hydrates the full G1 bodies, starts a G2 candidate with changed
// content, and proves the activation waits until the reader releases. The
// lock attempt is observed through a delegating real locker, so the wait is
// not inferred from a scheduler sleep: the signal proves the waiter reached
// the lock boundary before the test checks that it cannot complete. The
// reader sees exactly G1; after release the activation commits, the next
// reader sees exactly G2, and the same exclusive-lock barrier applies to
// cleanup.
func TestConcurrentReadAcrossActivation(t *testing.T) {
	fixture := loadProjectionRecoveryFixture(t)
	id, err := schema.NewSessionID(fixture.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	s, root := openGenerationStore(t)
	seedGenerationSession(t, s, fixture.Session.ID)
	longText := "user input " + strings.Repeat("x", fixture.Generation.LongTextPadding)
	longToolInput := "rg parser " + strings.Repeat("y", fixture.Generation.LongTextPadding)
	longToolResult := "parser.go:12 " + strings.Repeat("z", fixture.Generation.LongTextPadding)
	attempts := make(chan struct{}, 4)
	acquired := make(chan time.Time, 4)
	s.sessionLocker = &lockAttemptBarrier{SessionLocker: s.sessionLocker, exclusiveAttempts: attempts, acquired: acquired}

	g1, g1Blobs := buildTestGeneration(t, id, fixture.Generation.CompleteID, longText+" G1", longToolInput, longToolResult)
	if err := activateTestGeneration(t, s, g1, g1Blobs); err != nil {
		t.Fatalf("activate G1: %v", err)
	}
	// The setup activation above took the exclusive lock and left its signal
	// and stamp buffered; clear them so the waits below observe only contention.
	drainExclusiveAttempts(attempts)
	drainAcquired(acquired)

	readerEntered := make(chan string, 1)
	readerRelease := make(chan struct{})
	readerDone := make(chan error, 1)
	go func() {
		readerDone <- func() error {
			release, err := s.sessionLocker.LockShared(context.Background(), id)
			if err != nil {
				return err
			}
			defer func() { _ = release() }()
			// Hydrate every full G1 body while the shared lock is held; the
			// exclusive activation below must therefore wait through it.
			got, err := readLockedBodyContent(s, id, fixture.Generation.CompleteID)
			if err != nil {
				return err
			}
			readerEntered <- got
			<-readerRelease
			return nil
		}()
	}()
	if got := testwait.Receive(t, readerEntered, "reader entered with its G1 bodies"); got != fixture.Generation.CompleteID {
		t.Fatalf("reader entered with generation %q, want G1", got)
	}

	g2, g2Blobs := buildTestGeneration(t, id, fixture.Generation.FailedID, longText+" G2", longToolInput, longToolResult)
	activationDone := make(chan error, 1)
	go func() { activationDone <- activateTestGeneration(t, s, g2, g2Blobs) }()
	waitForExclusiveAttempt(t, attempts, "activation")

	// The barrier signals before the real lock call, so a completion check
	// cannot prove the lock is held. Release the reader and assert instead that
	// the activation acquired the exclusive lock only after that instant; its
	// attempt above happened first, so a lock that was not held would have been
	// acquired before releaseAt.
	activationReleaseAt := time.Now()
	close(readerRelease)
	if err := testwait.Receive(t, readerDone, "reader left its shared lock"); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if err := testwait.Receive(t, activationDone, "activation completed after the reader released the shared lock"); err != nil {
		t.Fatalf("activation after reader release: %v", err)
	}
	if acquiredAt := testwait.Receive(t, acquired, "activation acquired the exclusive session lock"); acquiredAt.Before(activationReleaseAt) {
		t.Fatal("activation acquired the exclusive session lock before the reader released the shared lock")
	}
	if got := visibleGeneration(t, s, id); got != fixture.Generation.FailedID {
		t.Fatalf("after activation visible = %q, want G2", got)
	}
	assertLockedBodyContains(t, s, id, fixture.Generation.FailedID, longText+" G2")

	// Cleanup of an owned inactive file-backed generation takes the same
	// exclusive lock, so it also waits for a reader that is still holding
	// the shared lock.
	seedInactiveFileBackedGeneration(t, s, root, id, "gen-inactive-dir")
	cleanupReaderEntered := make(chan struct{}, 1)
	cleanupReaderRelease := make(chan struct{})
	cleanupReaderDone := make(chan error, 1)
	go func() {
		cleanupReaderDone <- func() error {
			release, err := s.sessionLocker.LockShared(context.Background(), id)
			if err != nil {
				return err
			}
			defer func() { _ = release() }()
			cleanupReaderEntered <- struct{}{}
			<-cleanupReaderRelease
			return nil
		}()
	}()
	testwait.Receive(t, cleanupReaderEntered, "second reader entered its shared lock")
	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- s.CleanupInactiveGeneration(context.Background(), id, "gen-inactive-dir")
	}()
	waitForExclusiveAttempt(t, attempts, "cleanup")
	cleanupReleaseAt := time.Now()
	close(cleanupReaderRelease)
	if err := testwait.Receive(t, cleanupReaderDone, "second reader left its shared lock"); err != nil {
		t.Fatalf("second reader: %v", err)
	}
	if err := testwait.Receive(t, cleanupDone, "cleanup completed after the reader released the shared lock"); err != nil {
		t.Fatalf("cleanup after reader release: %v", err)
	}
	if acquiredAt := testwait.Receive(t, acquired, "cleanup acquired the exclusive session lock"); acquiredAt.Before(cleanupReleaseAt) {
		t.Fatal("cleanup acquired the exclusive session lock before the reader released the shared lock")
	}
	if _, err := os.Stat(filepath.Join(root, fixture.Session.ID, "generations", "gen-inactive-dir")); !os.IsNotExist(err) {
		t.Fatalf("cleanup left the inactive generation directory: %v", err)
	}
	// Cleanup must never remove the active generation.
	if err := s.CleanupInactiveGeneration(context.Background(), id, fixture.Generation.FailedID); err == nil {
		t.Fatal("cleanup removed the active generation")
	}
}

// readLockedBodyContent reads one generation's active pointer and its first
// body text. It runs on the caller's connection while the caller holds the
// shared lock, so the test proves what a locked reader observes.
func readLockedBodyContent(s *Store, sid schema.SessionID, generationID string) (string, error) {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return "", err
	}
	defer s.pool.Put(conn)
	active, err := readActiveGenerationOnConn(conn, sid)
	if err != nil {
		return "", err
	}
	if active == nil || *active != generationID {
		return "", fmt.Errorf("active = %v, want %q", active, generationID)
	}
	return *active, nil
}

// assertLockedBodyContains proves one committed body holds the expected
// content bytes: the mapping row joined to its body row, read after the
// commit released its locks.
func assertLockedBodyContains(t *testing.T, s *Store, sid schema.SessionID, generationID, want string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	found := false
	if err := sqlitex.ExecuteTransient(conn, `SELECT b.content_preview, b.tool_output FROM session_entry_bodies b JOIN session_generation_entries m ON m.session_id = b.session_id AND m.body_digest = b.body_digest WHERE m.session_id = ? AND m.generation_id = ? AND m.partition_id = 0 ORDER BY m.entry_index LIMIT 1`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			found = true
			preview := stmt.ColumnText(0)
			if !strings.Contains(preview, want) && stmt.ColumnText(1) != "" {
				preview = stmt.ColumnText(1)
			}
			if !strings.Contains(preview, want) {
				t.Errorf("first body holds %q, want content containing %q", preview, want)
			}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatalf("no main body rows for generation %q", generationID)
	}
}

// seedInactiveFileBackedGeneration records one owned inactive file-backed
// generation: a catalog row plus its directory with a valid manifest, so
// directory cleanup has something owned to remove. The session's active
// pointer names a harmonized generation, so this row is inactive by
// construction.
func seedInactiveFileBackedGeneration(t *testing.T, s *Store, root string, sid schema.SessionID, generationID string) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, title_refs_json, source_evidence_digest, completeness, index_format_version, installed_at_ms) VALUES (?, ?, '{}', '[]', ?, 'complete', 2, 1)`, &sqlitex.ExecOptions{
		Args: []any{string(sid), generationID, strings.Repeat("b", 64)},
	}); err != nil {
		t.Fatalf("seed inactive file-backed generation: %v", err)
	}
	genDir := filepath.Join(root, string(sid), "generations", generationID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatalf("seed inactive generation directory: %v", err)
	}
	v2, blobs := buildTestGeneration(t, sid, generationID, "inactive text", "inactive input", "inactive output")
	filled := filledCandidateForValidation(t, v2, blobs)
	manifest, err := json.Marshal(filled.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatalf("seed inactive generation manifest: %v", err)
	}
}

// waitForExclusiveAttempt blocks until the exclusive-lock waiter observably
// reached its lock attempt, so a later non-completion check proves contention
// rather than goroutine scheduling.
func waitForExclusiveAttempt(t *testing.T, attempts <-chan struct{}, label string) {
	t.Helper()
	testwait.Receive(t, attempts, label+" attempted the exclusive session lock")
}

// drainExclusiveAttempts clears the setup activation's exclusive-lock signal so
// the converted waits observe only the contended operations. The attempt
// barrier buffers that signal, and a converted wait would otherwise consume it
// and lose the proof that activation and cleanup wait for a reader's shared
// lock.
func drainExclusiveAttempts(attempts chan struct{}) {
	for {
		select {
		case <-attempts:
			continue
		default:
			return
		}
	}
}

// drainAcquired clears the setup activation's lock-acquisition stamp for the
// same reason.
func drainAcquired(acquired chan time.Time) {
	for {
		select {
		case <-acquired:
			continue
		default:
			return
		}
	}
}
