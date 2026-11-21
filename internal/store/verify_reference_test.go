package store_test

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/verify_reference.yaml
var verifyReferenceYAML []byte

//go:embed testdata/verify_reference.manifest.yaml
var verifyReferenceManifestYAML []byte

type verifyReferenceCase struct {
	Name  string `yaml:"name"`
	Given string `yaml:"given"`
	When  string `yaml:"when"`
	Then  string `yaml:"then"`
	Kind  string `yaml:"kind"`
}

func loadVerifyReferenceCases(t *testing.T) []verifyReferenceCase {
	t.Helper()
	var fixture struct {
		Cases []verifyReferenceCase `yaml:"cases"`
	}
	decodeStrictYAML(t, verifyReferenceYAML, &fixture, "verify reference fixture")
	var manifest struct {
		Required []string `yaml:"required"`
	}
	decodeStrictYAML(t, verifyReferenceManifestYAML, &manifest, "verify reference manifest")
	names := make([]string, 0, len(fixture.Cases))
	for _, c := range fixture.Cases {
		stated := c.Given != "" && c.When != "" && c.Then != ""
		if c.Name == "" || slices.Contains(names, c.Name) || !stated || c.Kind == "" {
			t.Fatalf("verify reference case %q is unnamed, duplicated, or missing given/when/then/kind", c.Name)
		}
		names = append(names, c.Name)
	}
	for _, req := range manifest.Required {
		if !slices.Contains(names, req) {
			t.Fatalf("required verify reference case %q is missing", req)
		}
	}
	return fixture.Cases
}

