package store

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitemigration"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/content_gc.yaml
var contentGCTypedYAML []byte

//go:embed testdata/content_gc.manifest.yaml
var contentGCTypedManifestYAML []byte

// contentGCFlagSession is one flag-complete-after-v62 session shape: the
// active pointer (absent means NULL) and the projection generation rows it
// holds. The runner derives the expected flag from the v62 backfill
// predicate: flagged exactly when a non-active projection row exists.
type contentGCFlagSession struct {
	ID          string   `yaml:"id"`
	Active      *string  `yaml:"active,omitempty"`
	Generations []string `yaml:"generations,omitempty"`
}

// contentGCCase is one content_gc case: the section-10 name plus the typed
// expectations its runner asserts. Counts default to zero, so a sweep case
// states every expected count explicitly; flag, rebuild, and mirror
// expectations are pointers, so an absent field asserts nothing. A case
// with no typed fields is a placeholder a later change fills; the loader's
// manifest check still protects its name.
type contentGCCase struct {
	Name                string               `yaml:"name"`
	WantRowsDeleted     int64                `yaml:"wantRowsDeleted,omitempty"`
	WantBodiesDeleted   int64                `yaml:"wantBodiesDeleted,omitempty"`
	WantBlobsDeleted    int64                `yaml:"wantBlobsDeleted,omitempty"`
	WantDirsRemoved     int64                `yaml:"wantDirsRemoved,omitempty"`
	WantFlagCleared     *bool                `yaml:"wantFlagCleared,omitempty"`
	WantRebuilt         *bool                `yaml:"wantRebuilt,omitempty"`
	WantMirrorRows      *int64               `yaml:"wantMirrorRows,omitempty"`
	WantFullContentRows *int64               `yaml:"wantFullContentRows,omitempty"`
	FirstTerms          []string             `yaml:"firstTerms,omitempty"`
	SecondTerms         []string             `yaml:"secondTerms,omitempty"`
	MatchFound          []string             `yaml:"matchFound,omitempty"`
	MatchEmpty          []string             `yaml:"matchEmpty,omitempty"`
	Sessions            []contentGCFlagSession `yaml:"sessions,omitempty"`
}

// isPlaceholder reports whether the case carries no typed expectations, in
// which case the runner logs it and asserts nothing.
func (c contentGCCase) isPlaceholder() bool {
	return c.WantRowsDeleted == 0 && c.WantBodiesDeleted == 0 && c.WantBlobsDeleted == 0 &&
		c.WantDirsRemoved == 0 && c.WantFlagCleared == nil && c.WantRebuilt == nil &&
		c.WantMirrorRows == nil && c.WantFullContentRows == nil &&
		len(c.FirstTerms) == 0 && len(c.SecondTerms) == 0 &&
		len(c.MatchFound) == 0 && len(c.MatchEmpty) == 0 && len(c.Sessions) == 0
}

type contentGCFixtures struct {
	Cases []contentGCCase `yaml:"cases"`
}

