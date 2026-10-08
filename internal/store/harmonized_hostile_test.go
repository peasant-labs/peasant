package store

import (
	"context"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/schema"
)

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

	t.Run("malformed-record", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := hostileBase(t, sid, "gen_hostile_malformed")
		broken := "{not an entry extra}"
		v2.Generation.Main.Entries[0].Extra = &broken
		if err := activateHostile(t, s, v2, blobs); err == nil {
			t.Fatal("malformed extra succeeded; hostile input must stop in prepare")
		}
		assertNothingStaged(t, s, sid)
	})

	t.Run("oversized-record-placeholder", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		// The store carries no size gate: a large entry stages like any
		// other, and its digest still anchors its exact bytes. The record
		// size limit and its placeholder live in ingest, above this
		// boundary; what reaches the store is ordinary data.
		v2, blobs := hostileBase(t, sid, "gen_hostile_large")
		large := strings.Repeat("L", 262144)
		v2.Generation.Main.Entries[0].ContentPreview = &large
		blobs[schema.SourceEntryRef("e_u1")] = []byte(large)
		if err := activateHostile(t, s, filledCandidateForValidation(t, v2, blobs), blobs); err != nil {
			t.Fatalf("large entry refused: %v", err)
		}
	})

	t.Run("invalid-utf8", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := hostileBase(t, sid, "gen_hostile_utf8")
		broken := "valid prefix\xff\xfe invalid suffix"
		v2.Generation.Main.Entries[0].ContentPreview = &broken
		blobs[schema.SourceEntryRef("e_u1")] = []byte(broken)
		if err := activateHostile(t, s, v2, blobs); err == nil {
			t.Fatal("invalid UTF-8 succeeded; hostile input must stop in prepare")
		}
		assertNothingStaged(t, s, sid)
	})

	t.Run("foreign-session-entry", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := hostileBase(t, sid, "gen_hostile_foreign")
		foreign, err := schema.NewSessionID("ffffffff-ffff-4fff-8fff-ffffffffffff")
		if err != nil {
			t.Fatal(err)
		}
		v2.Generation.Main.Entries[0].SessionID = foreign
		if err := activateHostile(t, s, v2, blobs); err == nil {
			t.Fatal("foreign-session entry succeeded; hostile input must stop in prepare")
		}
		assertNothingStaged(t, s, sid)
	})

	t.Run("duplicate-ref", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := hostileBase(t, sid, "gen_hostile_dupref")
		v2.Generation.Main.Entries[1].SourceEntryRef = v2.Generation.Main.Entries[0].SourceEntryRef
		if err := activateHostile(t, s, v2, blobs); err == nil {
			t.Fatal("duplicate source ref succeeded; one block cannot live in two partitions")
		}
		assertNothingStaged(t, s, sid)
	})

	t.Run("forged-full-claim", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		// An incomplete first-discovery candidate cannot certify full
		// content no matter what the caller claims: the store derives the
		// expected write from the same mapping the pipeline uses.
		v2, blobs := buildIncompleteGeneration(t, sid, "gen_hostile_forged", "hostile preview", "hostile preview in", "hostile preview out")
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
	})

	t.Run("pi-carrier-with-content-refused", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		v2, blobs := hostileBase(t, sid, "gen_hostile_carrier")
		carrier, err := ingest.NewPiCarrier(sid, len(v2.Generation.Main.Entries), mustDecodePiExtra(t))
		if err != nil {
			t.Fatal(err)
		}
		withContent := "smuggled content"
		carrier.ContentPreview = &withContent
		carrier.SourceEntryRef = schema.SourceEntryRef("e_carrier_hostile")
		v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
		if err := activateHostile(t, s, v2, blobs); err == nil {
			t.Fatal("pi carrier with content succeeded; carriers carry no searchable content")
		}
		assertNothingStaged(t, s, sid)
	})

	t.Run("pi-carrier-refused-at-store-boundary", func(t *testing.T) {
		s, _ := openGenerationStore(t)
		seedGenerationSession(t, s, string(sid))
		// A well-formed carrier passes validation, so the store boundary
		// is what refuses one that smuggles content: the shared validator
		// runs at S0 over the same entries P1 saw.
		v2, _ := hostileBase(t, sid, "gen_hostile_boundary")
		carrier, err := ingest.NewPiCarrier(sid, len(v2.Generation.Main.Entries), mustDecodePiExtra(t))
		if err != nil {
			t.Fatal(err)
		}
		toolOutput := "smuggled tool output"
		carrier.ToolOutput = &toolOutput
		carrier.SourceEntryRef = schema.SourceEntryRef("e_carrier_boundary")
		v2.Generation.Main.Entries = append(v2.Generation.Main.Entries, carrier)
		entries := append([]schema.SessionEntry(nil), v2.Generation.Main.Entries...)
		if err := validateEntriesForStorage(sid, entries); err == nil {
			t.Fatal("store boundary accepted a carrier with tool output")
		}
	})
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
