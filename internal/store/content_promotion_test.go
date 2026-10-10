package store

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

type contentPromotionCase struct {
	Name      string `yaml:"name"`
	Mode      string `yaml:"mode"`
	Damage    string `yaml:"damage"`
	Term      string `yaml:"term"`
	StatsCase string `yaml:"statsCase"`
	PriorHex  string `yaml:"priorHex"`
	Records   []struct {
		Kind   string `yaml:"kind"`
		Source string `yaml:"source"`
		Data   string `yaml:"data"`
	} `yaml:"records"`
}

func loadContentPromotionFixture(t *testing.T) []contentPromotionCase {
	t.Helper()
	var fixture struct {
		Cases []contentPromotionCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(contentPromotionYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(contentPromotionManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		if c.Mode == "" {
			t.Fatalf("promotion fixture %s has no runner mode", c.Name)
		}
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "content promotion"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

func TestContentPromotion(t *testing.T) {
	for _, c := range loadContentPromotionFixture(t) {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Mode {
			case "rewrite":
				runPromotionRewrite(t, c)
			case "stats":
				found := false
				for _, stats := range LoadStatsOverflowFixtures(t) {
					if stats.Name == c.StatsCase {
						runStatsOverflowCase(t, stats)
						found = true
					}
				}
				if !found {
					t.Fatalf("promotion names absent stat fixture %q", c.StatsCase)
				}
			case "prior":
				runPromotionPrior(t, c)
			case "metadata":
				runPromotionMetadata(t, c)
			default:
				t.Fatalf("unknown promotion mode %q", c.Mode)
			}
		})
	}
}

func promotionMappings(t *testing.T, s *Store, sid schema.SessionID) []string {
	t.Helper()
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var rows []string
	if err := sqlitex.Execute(conn, `SELECT generation_id,partition_id,entry_index,source_entry_ref,body_digest FROM session_generation_entries WHERE session_id=? ORDER BY generation_id,partition_id,entry_index`, &sqlitex.ExecOptions{Args: []any{string(sid)}, ResultFunc: func(stmt *sqlite.Stmt) error {
		rows = append(rows, fmt.Sprintf("%s/%d/%d/%s/%s", stmt.ColumnText(0), stmt.ColumnInt64(1), stmt.ColumnInt64(2), stmt.ColumnText(3), stmt.ColumnText(4)))
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return rows
}

func promotionMatchCount(t *testing.T, s *Store, term string) int64 {
	t.Helper()
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	var count int64
	if err := sqlitex.Execute(conn, `SELECT count(*) FROM session_search_fts WHERE session_search_fts MATCH ?`, &sqlitex.ExecOptions{Args: []any{term}, ResultFunc: func(stmt *sqlite.Stmt) error {
		count = stmt.ColumnInt64(0)
		return nil
	}}); err != nil {
		t.Fatal(err)
	}
	return count
}

func runPromotionRewrite(t *testing.T, c contentPromotionCase) {
	t.Helper()
	s, sid, v2, blobs, identity, proof := seedCorruptionCandidate(t, c.Damage)
	before := contentBodyIDs(t, s, sid)
	mappings := promotionMappings(t, s, sid)
	matches := promotionMatchCount(t, s, c.Term)
	if matches == 0 {
		t.Fatal("repair fixture term matches no body")
	}
	applyCorruptionDamage(t, s, sid, c.Damage)
	if _, err := s.VerifyContent(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	v2.Generation.ID += "_rewrite"
	if _, err := s.ActivateGeneration(t.Context(), GenerationActivation{Generation: filledCandidateForValidation(t, v2, blobs), Blobs: blobs, PriorEvidence: nil, IndexerVersion: 1, IndexedAtMs: 200, ArtifactIdentity: &identity, IndexedInputHash: &proof}); err != nil {
		t.Fatal(err)
	}
	after := contentBodyIDs(t, s, sid)
	if len(after) != len(before) || !reflect.DeepEqual(mappings, promotionMappings(t, s, sid)) {
		t.Fatal("repair changed digest membership or generation mappings")
	}
	changed := 0
	for digest, id := range before {
		next, exists := after[digest]
		if !exists {
			t.Fatalf("repair lost body digest %s", digest)
		}
		if next != id {
			changed++
		}
	}
	if changed != len(before) {
		t.Fatalf("repair rewrote %d bodies; want the production rewrite of all %d bodies", changed, len(before))
	}
	if _, err := s.EnsureSearchIndexHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := promotionMatchCount(t, s, c.Term); got != matches {
		t.Fatalf("repair MATCH count = %d; want %d", got, matches)
	}
	conn, err := s.pool.Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(conn)
	if err := sqlitex.Execute(conn, `PRAGMA foreign_key_check`, &sqlitex.ExecOptions{ResultFunc: func(*sqlite.Stmt) error {
		return fmt.Errorf("repair left a foreign-key violation")
	}}); err != nil {
		t.Fatal(err)
	}
}

func assertPromotionPrior(t *testing.T, s *Store, sid schema.SessionID, expected []byte) {
	t.Helper()
	prior, err := s.ReadNativeGenerationPrior(t.Context(), sid)
	if err != nil || prior == nil {
		t.Fatalf("read prior evidence: %v", err)
	}
	if !bytes.Equal(prior.PriorEvidence, expected) {
		t.Fatalf("prior bytes = %x; want %x", prior.PriorEvidence, expected)
	}
}

func runPromotionPrior(t *testing.T, c contentPromotionCase) {
	t.Helper()
	evidence, err := hex.DecodeString(c.PriorHex)
	if err != nil || len(evidence) == 0 {
		t.Fatalf("invalid prior bytes: %v", err)
	}
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "b2b2b2b2-b2b2-42b2-82b2-b2b2b2b2b2b2")
	v2, blobs := buildTestGeneration(t, sid, "prior_binary", "prior text", "prior input", "prior output")
	if _, err := s.ActivateGeneration(t.Context(), GenerationActivation{Generation: v2, Blobs: blobs, PriorEvidence: evidence, IndexerVersion: 1, IndexedAtMs: 1}); err != nil {
		t.Fatal(err)
	}
	assertPromotionPrior(t, s, sid, evidence)
	legacy, root := openGenerationStore(t)
	legacyID := migrateCaseSessionID(t, c.Name, 0)
	seedGenerationSession(t, legacy, string(legacyID))
	seedMigrateProfile(t, legacy, root, legacyID, "clean", "prior_convert")
	if err := os.WriteFile(filepath.Join(root, string(legacyID), "generations", "prior_convert", priorEvidenceName), evidence, 0600); err != nil {
		t.Fatal(err)
	}
	if outcome, err := legacy.MigrateSession(t.Context(), legacyID); err != nil || outcome != MigrateOutcomeConverted {
		t.Fatalf("convert binary evidence: %s, %v", outcome, err)
	}
	assertPromotionPrior(t, legacy, legacyID, evidence)
}

func runPromotionMetadata(t *testing.T, c contentPromotionCase) {
	t.Helper()
	s, _ := openGenerationStore(t)
	sid := gcSession(t, s, "c2c2c2c2-c2c2-42c2-82c2-c2c2c2c2c2c2")
	v2 := benchV2InstallGeneration(sid, "metadata_preserved", len(c.Records))
	kinds := map[schema.NativeMetadataKind]bool{}
	for i, record := range c.Records {
		kind, err := schema.NewNativeMetadataKind(record.Kind)
		if err != nil {
			t.Fatal(err)
		}
		source, err := schema.NewNativeMetadataSourceType(record.Source)
		if err != nil {
			t.Fatal(err)
		}
		if kinds[kind] {
			t.Fatalf("duplicate metadata kind %s", kind)
		}
		kinds[kind] = true
		v2.Generation.Main.NativeMetadata = append(v2.Generation.Main.NativeMetadata, schema.NativeMetadataRecord{ID: fmt.Sprintf("n%d", i), Kind: kind, Source: schema.NativeSourceRef{EntryRef: v2.Generation.Main.Entries[i].SourceEntryRef, SourceType: source}, Data: json.RawMessage(record.Data)})
	}
	for _, kind := range schema.AllNativeMetadataKinds {
		if !kinds[kind] {
			t.Fatalf("metadata fixture omits published kind %s", kind)
		}
	}
	if err := activateTestGeneration(t, s, v2, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.WithSessionSnapshot(t.Context(), sid, func(snapshot indexformat.ReadSnapshot) error {
		records := snapshot.Main.NativeMetadata
		if !reflect.DeepEqual(records, v2.Generation.Main.NativeMetadata) {
			return fmt.Errorf("snapshot changed raw metadata: got %+v; want %+v", records, v2.Generation.Main.NativeMetadata)
		}
		for i, record := range records {
			raw, err := json.Marshal(record)
			if err != nil {
				return err
			}
			var wire struct {
				Data json.RawMessage `json:"data"`
			}
			if err := json.Unmarshal(raw, &wire); err != nil {
				return err
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(c.Records[i].Data)); err != nil {
				return err
			}
			if !bytes.Equal(wire.Data, compact.Bytes()) {
				return fmt.Errorf("wire metadata did not compact %s", record.Kind)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