func loadContentGCFixtures(t *testing.T) contentGCFixtures {
	t.Helper()
	var fixtures contentGCFixtures
	decoder := yaml.NewDecoder(strings.NewReader(string(contentGCTypedYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixtures); err != nil {
		t.Fatalf("decode content_gc.yaml: %v", err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentGCTypedManifestYAML)
	if err != nil {
		t.Fatalf("decode content_gc manifest: %v", err)
	}
	actual := make([]string, 0, len(fixtures.Cases))
	for _, c := range fixtures.Cases {
		actual = append(actual, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, actual, "content garbage collection"); err != nil {
		t.Fatal(err)
	}
	return fixtures
}

// TestContentGCFixtureManifest pins the content garbage collection case
// inventory: the typed loader compiles, the manifest loads, and every
// required name is present with no undeclared extra.
func TestContentGCFixtureManifest(t *testing.T) {
	loadContentGCFixtures(t)
}

// TestContentGCFamily runs the content_gc cases under their section-10
// names. Every case drives the production sweep (or the migration, prune,
// and foreign-key paths it names) and asserts the exact deleted set, the
// flag, and the raw MATCH sets. The runner is sequential: the sweep's crash
// seam is a process-global hook.
func TestContentGCFamily(t *testing.T) {
	fixtures := loadContentGCFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Name {
			case "active-only":
				runGCActiveOnly(t, c)
			case "superseded-only":
				runGCSupersededOnly(t, c)
			case "shared-active-superseded":
				runGCSharedActiveSuperseded(t, c)
			case "non-native-untouched":
				runGCNonNativeUntouched(t, c)
			case "crash-orphan":
				runGCCrashOrphan(t, c)
			case "fk-blocks-live-delete":
				runGCFKBlocksLiveDelete(t, c)
			case "blob-unreferenced-swept":
				runGCBlobUnreferencedSwept(t, c)
			case "blob-referenced-kept":
				runGCBlobReferencedKept(t, c)
			case "chunk-cascade":
				runGCChunkCascade(t, c)
			case "blob-shared-active-superseded":
				runGCBlobSharedActiveSuperseded(t, c)
			case "flag-complete-after-v62":
				runGCFlagCompleteAfterV62(t, c)
			case "directory-orphan-retried-without-rows":
				runGCDirectoryOrphanRetriedWithoutRows(t, c)
			case "representation-replacement-flagged-and-swept":
				runGCRepresentationReplacementFlaggedAndSwept(t, c)
			case "non-native-reindexed-as-native-settles":
				runGCNonNativeReindexedAsNativeSettles(t, c)
			case "sweep-session-qualified":
				runGCSweepSessionQualified(t, c)
			case "fts-postings-removed-on-sweep":
				runGCFTSPostingsRemovedOnSweep(t, c)
			case "fts-postings-removed-on-prune":
				runGCFTSPostingsRemovedOnPrune(t, c)
			default:
				if !c.isPlaceholder() {
					t.Fatalf("%s: not one of the cases this change owns, yet it carries typed expectations; route it to its owner", c.Name)
				}
				t.Logf("%s: placeholder; a later change fills this case", c.Name)
			}
		})
	}
}

// gcEntryRefs are the deterministic source refs the sweep-family candidates
// emit. They are stable across generations on purpose: a refresh over the
// same source entries reuses its refs, so an unchanged entry shares its
// body by digest.
var gcEntryRefs = []schema.SourceEntryRef{"e_gc0", "e_gc1", "e_gc2"}

// gcBuildCandidate builds one valid three-entry harmonized candidate from
// the given entry texts. Non-emitted refs in extraBlobs become blob objects
// with the supplied bytes, retained by one inherited context segment (the
// validator admits non-emitted content only for retained refs). It returns
// the filled candidate and its staged bytes for activation and digest
// oracles.
func gcBuildCandidate(t *testing.T, sid schema.SessionID, genID string, texts []string, extraBlobs map[string]string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	if len(texts) != len(gcEntryRefs) {
		t.Fatalf("candidate %s needs one text per entry (%d), got %d", genID, len(gcEntryRefs), len(texts))
	}
	entries := make([]schema.SessionEntry, 0, len(gcEntryRefs))
	blobs := make(map[schema.SourceEntryRef][]byte, len(gcEntryRefs)+len(extraBlobs))
	content := make([]indexformat.ContentRecord, 0, len(gcEntryRefs)+len(extraBlobs))
	for i, ref := range gcEntryRefs {
		text := texts[i]
		entries = append(entries, schema.SessionEntry{
			SessionID: sid, EntryIndex: i, Harness: defaults.HarnessClaudeCode,
			EntryType: ingest.EntryTypeText, Role: ingest.RoleUser,
			ContentPreview: &text, SourceEntryRef: ref,
		})
		blobs[ref] = []byte(text)
		content = append(content, indexformat.ContentRecord{Ref: ref})
	}
	retained := make([]schema.SourceEntryRef, 0, len(extraBlobs))
	for ref, payload := range extraBlobs {
		content = append(content, indexformat.ContentRecord{Ref: schema.SourceEntryRef(ref)})
		blobs[schema.SourceEntryRef(ref)] = []byte(payload)
		retained = append(retained, schema.SourceEntryRef(ref))
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
		Content:              content,
		Aliases:              []indexformat.NativeAlias{{NativeKey: "native-0", Ref: gcEntryRefs[0]}},
		SourceEvidenceDigest: strings.Repeat("c", 64),
		TitleRefs:            []schema.SourceEntryRef{gcEntryRefs[0]},
	}
	if len(retained) > 0 {
		sortStrings(retained)
		generation.Segments = []indexformat.ContextSegment{{
			Ordinal:          0,
			PhysicalSourceID: "gc-retained-source",
			Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
			Inclusion:        indexformat.SegmentInclusionInherited,
			CapturedRefs:     retained,
		}}
	}
	v2 := indexformat.V2{Generation: generation}
	return filledCandidateForValidation(t, v2, blobs), blobs
}

// gcTermTexts embeds one MATCH term per entry behind the candidate's name:
// entries of different generations over different terms never share a body,
// while byte-identical texts (the shared case reuses the old text) share
// one body by digest.
func gcTermTexts(genID string, terms []string) []string {
	texts := make([]string, 0, len(terms))
	for _, term := range terms {
		texts = append(texts, "gc "+genID+" entry "+term)
	}
	return texts
}

// sortStrings orders refs deterministically for the retaining segment.
func sortStrings(refs []schema.SourceEntryRef) {
	for i := 1; i < len(refs); i++ {
		for j := i; j > 0 && refs[j] < refs[j-1]; j-- {
			refs[j], refs[j-1] = refs[j-1], refs[j]
		}
	}
}

// gcActivate builds and activates one sweep-family candidate through the
// production activation. It returns the filled candidate and blobs for
// digest oracles.
func gcActivate(t *testing.T, s *Store, sid schema.SessionID, genID string, terms []string, extraBlobs map[string]string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return gcActivateTexts(t, s, sid, genID, gcTermTexts(genID, terms), extraBlobs, false)
}

// gcActivateTexts builds and activates one candidate from explicit entry
// texts: the shared case reuses the old text byte-for-byte so the entry
// shares its body. Explicit marks an operator-initiated rebuild that
// deliberately replaces full read authority (the reindexed-native case).
func gcActivateTexts(t *testing.T, s *Store, sid schema.SessionID, genID string, texts []string, extraBlobs map[string]string, explicit bool) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	v2, blobs := gcBuildCandidate(t, sid, genID, texts, extraBlobs)
	if !explicit {
		if err := activateTestGeneration(t, s, v2, blobs); err != nil {
			t.Fatalf("activate %s: %v", genID, err)
		}
		return v2, blobs
	}
	if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:      v2,
		Blobs:           blobs,
		IndexerVersion:  1,
		IndexedAtMs:     1,
		ExplicitRebuild: true,
	}); err != nil {
		t.Fatalf("activate %s as an explicit rebuild: %v", genID, err)
	}
	return v2, blobs
}

