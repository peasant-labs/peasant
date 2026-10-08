package store

import (
	"context"
	_ "embed"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type hostileInputCase struct {
	Name           string   `yaml:"name"`
	Operation      string   `yaml:"operation"`
	Content        string   `yaml:"content"`
	ContentBytes   []byte   `yaml:"contentBytes"`
	Repeat         int      `yaml:"repeat"`
	ForeignSession string   `yaml:"foreignSession"`
	DuplicateEntry int      `yaml:"duplicateEntry"`
	DuplicateOf    int      `yaml:"duplicateOf"`
	Texts          []string `yaml:"texts"`
	Ref            string   `yaml:"ref"`
	ExpectedDigest string   `yaml:"expectedDigest"`
	ExpectedChunks int      `yaml:"expectedChunks"`
	ExpectedLength int      `yaml:"expectedLength"`
}

//go:embed testdata/content_hostile_input.yaml
var contentHostileInputYAML []byte

//go:embed testdata/content_hostile_input.manifest.yaml
var contentHostileInputManifestYAML []byte

func LoadContentHostileInputFixtures(t *testing.T) []hostileInputCase {
	t.Helper()
	var fixtures struct {
		Cases []hostileInputCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(contentHostileInputYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentHostileInputManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		if c.Operation == "" {
			t.Fatalf("hostile input %q has no operation", c.Name)
		}
		if c.Operation == "forged-full-claim" && len(c.Texts) != 3 {
			t.Fatalf("hostile input %q needs text, input, and output fixture values", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "hostile input"); err != nil {
		t.Fatal(err)
	}
	return fixtures.Cases
}

// hostileBase builds one valid candidate the hostile cases mutate: valid
// content records, valid entries, and bytes for every record.
func hostileBase(t *testing.T, sid schema.SessionID, genID string) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
	t.Helper()
	return buildTestGeneration(t, sid, genID, "hostile text", "hostile input", "hostile output")
}

// activateHostile attempts one hostile activation and returns its error: a
// nil error means the store accepted hostile input.
func activateHostile(t *testing.T, s *Store, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) error {
	t.Helper()
	_, err := s.ActivateGeneration(context.Background(), GenerationActivation{
		Generation:     v2,
		Blobs:          blobs,
		IndexerVersion: 1,
		IndexedAtMs:    1,
	})
	return err
}

// assertNothingStaged proves a refused activation staged nothing and left
// the session without bodies, generations, or the sweep flag: hostile
// input stops in prepare before any write.
func assertNothingStaged(t *testing.T, s *Store, sid schema.SessionID) {
	t.Helper()
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if got := countRowsOnConn(t, conn, `SELECT COUNT(*) FROM session_entry_bodies WHERE session_id = ?`, string(sid)); got != 0 {
		t.Fatalf("refused activation staged %d bodies", got)
	}
	if got := countRowsOnConn(t, conn, `SELECT COUNT(*) FROM session_content WHERE session_id = ?`, string(sid)); got != 0 {
		t.Fatalf("refused activation staged %d blobs", got)
	}
	if got := countRowsOnConn(t, conn, `SELECT COUNT(*) FROM session_generations WHERE session_id = ?`, string(sid)); got != 0 {
		t.Fatalf("refused activation wrote %d generation rows", got)
	}
	if readSweepFlag(t, s, sid) {
		t.Fatal("refused activation set the sweep flag")
	}
}

// TestHarmonizedHostileInput drives the store boundary through its
// section-10 hostile dimensions: parse and assessment failures stop in
// prepare, before any write, and the store computes digests over the bytes
// it stores rather than reading one from input.
func TestHarmonizedHostileInput(t *testing.T) {
	sid, err := schema.NewSessionID("f6f6f6f6-f6f6-46f6-86f6-f6f6f6f6f6f6")
	if err != nil {
		t.Fatal(err)
	}

	for _, c := range LoadContentHostileInputFixtures(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Operation {
			case "empty-non-emitted-stages-on-harvest":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_empty_staged")
				ref, err := schema.NewSourceEntryRef(c.Ref)
				if err != nil {
					t.Fatal(err)
				}
				v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{Ref: ref})
				blobs[ref] = []byte(c.Content)
				v2.Generation.Segments = []indexformat.ContextSegment{{
					Ordinal: 0, PhysicalSourceID: "source-empty-staged",
					Coordinates: indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
					Inclusion:   indexformat.SegmentInclusionInherited, CapturedRefs: []schema.SourceEntryRef{ref},
				}}
				activation := GenerationActivation{Generation: v2, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 1}
				activation.Prepared, err = s.StageGeneration(t.Context(), activation)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = s.ActivateGeneration(t.Context(), activation); err != nil {
					t.Fatal(err)
				}
				conn, err := s.pool.Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				found := false
				err = sqlitex.ExecuteTransient(conn, `SELECT digest, byte_length, (SELECT count(*) FROM session_content_chunks c WHERE c.session_id=h.session_id AND c.digest=h.digest) FROM session_content h WHERE session_id=?`, &sqlitex.ExecOptions{
					Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
						found = true
						if stmt.ColumnText(0) != c.ExpectedDigest || stmt.ColumnInt(1) != c.ExpectedLength || stmt.ColumnInt(2) != c.ExpectedChunks {
							t.Errorf("empty header/chunks: got %s/%d/%d", stmt.ColumnText(0), stmt.ColumnInt(1), stmt.ColumnInt(2))
						}
						return nil
					},
				})
				s.pool.Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				if !found {
					t.Fatal("empty blob header missing")
				}
				conn, err = s.pool.Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				var record indexformat.ContentRecord
				err = sqlitex.ExecuteTransient(conn, `SELECT c.source_entry_ref,c.digest,h.byte_length FROM session_generation_content c JOIN session_content h ON h.session_id=c.session_id AND h.digest=c.digest WHERE c.session_id=? AND c.generation_id=? AND c.source_entry_ref=?`, &sqlitex.ExecOptions{
					Args: []any{string(sid), v2.Generation.ID, string(ref)}, ResultFunc: func(stmt *sqlite.Stmt) error {
						record = indexformat.ContentRecord{Ref: ref, Digest: stmt.ColumnText(1), ByteLength: stmt.ColumnInt64(2)}
						return nil
					},
				})
				if err != nil {
					s.pool.Put(conn)
					t.Fatal(err)
				}
				if record.Ref != ref {
					s.pool.Put(conn)
					t.Fatal("empty blob descriptor missing")
				}
				// Non-emitted refs are not hydration fields. The verified blob
				// reader used by conversion reads their complete header/chunk data.
				data, err := readMigrateBlobBytes(conn, sid, record.Digest)
				s.pool.Put(conn)
				if err != nil {
					t.Fatal(err)
				}
				if string(data) != c.Content {
					t.Fatalf("empty full read returned %q", data)
				}
			case "malformed-record":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_hostile_malformed")
				broken := c.Content
				v2.Generation.Main.Entries[0].Extra = &broken
				if err := activateHostile(t, s, v2, blobs); err == nil {
					t.Fatal("malformed extra succeeded; hostile input must stop in prepare")
				}
				assertNothingStaged(t, s, sid)
			case "oversized-record-placeholder":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				// The store carries no size gate: a large entry stages like any
				// other, and its digest still anchors its exact bytes. The record
				// size limit and its placeholder live in ingest, above this
				// boundary; what reaches the store is ordinary data.
				v2, blobs := hostileBase(t, sid, "gen_hostile_large")
				large := strings.Repeat(c.Content, c.Repeat)
				v2.Generation.Main.Entries[0].ContentPreview = &large
				blobs[schema.SourceEntryRef("e_u1")] = []byte(large)
				if err := activateHostile(t, s, filledCandidateForValidation(t, v2, blobs), blobs); err != nil {
					t.Fatalf("large entry refused: %v", err)
				}
			case "invalid-utf8":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_hostile_utf8")
				broken := string(c.ContentBytes)
				v2.Generation.Main.Entries[0].ContentPreview = &broken
				blobs[schema.SourceEntryRef("e_u1")] = []byte(broken)
				if err := activateHostile(t, s, v2, blobs); err == nil {
					t.Fatal("invalid UTF-8 succeeded; hostile input must stop in prepare")
				}
				assertNothingStaged(t, s, sid)
			case "foreign-session-entry":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_hostile_foreign")
				foreign, err := schema.NewSessionID(c.ForeignSession)
				if err != nil {
					t.Fatal(err)
				}
				v2.Generation.Main.Entries[0].SessionID = foreign
				if err := activateHostile(t, s, v2, blobs); err == nil {
					t.Fatal("foreign-session entry succeeded; hostile input must stop in prepare")
				}
				assertNothingStaged(t, s, sid)
			case "duplicate-ref":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_hostile_dupref")
				v2.Generation.Main.Entries[c.DuplicateEntry].SourceEntryRef = v2.Generation.Main.Entries[c.DuplicateOf].SourceEntryRef
				if err := activateHostile(t, s, v2, blobs); err == nil {
					t.Fatal("duplicate source ref succeeded; one block cannot live in two partitions")
				}
				assertNothingStaged(t, s, sid)
			case "forged-full-claim":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				// An incomplete first-discovery candidate cannot certify full
				// content no matter what the caller claims: the store derives the
				// expected write from the same mapping the pipeline uses.
				v2, blobs := buildIncompleteGeneration(t, sid, "gen_hostile_forged", c.Texts[0], c.Texts[1], c.Texts[2])
				if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{
					Generation:     filledCandidateForValidation(t, v2, blobs),
					Blobs:          blobs,
					IndexerVersion: 1,
					IndexedAtMs:    1,
					ContentCapture: ingest.SessionContentCaptureWrite{
						Status: ingest.ContentCaptureComplete, SourceAuthority: ingest.ContentSourceNewIngest,
						TranscriptOrigin: ingest.TranscriptOriginFile, CaptureFormat: ingest.ContentCaptureFormatFull,
					},
				}); err == nil {
					t.Fatal("forged full claim succeeded; a caller-supplied full flag is not proof")
				}
				// The refusal lands at the commit, after staging: no generation
				// row exists, and the flag marks the staged orphans for the sweep.
				// A crash between staging and the commit leaves exactly this
				// state, and the next harvest recovers it.
				if rowPresent(t, s, sid, "gen_hostile_forged") {
					t.Fatal("refused activation wrote a generation row")
				}
				if !readSweepFlag(t, s, sid) {
					t.Fatal("refused activation after staging left the sweep flag unset")
				}
			case "pi-carrier-with-content-refused":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				v2, blobs := hostileBase(t, sid, "gen_hostile_carrier")
				carrier, err := ingest.NewPiCarrier(sid, len(v2.Generation.Main.Entries), mustDecodePiExtra(t))
				if err != nil {
					t.Fatal(err)
				}
				withContent := c.Content
				carrier.ContentPreview = &withContent
				carrier.SourceEntryRef = schema.SourceEntryRef("e_carrier_hostile")
				v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
				if err := activateHostile(t, s, v2, blobs); err == nil {
					t.Fatal("pi carrier with content succeeded; carriers carry no searchable content")
				}
				assertNothingStaged(t, s, sid)
			case "pi-carrier-refused-at-store-boundary":
				s, _ := openGenerationStore(t)
				seedGenerationSession(t, s, string(sid))
				// A well-formed carrier passes validation, so the store boundary
				// is what refuses one that smuggles content: the shared validator
				// runs over the same entries the ingest boundary saw.
				v2, _ := hostileBase(t, sid, "gen_hostile_boundary")
				carrier, err := ingest.NewPiCarrier(sid, len(v2.Generation.Main.Entries), mustDecodePiExtra(t))
				if err != nil {
					t.Fatal(err)
				}
				toolOutput := c.Content
				carrier.ToolOutput = &toolOutput
				carrier.SourceEntryRef = schema.SourceEntryRef("e_carrier_boundary")
				v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
				entries := append([]schema.SessionEntry(nil), v2.Generation.Main.Entries...)
				if err := validateEntriesForStorage(sid, entries); err == nil {
					t.Fatal("store boundary accepted a carrier with tool output")
				}
			default:
				t.Fatalf("unknown hostile input operation %q", c.Operation)
			}
		})
	}
}

// mustDecodePiExtra builds minimal typed Pi evidence for carrier tests.
func mustDecodePiExtra(t *testing.T) ingest.PiExtra {
	t.Helper()
	encoded, err := ingest.EncodePiExtra(ingest.PiExtra{Kind: ingest.PiExtraCarrier, Harness: schema.HarnessPi})
	if err != nil {
		t.Fatalf("encode test pi extra: %v", err)
	}
	extra, _, err := ingest.DecodePiExtra(encoded)
	if err != nil {
		t.Fatalf("decode test pi extra: %v", err)
	}
	return extra
}

func strPtr(text string) *string { return &text }