func TestVerifyContentReferenceGeneration(t *testing.T) {
	refFixture := loadGenerationReferenceFixture(t)
	cases := loadVerifyReferenceCases(t)
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Kind {
			case "damaged-body":
				w := newReferenceWorld(t, refFixture.RenamedProject)
				w.install(t, w.renamed)
				db, sid := w.seeded.db, w.seeded.sid

				rows := referenceQuery(t, db, `SELECT m.body_digest FROM session_generation_entries m WHERE m.session_id = ? AND m.generation_id = ? ORDER BY m.partition_id, m.entry_index LIMIT 1`, string(sid), w.corpus.GenerationID)
				if len(rows) != 1 {
					t.Fatalf("owner body count = %d, want 1", len(rows))
				}
				digest := strings.SplitN(rows[0], ":", 2)[1]

				// Corrupt content_preview under its body_digest on the owner's body row.
				conn, err := db.PoolForTest().Take(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if err := sqlitex.Execute(conn, `DROP TRIGGER session_entry_bodies_immutable`, nil); err != nil {
					db.PoolForTest().Put(conn)
					t.Fatalf("drop immutability trigger: %v", err)
				}
				if err := sqlitex.Execute(conn, `UPDATE session_entry_bodies SET content_preview = 'corrupted preview under digest' WHERE session_id = ? AND body_digest = ?`, &sqlitex.ExecOptions{
					Args: []any{string(sid), digest},
				}); err != nil {
					db.PoolForTest().Put(conn)
					t.Fatalf("corrupt owner body: %v", err)
				}
				if err := sqlitex.Execute(conn, `CREATE TRIGGER session_entry_bodies_immutable BEFORE UPDATE ON session_entry_bodies BEGIN SELECT RAISE(ABORT, 'session_entry_bodies rows are immutable; insert a new entry instead'); END`, nil); err != nil {
					db.PoolForTest().Put(conn)
					t.Fatalf("recreate immutability trigger: %v", err)
				}
				db.PoolForTest().Put(conn)

				report, err := db.VerifyContent(t.Context(), true)
				if err != nil {
					t.Fatalf("VerifyContent(repair=true): %v", err)
				}
				if len(report.Damaged) != 1 || report.Damaged[0].SessionID != sid || report.Damaged[0].Object != "body:"+digest {
					t.Fatalf("report.Damaged = %+v, want session %s object body:%s", report.Damaged, sid, digest)
				}
				if !slices.Contains(report.Repaired, sid) {
					t.Fatalf("report.Repaired = %v, want %s", report.Repaired, sid)
				}
				hashRows := referenceQuery(t, db, `SELECT indexed_input_hash FROM sessions WHERE session_id = ?`, string(sid))
				if want := []string{"5:"}; !slices.Equal(hashRows, want) {
					t.Fatalf("indexed_input_hash = %v, want NULL (%v) so a later harvest repairs it", hashRows, want)
				}

			case "damaged-blob":
				w := newReferenceWorld(t, refFixture.RenamedProject)
				db, sid := w.seeded.db, w.seeded.sid

				// Seed an owner with a content blob, then install a reference generation.
				cand, err := w.seeded.builder.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
					Session: w.seeded.session, GenerationID: "G_blob_owner", RecordedMetadata: &w.seeded.recorded,
					CapturedTranscript: w.seeded.transcript, Allocator: &countingRefAllocator{},
				})
				if err != nil {
					t.Fatalf("build candidate: %v", err)
				}
				v2 := cand.Result
				blobs := maps.Clone(cand.Blobs)
				if blobs == nil {
					blobs = map[schema.SourceEntryRef][]byte{}
				}
				blobRef := schema.SourceEntryRef("e_verify_blob_0")
				payload := []byte("synthetic blob payload for verify reference test")
				sum := sha256.Sum256(payload)
				blobDigest := hex.EncodeToString(sum[:])
				blobs[blobRef] = payload
				v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{
					Ref:          blobRef,
					RelativeBlob: "c_" + blobDigest + ".blob",
					ByteLength:   int64(len(payload)),
					Digest:       blobDigest,
				})
				v2.Generation.Segments = append(v2.Generation.Segments, indexformat.ContextSegment{
					Ordinal:          len(v2.Generation.Segments),
					PhysicalSourceID: "source-0",
					Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
					Inclusion:        indexformat.SegmentInclusionInherited,
					CapturedRefs:     []schema.SourceEntryRef{blobRef},
				})
				if v2.Generation.Metadata.AdapterVersion == nil {
					v := w.seeded.target.AdapterVersion
					v2.Generation.Metadata.AdapterVersion = &v
				}
				assessment, err := ingest.AssessCapture(ingest.CaptureFacts{Harness: w.seeded.harness, Result: v2, Policy: ingest.CaptureFreshCandidate, Authoritative: true})
				if err != nil {
					t.Fatal(err)
				}
				capture, err := assessment.ContentCapture(ingest.ContentSourceProviderSource, w.seeded.session.TranscriptOrigin, w.corpus.ClockMs)
				if err != nil {
					t.Fatal(err)
				}
				state, err := db.ReadIndexState(t.Context(), sid)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.ActivateGeneration(t.Context(), store.GenerationActivation{
					Generation: v2, Blobs: blobs,
					IndexerVersion: w.seeded.target.IndexerVersion, IndexedAtMs: w.corpus.ClockMs,
					ContentCapture: capture, ExpectedState: state, IndexedInputHash: &referenceInputHash,
				}); err != nil {
					t.Fatalf("activate blob owner: %v", err)
				}

				state, err = db.ReadIndexState(t.Context(), sid)
				if err != nil || state == nil {
					t.Fatalf("read index state: %+v %v", state, err)
				}
				candMeta := w.renamed
				if candMeta.AdapterVersion == nil {
					v := w.seeded.target.AdapterVersion
					candMeta.AdapterVersion = &v
				}
				plan, err := db.PlanManagedGenerationReference(t.Context(), ingest.ManagedGenerationReferenceRequest{
					Proof: ingest.ManagedGenerationUnchangedProof{
						SessionID: sid, Metadata: candMeta, InputDigest: referenceInputHash,
						IndexerTarget: w.seeded.target.IndexerVersion, FormatTarget: w.seeded.target.IndexVersion, Expected: state,
					},
					Candidate: ingest.GenerationMetadataCandidate{
						Metadata: candMeta, Completeness: v2.Generation.Completeness,
						SourceEvidenceDigest: v2.Generation.SourceEvidenceDigest,
					},
					GenerationID: referenceGenerationID,
				})
				if err != nil {
					t.Fatalf("plan reference: %v", err)
				}
				if _, err := db.InstallManagedGenerationReference(t.Context(), plan); err != nil {
					t.Fatalf("install reference: %v", err)
				}

				// Flip one byte in the owner's blob chunk.
				referenceExec(t, db, `UPDATE session_content_chunks SET data = zeroblob(length(data)) WHERE session_id = ? AND digest = ? AND chunk_index = 0`, string(sid), blobDigest)

				report, err := db.VerifyContent(t.Context(), true)
				if err != nil {
					t.Fatalf("VerifyContent(repair=true): %v", err)
				}
				if len(report.Damaged) != 1 || report.Damaged[0].SessionID != sid || report.Damaged[0].Object != "blob:"+blobDigest {
					t.Fatalf("report.Damaged = %+v, want session %s object blob:%s", report.Damaged, sid, blobDigest)
				}
				if !slices.Contains(report.Repaired, sid) {
					t.Fatalf("report.Repaired = %v, want %s", report.Repaired, sid)
				}
				hashRows := referenceQuery(t, db, `SELECT indexed_input_hash FROM sessions WHERE session_id = ?`, string(sid))
				if want := []string{"5:"}; !slices.Equal(hashRows, want) {
					t.Fatalf("indexed_input_hash = %v, want NULL (%v) so a later harvest repairs it", hashRows, want)
				}

			case "readers-parity":
				w := newReferenceWorld(t, refFixture.RenamedProject)
				w.install(t, w.renamed)
				control := seedReaderEntrypointStoreWithInput(t, w.corpus, w.h, referenceInputHash)
				if out, err := control.parse(t, w.renamed, referenceGenerationID, referenceInputHash); err != nil || out.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("control parse: %+v %v", out, err)
				}
				refDB, ctrlDB := w.seeded.db, control.db
				sid := w.seeded.sid

				// 1. CreateEntryAnnotation (sqlInsertTargetEntry + selectShimAnchorEntryForSpanSQL).
				createAnn := func(targetDB *store.Store) string {
					t.Helper()
					annID, err := targetDB.GetAnnotatorIDByName(t.Context(), "frustration-classifier")
					if err != nil || annID == "" {
						t.Fatalf("annotator: id=%q err=%v", annID, err)
					}
					typeID, err := targetDB.GetAnnotationTypeID(t.Context(), "quality.frustration_signal")
					if err != nil || typeID == "" {
						t.Fatalf("annotation type: id=%q err=%v", typeID, err)
					}
					id, err := targetDB.CreateEntryAnnotation(t.Context(), ingest.EntryAnnotationParams{
						SessionID: string(sid), EntryIndex: 0, EndIndex: 1,
						AnnotatorID: annID, AnnotationTypeID: typeID, Value: "detected",
					})
					if err != nil {
						t.Fatalf("CreateEntryAnnotation on %s: %v", sid, err)
					}
					return id
				}
				refAnn := createAnn(refDB)
				ctrlAnn := createAnn(ctrlDB)
				if refAnn == "" || ctrlAnn == "" {
					t.Fatalf("empty annotation id: ref=%q ctrl=%q", refAnn, ctrlAnn)
				}

				// 2. ListUserPrompts (userPromptBodiesSQL).
				refPrompts, err := refDB.ListUserPrompts(t.Context(), sid)
				if err != nil {
					t.Fatalf("ref ListUserPrompts: %v", err)
				}
				ctrlPrompts, err := ctrlDB.ListUserPrompts(t.Context(), sid)
				if err != nil {
					t.Fatalf("ctrl ListUserPrompts: %v", err)
				}
				if len(ctrlPrompts) == 0 || !reflect.DeepEqual(refPrompts, ctrlPrompts) {
					t.Fatalf("ListUserPrompts mismatch:\nref  %#v\nctrl %#v", refPrompts, ctrlPrompts)
				}

				// 3. SessionEntriesExist (shimEntriesExistSQL + indexFormatStateSQL).
				refExists, err := refDB.SessionEntriesExist(t.Context(), ingest.SessionID(sid))
				if err != nil || !refExists {
					t.Fatalf("ref SessionEntriesExist = (%v, %v), want (true, nil)", refExists, err)
				}
				ctrlExists, err := ctrlDB.SessionEntriesExist(t.Context(), ingest.SessionID(sid))
				if err != nil || refExists != ctrlExists {
					t.Fatalf("SessionEntriesExist ref=%v ctrl=%v (%v)", refExists, ctrlExists, err)
				}

				// 4. SessionsWithoutEntries (readSessionsWithoutEntriesChunk).
				refWithout, err := refDB.SessionsWithoutEntries(t.Context(), []ingest.SessionID{ingest.SessionID(sid)})
				if err != nil {
					t.Fatalf("ref SessionsWithoutEntries: %v", err)
				}
				ctrlWithout, err := ctrlDB.SessionsWithoutEntries(t.Context(), []ingest.SessionID{ingest.SessionID(sid)})
				if err != nil {
					t.Fatalf("ctrl SessionsWithoutEntries: %v", err)
				}
				if refWithout[ingest.SessionID(sid)] || !reflect.DeepEqual(refWithout, ctrlWithout) {
					t.Fatalf("SessionsWithoutEntries ref=%v ctrl=%v", refWithout, ctrlWithout)
				}

				// 5. ListSessionsFiltered with Facets (sqlSessionNativeEntries).
				for _, facet := range defaults.AllSessionWorkflowFacets {
					filter := store.SessionListFilter{Facets: []defaults.SessionWorkflowFacet{facet}}
					refRows, err := refDB.ListSessionsFiltered(t.Context(), filter)
					if err != nil {
						t.Fatalf("ref ListSessionsFiltered(%s): %v", facet, err)
					}
					ctrlRows, err := ctrlDB.ListSessionsFiltered(t.Context(), filter)
					if err != nil {
						t.Fatalf("ctrl ListSessionsFiltered(%s): %v", facet, err)
					}
					if len(refRows) != len(ctrlRows) {
						t.Fatalf("ListSessionsFiltered(%s) count ref=%d ctrl=%d", facet, len(refRows), len(ctrlRows))
					}
				}

				// 6. SearchMergedOnConn (unifiedSearchSQLTemplate).
				searchOn := func(targetDB *store.Store, term string) []store.UnifiedSearchHit {
					t.Helper()
					conn, err := targetDB.PoolForTest().Take(t.Context())
					if err != nil {
						t.Fatal(err)
					}
					defer targetDB.PoolForTest().Put(conn)
					hits, err := store.SearchMergedOnConn(conn, term, 20, 0, store.SearchFilters{})
					if err != nil {
						t.Fatalf("SearchMergedOnConn(%q): %v", term, err)
					}
					return hits
				}
				// Use a word from the first user prompt.
				fields := strings.Fields(ctrlPrompts[0].Text)
				if len(fields) == 0 {
					t.Fatalf("first user prompt has no fields: %q", ctrlPrompts[0].Text)
				}
				term := strings.Trim(fields[0], ".,!?:;\"'")
				refHits := searchOn(refDB, term)
				ctrlHits := searchOn(ctrlDB, term)
				if len(ctrlHits) == 0 || len(refHits) != len(ctrlHits) {
					t.Fatalf("SearchMergedOnConn(%q) hits ref=%d ctrl=%d", term, len(refHits), len(ctrlHits))
				}
				for i := range ctrlHits {
					if refHits[i].SessionID != ctrlHits[i].SessionID || refHits[i].EntryIndex != ctrlHits[i].EntryIndex {
						t.Fatalf("SearchMergedOnConn(%q)[%d] ref=%+v ctrl=%+v", term, i, refHits[i], ctrlHits[i])
					}
				}
			default:
				t.Fatalf("unknown verify reference case kind %q", c.Kind)
			}
		})
	}
}