// gcBodyDigests recomputes one candidate's body digests from its in-memory
// fields: the memory-side oracle the sweep's deleted set is compared
// against.
func gcBodyDigests(t *testing.T, sid schema.SessionID, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) map[string]struct{} {
	t.Helper()
	prepared, err := prepareHarmonizedCandidate(sid, v2.Generation, blobs)
	if err != nil {
		t.Fatalf("prepare digest oracle: %v", err)
	}
	digests := make(map[string]struct{}, len(prepared.bodyDigests))
	for _, digest := range prepared.bodyDigests {
		digests[string(digest)] = struct{}{}
	}
	return digests
}

// gcRemainingDigests reads the session's stored body digests: the deleted
// set equals the seeded set minus this set, exactly.
func gcRemainingDigests(t *testing.T, s *Store, sid schema.SessionID) map[string]struct{} {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	remaining := map[string]struct{}{}
	if err := sqlitex.ExecuteTransient(conn, `SELECT body_digest FROM session_entry_bodies WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			remaining[stmt.ColumnText(0)] = struct{}{}
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return remaining
}

// gcCount counts one session-scoped table.
func gcCount(t *testing.T, s *Store, table string, sid schema.SessionID) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM `+table+` WHERE session_id = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid)},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// gcMatchCount counts the raw FTS documents matching one term: the
// section-10 assertion for every deleted body (zero) and every surviving
// one (nonzero).
func gcMatchCount(t *testing.T, s *Store, term string) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_search_fts WHERE session_search_fts MATCH ?`, &sqlitex.ExecOptions{
		Args: []any{term},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatalf("raw MATCH for %q: %v", term, err)
	}
	return count
}

// gcBlobChunks counts one blob's stored chunks.
func gcBlobChunks(t *testing.T, s *Store, sid schema.SessionID, digest string) int64 {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.ExecuteTransient(conn, `SELECT COUNT(*) FROM session_content_chunks WHERE session_id = ? AND digest = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), digest},
		ResultFunc: func(stmt *sqlite.Stmt) error {
			count = stmt.ColumnInt64(0)
			return nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	return count
}

// gcBlobDigest hashes blob bytes the way the writer addresses them.
func gcBlobDigest(t *testing.T, payload string) string {
	t.Helper()
	return gcDigestOf([]byte(payload))
}

// assertGCSweepResult asserts one sweep outcome against the fixture: the
// exact counts, the cleared flag, the rebuild bit, the mirror rows, and the
// raw MATCH sets.
func assertGCSweepResult(t *testing.T, s *Store, sid schema.SessionID, c contentGCCase, got SweepResult) {
	t.Helper()
	if got.RowsDeleted != c.WantRowsDeleted {
		t.Fatalf("%s: RowsDeleted = %d, want %d", c.Name, got.RowsDeleted, c.WantRowsDeleted)
	}
	if got.BodiesDeleted != c.WantBodiesDeleted {
		t.Fatalf("%s: BodiesDeleted = %d, want %d", c.Name, got.BodiesDeleted, c.WantBodiesDeleted)
	}
	if got.BlobsDeleted != c.WantBlobsDeleted {
		t.Fatalf("%s: BlobsDeleted = %d, want %d", c.Name, got.BlobsDeleted, c.WantBlobsDeleted)
	}
	if got.DirectoriesRemoved != c.WantDirsRemoved {
		t.Fatalf("%s: DirectoriesRemoved = %d, want %d", c.Name, got.DirectoriesRemoved, c.WantDirsRemoved)
	}
	if c.WantRebuilt != nil && got.Rebuilt != *c.WantRebuilt {
		t.Fatalf("%s: Rebuilt = %v, want %v", c.Name, got.Rebuilt, *c.WantRebuilt)
	}
	if c.WantFlagCleared != nil {
		// WantFlagCleared names the desired end state, so derive the
		// set-bit it implies and compare directly: a set flag fails a
		// case that wants it cleared, and vice versa.
		wantSet := !*c.WantFlagCleared
		if flag := readSweepFlag(t, s, sid); flag != wantSet {
			t.Fatalf("%s: sweep flag is set = %v, want set = %v", c.Name, flag, wantSet)
		}
	}
	if c.WantMirrorRows != nil {
		if got := gcCount(t, s, "session_entries", sid); got != *c.WantMirrorRows {
			t.Fatalf("%s: mirror rows = %d, want %d", c.Name, got, *c.WantMirrorRows)
		}
	}
	if c.WantFullContentRows != nil {
		if got := gcCount(t, s, "session_entry_full_content", sid); got != *c.WantFullContentRows {
			t.Fatalf("%s: full-content rows = %d, want %d", c.Name, got, *c.WantFullContentRows)
		}
		chunks := gcCount(t, s, "session_entry_full_content_chunks", sid)
		if *c.WantFullContentRows == 0 && chunks != 0 {
			t.Fatalf("%s: %d full-content chunks survive with no manifest rows, want none", c.Name, chunks)
		}
		if *c.WantFullContentRows > 0 && chunks == 0 {
			t.Fatalf("%s: no full-content chunks with %d manifest rows, want the stored prose", c.Name, *c.WantFullContentRows)
		}
	}
	for _, term := range c.MatchFound {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("%s: raw MATCH for surviving term %q returned no rows", c.Name, term)
		}
	}
	for _, term := range c.MatchEmpty {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("%s: raw MATCH for deleted term %q returned %d rows, want none", c.Name, term, got)
		}
	}
}

// assertGCDigestsEqual asserts the stored body set equals the expected set
// exactly: the deleted set is the seeded set minus this set.
func assertGCDigestsEqual(t *testing.T, s *Store, sid schema.SessionID, name string, want map[string]struct{}) {
	t.Helper()
	remaining := gcRemainingDigests(t, s, sid)
	if len(remaining) != len(want) {
		t.Fatalf("%s: %d bodies remain, want %d", name, len(remaining), len(want))
	}
	for digest := range want {
		if _, ok := remaining[digest]; !ok {
			t.Fatalf("%s: expected body %s is missing after the sweep", name, digest)
		}
	}
}

// gcSession seeds one sweep-family session row.
func gcSession(t *testing.T, s *Store, raw string) schema.SessionID {
	t.Helper()
	sid, err := schema.NewSessionID(raw)
	if err != nil {
		t.Fatal(err)
	}
	seedGenerationSession(t, s, string(sid))
	return sid
}

// runGCActiveOnly sweeps a session with only its active generation: nothing
// is deleted, the flag clears, and every term still matches.
func runGCActiveOnly(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "a1a1a1a1-a1a1-41a1-81a1-a1a1a1a1a1a1")
	gcActivate(t, s, sid, "gc_active_only", c.SecondTerms, nil)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep active-only: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
}

// runGCSupersededOnly sweeps a superseded generation beside disjoint active
// content: the old catalog rows and bodies are deleted exactly, the active
// set is byte-identical, and only the active terms match.
func runGCSupersededOnly(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "b2b2b2b2-b2b2-42b2-82b2-b2b2b2b2b2b2")
	gcActivate(t, s, sid, "gc_superseded_old", c.FirstTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_superseded_active", c.SecondTerms, nil)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep superseded-only: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, activeBlobs))
	if got := gcCount(t, s, "session_generations", sid); got != 1 {
		t.Fatalf("%s: %d generation rows remain, want the 1 active", c.Name, got)
	}
}

// runGCSharedActiveSuperseded sweeps a superseded generation that shares
// one body with the active one: the shared body survives with the active
// set, the old-only bodies are deleted, and the shared term still matches.
func runGCSharedActiveSuperseded(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "c3c3c3c3-c3c3-43c3-83c3-c3c3c3c3c3c3")
	oldTexts := gcTermTexts("gc_shared_old", c.FirstTerms)
	gcActivateTexts(t, s, sid, "gc_shared_old", oldTexts, nil, false)
	// The shared entry reuses the old text byte-for-byte (same index, ref,
	// and fields), so both generations address one body by digest.
	activeTexts := gcTermTexts("gc_shared_active", c.SecondTerms)
	activeTexts[0] = oldTexts[0]
	active, activeBlobs := gcActivateTexts(t, s, sid, "gc_shared_active", activeTexts, nil, false)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep shared-active-superseded: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, activeBlobs))
}

// runGCNonNativeUntouched sweeps a legacy session: the mirror and its
// full-content rows are intact and the flag stays clear.
func runGCNonNativeUntouched(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "d4d4d4d4-d4d4-44d4-84d4-d4d4d4d4d4d4")
	gcSeedV1(t, s, sid, "gc non-native untouched gcnativeonly", true)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep non-native-untouched: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
}

// runGCCrashOrphan recovers staged-but-uncommitted objects: the sweep
// deletes the orphan bodies and blob with the old generation intact, clears
// the flag, and the staged candidate still activates afterwards.
func runGCCrashOrphan(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "e5e5e5e5-e5e5-45e5-85e5-e5e5e5e5e5e5")
	old, oldBlobs := gcActivate(t, s, sid, "gc_crash_g1", c.FirstTerms, nil)
	staged, stagedBlobs := gcBuildCandidate(t, sid, "gc_crash_g2", gcTermTexts("gc_crash_g2", c.SecondTerms), map[string]string{"e_gcblobCrash": "gc crash orphan blob bytes"})
	filled := filledCandidateForValidation(t, staged, stagedBlobs)
	if _, err := s.StageGeneration(context.Background(), GenerationActivation{Generation: filled, Blobs: stagedBlobs}); err != nil {
		t.Fatalf("stage the crash candidate: %v", err)
	}
	if !readSweepFlag(t, s, sid) {
		t.Fatal("staged candidate left the sweep flag unset")
	}
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep crash-orphan: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, old, oldBlobs))
	// The dropped staging is reusable: the candidate activates cleanly over
	// the swept store, which is the re-harvest recovery.
	if err := activateTestGeneration(t, s, staged, stagedBlobs); err != nil {
		t.Fatalf("activate after the orphan sweep: %v", err)
	}
	if got := visibleGeneration(t, s, sid); got != "gc_crash_g2" {
		t.Fatalf("visible generation = %q after recovery, want gc_crash_g2", got)
	}
}

// runGCFKBlocksLiveDelete proves the database refuses a referenced body:
// the direct delete fails, the row survives, and its term still matches.
func runGCFKBlocksLiveDelete(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "f6f6f6f6-f6f6-46f6-86f6-f6f6f6f6f6f6")
	gcActivate(t, s, sid, "gc_fk_active", c.FirstTerms, nil)
	digests := gcRemainingDigests(t, s, sid)
	if len(digests) == 0 {
		t.Fatal("no bodies to guard")
	}
	var victim string
	for digest := range digests {
		victim = digest
		break
	}
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.ExecuteTransient(conn, `DELETE FROM session_entry_bodies WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
		Args: []any{string(sid), victim},
	}); err == nil {
		t.Fatal("deleting a referenced body succeeded; the foreign key must refuse it")
	} else if !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("referenced-body delete refused without a foreign-key error: %v", err)
	}
	if got := gcRemainingDigests(t, s, sid); len(got) != len(digests) {
		t.Fatalf("%d bodies remain after the refused delete, want %d", len(got), len(digests))
	}
	for _, term := range c.MatchFound {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("raw MATCH for guarded term %q returned no rows", term)
		}
	}
}

// runGCBlobUnreferencedSwept sweeps a blob only the superseded generation
// described: the old catalog rows, bodies, and blob go; the active set and
// its terms survive.
func runGCBlobUnreferencedSwept(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "a7a7a7a7-a7a7-47a7-87a7-a7a7a7a7a7a7")
	gcActivate(t, s, sid, "gc_blobu_old", c.FirstTerms, map[string]string{"e_gcblobU": "gc unreferenced blob bytes"})
	active, activeBlobs := gcActivate(t, s, sid, "gc_blobu_active", c.SecondTerms, nil)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep blob-unreferenced-swept: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, activeBlobs))
	if got := gcCount(t, s, "session_content", sid); got != 0 {
		t.Fatalf("%s: %d blob objects remain, want none", c.Name, got)
	}
}

// runGCBlobReferencedKept sweeps a session whose blob is still described:
// nothing is deleted, the blob and its chunks survive, and the flag
// clears.
func runGCBlobReferencedKept(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "b8b8b8b8-b8b8-48b8-88b8-b8b8b8b8b8b8")
	gcActivate(t, s, sid, "gc_blobrk_active", c.FirstTerms, map[string]string{"e_gcblobRK": "gc referenced blob bytes"})
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep blob-referenced-kept: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	digest := gcBlobDigest(t, "gc referenced blob bytes")
	if got := gcCount(t, s, "session_content", sid); got != 1 {
		t.Fatalf("%s: %d blob objects remain, want the 1 referenced", c.Name, got)
	}
	if got := gcBlobChunks(t, s, sid, digest); got != 1 {
		t.Fatalf("%s: %d chunks remain for the referenced blob, want 1", c.Name, got)
	}
}

// runGCChunkCascade sweeps a multi-chunk orphan blob: the header and every
// chunk go together, so no chunk survives its object.
func runGCChunkCascade(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "c9c9c9c9-c9c9-49c9-89c9-c9c9c9c9c9c9")
	payload := "gc big blob bytes " + strings.Repeat("x", 70000)
	gcActivate(t, s, sid, "gc_chunk_old", c.FirstTerms, map[string]string{"e_gcblobBig": payload})
	digest := gcBlobDigest(t, payload)
	if got := gcBlobChunks(t, s, sid, digest); got != 2 {
		t.Fatalf("seeded %d chunks for the big blob, want 2", got)
	}
	gcActivate(t, s, sid, "gc_chunk_active", c.SecondTerms, nil)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep chunk-cascade: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	if got := gcBlobChunks(t, s, sid, digest); got != 0 {
		t.Fatalf("%s: %d chunks survive the swept blob, want none", c.Name, got)
	}
}

// runGCBlobSharedActiveSuperseded sweeps a superseded generation that shares
// one blob with the active one: the shared object and its chunks survive,
// the old-only blob and its chunks go with the old rows.
func runGCBlobSharedActiveSuperseded(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "d0d0d0d0-d0d0-40d0-80d0-d0d0d0d0d0d0")
	shared := "gc shared blob bytes"
	gcActivate(t, s, sid, "gc_blobs_old", c.FirstTerms, map[string]string{"e_gcblobZ": shared, "e_gcblobW": "gc old-only blob bytes"})
	gcActivate(t, s, sid, "gc_blobs_active", c.SecondTerms, map[string]string{"e_gcblobZ": shared})
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep blob-shared-active-superseded: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	sharedDigest := gcBlobDigest(t, shared)
	if got := gcCount(t, s, "session_content", sid); got != 1 {
		t.Fatalf("%s: %d blob objects remain, want the 1 shared", c.Name, got)
	}
	if got := gcBlobChunks(t, s, sid, sharedDigest); got != 1 {
		t.Fatalf("%s: %d chunks remain for the shared blob, want 1", c.Name, got)
	}
	if got := gcBlobChunks(t, s, sid, gcBlobDigest(t, "gc old-only blob bytes")); got != 0 {
		t.Fatalf("%s: %d chunks survive the swept blob, want none", c.Name, got)
	}
}

// runGCFlagCompleteAfterV62 proves the v62 backfill flags exactly the
// sessions with a non-active projection row: an active-only session and a
// session with no rows stay clear, a NULL-active session with rows is
// flagged, and pending intents and staged directories (files the SQL cannot
// see) are drained by the migration instead.
func runGCFlagCompleteAfterV62(t *testing.T, c contentGCCase) {
	t.Helper()
	if len(c.Sessions) == 0 {
		t.Fatal("flag-complete-after-v62 carries no session shapes; the backfill predicate asserts against nothing")
	}
	ctx := t.Context()
	dbPath := filepath.Join(t.TempDir(), "gc-flag.db")
	conn, err := sqlite.OpenConn(dbPath, sqlite.OpenReadWrite, sqlite.OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	predecessor := sqlitemigration.Schema{
		Migrations:       dbSchema.Migrations[:61],
		MigrationOptions: dbSchema.MigrationOptions[:61],
	}
	if err := sqlitemigration.Migrate(ctx, conn, predecessor); err != nil {
		t.Fatalf("migrate to V61: %v", err)
	}
	execGCV61SQL(t, conn, `PRAGMA foreign_keys=OFF`)
	execGCV61SQL(t, conn, `
INSERT INTO host_slugs(opaque_id, host_slug) VALUES('gc-flag-host','gc-flag-host');
INSERT INTO projects(project_hash, canonical_cwd) VALUES('gc-flag-project','/synthetic/gc-flag');`)
	digest := strings.Repeat("f", 64)
	for _, session := range c.Sessions {
		active := "NULL"
		if session.Active != nil {
			active = "'" + *session.Active + "'"
		}
		execGCV61SQL(t, conn, `
INSERT INTO sessions(session_id, model_harness, model_id, opaque_host_id, project_hash, start_ms, end_ms, ingested_ms, source_path, source_format, schema_version, active_generation_id)
VALUES('`+session.ID+`','opencode','gc-flag-model','gc-flag-host','gc-flag-project',1,2,3,'/synthetic/gc-flag.jsonl','jsonl',11,`+active+`);`)
		for _, generationID := range session.Generations {
			execGCV61SQL(t, conn, `
INSERT INTO session_projection_generations(session_id, generation_id, metadata_json, source_evidence_digest, completeness, index_format_version, installed_at_ms, activated_at_ms)
VALUES('`+session.ID+`','`+generationID+`','{}','`+digest+`','complete',2,1,1);`)
		}
	}
	if err := sqlitemigration.Migrate(ctx, conn, dbSchema); err != nil {
		t.Fatalf("migrate to V62: %v", err)
	}
	for _, session := range c.Sessions {
		wantFlagged := false
		for _, generationID := range session.Generations {
			if session.Active == nil || generationID != *session.Active {
				wantFlagged = true
			}
		}
		var flag int64
		if err := sqlitex.ExecuteTransient(conn, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, &sqlitex.ExecOptions{
			Args: []any{session.ID},
			ResultFunc: func(stmt *sqlite.Stmt) error {
				flag = stmt.ColumnInt64(0)
				return nil
			},
		}); err != nil {
			t.Fatal(err)
		}
		if (flag == 1) != wantFlagged {
			t.Fatalf("%s: session %s flag = %d, want flagged = %v", c.Name, session.ID, flag, wantFlagged)
		}
	}
}

// execGCV61SQL runs one setup script on the flag-case migration connection.
func execGCV61SQL(t *testing.T, conn *sqlite.Conn, script string) {
	t.Helper()
	if err := sqlitex.ExecuteScript(conn, script, nil); err != nil {
		t.Fatalf("exec SQL: %v\nscript: %.200s", err, script)
	}
}

// runGCDirectoryOrphanRetriedWithoutRows sweeps an orphan directory with no
// rows behind it: the row step deletes nothing, the retry still removes the
// directory through the ownership proof and reports it.
func runGCDirectoryOrphanRetriedWithoutRows(t *testing.T, c contentGCCase) {
	t.Helper()
	s, root := openGenerationStore(t)
	sid := gcSession(t, s, "e1e1e1e1-e1e1-41e1-81e1-e1e1e1e1e1e1")
	gcActivate(t, s, sid, "gc_dir_active", c.FirstTerms, nil)
	orphanID := "gc_orphan_dir"
	staged, stagedBlobs := gcBuildCandidate(t, sid, orphanID, []string{"gc orphan dir body zero", "gc orphan dir body one", "gc orphan dir body two"}, nil)
	filled := filledCandidateForValidation(t, staged, stagedBlobs)
	encoded, err := json.Marshal(filled.Generation)
	if err != nil {
		t.Fatal(err)
	}
	genDir := filepath.Join(root, string(sid), "generations", orphanID)
	if err := os.MkdirAll(genDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(genDir, "manifest.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if !readSweepFlag(t, s, sid) {
		t.Fatal("active generation left the sweep flag unset")
	}
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep directory-orphan-retried-without-rows: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	if _, err := os.Stat(genDir); !os.IsNotExist(err) {
		t.Fatalf("%s: orphan directory survived the retry", c.Name)
	}
}

// runGCRepresentationReplacementFlaggedAndSwept replaces a stored
// representation: the format Delete flags the session before removing its
// rows, and the sweep clears the unreferenced bodies and blobs with no
// generation left behind.
func runGCRepresentationReplacementFlaggedAndSwept(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "f2f2f2f2-f2f2-42f2-82f2-f2f2f2f2f2f2")
	gcActivate(t, s, sid, "gc_replace_old", c.FirstTerms, map[string]string{"e_gcblobRep": "gc replacement blob bytes"})
	gcActivate(t, s, sid, "gc_replace_active", c.SecondTerms, nil)
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := (generationIndexFormat{}).Delete(context.Background(), conn, sid); err != nil {
		s.pool.Put(conn)
		t.Fatalf("representation replacement delete: %v", err)
	}
	s.pool.Put(conn)
	if !readSweepFlag(t, s, sid) {
		t.Fatal("representation replacement left the sweep flag unset")
	}
	if got := gcCount(t, s, "session_generations", sid); got != 0 {
		t.Fatalf("replacement left %d generation rows, want none", got)
	}
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep representation-replacement-flagged-and-swept: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	if got := len(gcRemainingDigests(t, s, sid)); got != 0 {
		t.Fatalf("%s: %d bodies remain, want none", c.Name, got)
	}
	if got := gcCount(t, s, "session_content", sid); got != 0 {
		t.Fatalf("%s: %d blob objects remain, want none", c.Name, got)
	}
}

// runGCNonNativeReindexedAsNativeSettles converts a legacy session through
// the write path: after the commit and the sweep no mirror row, no
// full-content row, and no flag remain, and the native terms match.
func runGCNonNativeReindexedAsNativeSettles(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "a3a3a3a3-a3a3-43a3-83a3-a3a3a3a3a3a3")
	preview := "gc reindexed v1 gcreindexv0"
	gcSeedV1(t, s, sid, preview, true)
	// The legacy session holds certified full read authority, so the
	// conversion runs as an operator-initiated explicit rebuild — the same
	// explicitness a reindex harvest carries.
	gcActivateTexts(t, s, sid, "gc_reindexed_native", gcTermTexts("gc_reindexed_native", c.SecondTerms), nil, true)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep non-native-reindexed-as-native-settles: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
}

// runGCSweepSessionQualified sweeps one of two sessions: the swept session
// settles exactly, and the other session's rows, bodies, flag, and terms
// are untouched.
func runGCSweepSessionQualified(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "b4b4b4b4-b4b4-44b4-84b4-b4b4b4b4b4b4")
	other := gcSession(t, s, "c5c5c5c5-c5c5-45c5-85c5-c5c5c5c5c5c5")
	gcActivate(t, s, sid, "gc_qual_old", c.FirstTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_qual_active", c.SecondTerms, nil)
	otherOldTerms := []string{"gcqualbo0", "gcqualbo1", "gcqualbo2"}
	otherActiveTerms := []string{"gcqualbn0", "gcqualbn1", "gcqualbn2"}
	otherOld, otherOldBlobs := gcActivate(t, s, other, "gc_qual_other_old", otherOldTerms, nil)
	otherActive, otherActiveBlobs := gcActivate(t, s, other, "gc_qual_other_active", otherActiveTerms, nil)
	otherDigests := gcBodyDigests(t, other, otherOld, otherOldBlobs)
	for digest := range gcBodyDigests(t, other, otherActive, otherActiveBlobs) {
		otherDigests[digest] = struct{}{}
	}
	otherGenerations := gcCount(t, s, "session_generations", other)
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep sweep-session-qualified: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, activeBlobs))
	if got := gcCount(t, s, "session_generations", other); got != otherGenerations {
		t.Fatalf("%s: other session holds %d generation rows, want the untouched %d", c.Name, got, otherGenerations)
	}
	assertGCDigestsEqual(t, s, other, c.Name+" (other session)", otherDigests)
	if !readSweepFlag(t, s, other) {
		t.Fatalf("%s: other session lost its sweep flag", c.Name)
	}
}

// runGCFTSPostingsRemovedOnSweep proves the sweep un-indexes exactly the
// swept bodies: raw MATCH finds the active terms and nothing else.
func runGCFTSPostingsRemovedOnSweep(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "d6d6d6d6-d6d6-46d6-86d6-d6d6d6d6d6d6")
	gcActivate(t, s, sid, "gc_fts_old", c.FirstTerms, nil)
	active, activeBlobs := gcActivate(t, s, sid, "gc_fts_active", c.SecondTerms, nil)
	for _, term := range c.FirstTerms {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("pre-sweep MATCH for %q returned no rows; the terms must be indexed first", term)
		}
	}
	got, err := s.SweepSession(context.Background(), sid)
	if err != nil {
		t.Fatalf("sweep fts-postings-removed-on-sweep: %v", err)
	}
	assertGCSweepResult(t, s, sid, c, got)
	assertGCDigestsEqual(t, s, sid, c.Name, gcBodyDigests(t, sid, active, activeBlobs))
}

// runGCFTSPostingsRemovedOnPrune proves pruning a harmonized session removes
// its postings: the session cascade fires the body un-indexing, so raw
// MATCH finds nothing afterwards.
func runGCFTSPostingsRemovedOnPrune(t *testing.T, c contentGCCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "e7e7e7e7-e7e7-47e7-87e7-e7e7e7e7e7e7")
	gcActivate(t, s, sid, "gc_prune_active", c.FirstTerms, nil)
	for _, term := range c.FirstTerms {
		if got := gcMatchCount(t, s, term); got == 0 {
			t.Fatalf("pre-prune MATCH for %q returned no rows; the terms must be indexed first", term)
		}
	}
	if _, err := s.PruneSessions(context.Background(), []ingest.SessionID{sid}); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, term := range c.MatchEmpty {
		if got := gcMatchCount(t, s, term); got != 0 {
			t.Fatalf("%s: raw MATCH for pruned term %q returned %d rows, want none", c.Name, term, got)
		}
	}
	if got := gcCount(t, s, "session_entry_bodies", sid); got != 0 {
		t.Fatalf("%s: %d bodies survive the prune, want none", c.Name, got)
	}
}

// gcSeedV1 writes one legacy mirror entry through the production V1 batch
// write: the bounded preview row plus, for a full capture, the chunked
// full-content rows the conversion later settles.
func gcSeedV1(t *testing.T, s *Store, sid schema.SessionID, preview string, full bool) {
	t.Helper()
	entry := schema.SessionEntry{
		SessionID: sid, EntryIndex: 0, Harness: defaults.HarnessClaudeCode,
		EntryType: schema.EntryTypeText, Role: schema.RoleUser,
		ContentPreview: &preview, SourceEntryRef: "e_gcv1",
	}
	capture := ingest.SessionContentCaptureWrite{
		Status:          ingest.ContentCaptureIncomplete,
		SourceAuthority: ingest.ContentSourceNone,
		CaptureFormat:   ingest.ContentCaptureFormatPreviewOnly,
	}
	if full {
		capture = ingest.SessionContentCaptureWrite{
			Status:           ingest.ContentCaptureComplete,
			SourceAuthority:  ingest.ContentSourceNewIngest,
			TranscriptOrigin: ingest.TranscriptOriginFile,
			CaptureFormat:    ingest.ContentCaptureFormatFull,
			CapturedAtMs:     1,
		}
	}
	results := s.IndexSessionEntryBatch(context.Background(), []ingest.SessionEntryWrite{{
		SessionID:          sid,
		Result:             indexformat.V1{Entries: []schema.SessionEntry{entry}},
		IndexVersion:       1,
		IndexerVersion:     1,
		IndexedAtMs:        1,
		RequireFullContent: full,
		ContentCapture:     capture,
	}})
	if len(results) != 1 || results[0].Err != nil || !results[0].Written {
		t.Fatalf("seed V1 mirror: %+v", results)
	}
}

// gcDigestOf hashes bytes the way the blob store addresses them.
func gcDigestOf(payload []byte) string {
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}
