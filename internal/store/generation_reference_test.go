package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/generation_reference.yaml
var generationReferenceYAML []byte

//go:embed testdata/generation_reference_refusal.yaml
var generationReferenceRefusalYAML []byte

// The identities the reference cases install.
const (
	referenceGenerationID = "G_reference"
	referenceLaterID      = "G_later"
	referenceOtherID      = "G_other"
)

// referenceInputHash is the consumed-input digest the reference cases record
// on the seeded generation; the cheap proof and the plan compare against it.
var referenceInputHash = strings.Repeat("d", 64)

// The kinds of install case.
const (
	referenceKindInstall        = "install"
	referenceKindRowDeltas      = "row-deltas"
	referenceKindRelationship   = "relationship"
	referenceKindParseParity    = "parse-parity"
	referenceKindLaterParse     = "later-parse"
	referenceKindCrash          = "crash"
	referenceKindMoved          = "moved"
	referenceKindAnnotatedParse = "annotated-parse"
	referenceKindChained        = "chained-reference"
	referenceKindBlobPartParity = "blobs-and-partitions-parity"
)

// The changes a refusal case applies.
const (
	refusalIndexerTargetAhead     = "indexer-target-ahead"
	refusalSourceEvidenceChanged  = "source-evidence-changed"
	refusalContextUnchanged       = "context-unchanged"
	refusalParseInputField        = "parse-input-field"
	refusalStatsRowMissing        = "stats-row-missing"
	refusalSourceSealForged       = "source-seal-forged"
	refusalLatestAttemptHeld      = "latest-attempt-held"
	refusalIndexStateMoved        = "index-state-moved"
	refusalWherePlan              = "plan"
	refusalWhereInstall           = "install"
	referenceParseInputFieldValue = "project.name"
)

type generationReferenceCase struct {
	Name                 string `yaml:"name"`
	Given                string `yaml:"given"`
	When                 string `yaml:"when"`
	Then                 string `yaml:"then"`
	Kind                 string `yaml:"kind"`
	Parent               string `yaml:"parent"`
	AppendedLine         string `yaml:"appendedLine"`
	SecondRenamedProject string `yaml:"secondRenamedProject"`
}

type generationReferenceFixture struct {
	RenamedProject string                    `yaml:"renamedProject"`
	RequiredNames  []string                  `yaml:"requiredNames"`
	Cases          []generationReferenceCase `yaml:"cases"`
}

type generationReferenceRefusalCase struct {
	Name   string `yaml:"name"`
	Reason string `yaml:"reason"`
	Change string `yaml:"change"`
	Where  string `yaml:"where"`
}

func loadGenerationReferenceFixture(t *testing.T) generationReferenceFixture {
	t.Helper()
	var fixture generationReferenceFixture
	decodeStrictYAML(t, generationReferenceYAML, &fixture, "generation reference fixture")
	var names []string
	for _, c := range fixture.Cases {
		stated := c.Given != "" && c.When != "" && c.Then != ""
		stated = stated && c.Kind != ""
		if c.Name == "" || slices.Contains(names, c.Name) || !stated {
			t.Fatalf("reference case %q is unnamed, duplicated, or misses its given, when, then or kind", c.Name)
		}
		names = append(names, c.Name)
	}
	if got, want := slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(fixture.RequiredNames)); !slices.Equal(got, want) || fixture.RenamedProject == "" {
		t.Fatalf("reference cases %v differ from the required names %v, or no renamed project is set", got, want)
	}
	return fixture
}

func loadGenerationReferenceRefusals(t *testing.T) []generationReferenceRefusalCase {
	t.Helper()
	var fixture struct {
		RequiredNames []string                         `yaml:"requiredNames"`
		Cases         []generationReferenceRefusalCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(generationReferenceRefusalYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode the reference refusal fixture: %v", err)
	}
	var names, reasons []string
	for _, c := range fixture.Cases {
		if _, err := ingest.NewReferenceRefusalReason(c.Reason); err != nil {
			t.Fatalf("refusal case %s: %v", c.Name, err)
		}
		names = append(names, c.Name)
		reasons = append(reasons, c.Reason)
	}
	if got, want := slices.Sorted(slices.Values(names)), slices.Sorted(slices.Values(fixture.RequiredNames)); !slices.Equal(got, want) {
		t.Fatalf("refusal cases %v differ from the required names %v", got, want)
	}
	var all []string
	for _, reason := range ingest.AllReferenceRefusalReasons() {
		all = append(all, string(reason))
	}
	if got := slices.Sorted(slices.Values(reasons)); !slices.Equal(got, slices.Sorted(slices.Values(all))) {
		t.Fatalf("refusal cases cover reasons %v, want exactly one case per reason %v", got, all)
	}
	return fixture.Cases
}

// referenceWorld is the Claude corpus session parsed and activated with its
// input proof, and the renamed metadata a context change captures.
type referenceWorld struct {
	corpus  readerEntrypointCorpus
	h       readerEntrypointHarness
	seeded  readerEntrypointStore
	renamed schema.UnifiedMetadata
}

func newReferenceWorld(t *testing.T, renamedProject string) *referenceWorld {
	t.Helper()
	corpus, _ := loadReaderEntrypoints(t)
	var claude readerEntrypointHarness
	for _, h := range corpus.Harnesses {
		if h.Harness == string(schema.HarnessClaudeCode) {
			claude = h
		}
	}
	w := &referenceWorld{corpus: corpus, h: claude}
	w.seeded = seedReaderEntrypointStoreWithInput(t, corpus, claude, referenceInputHash)
	w.renamed = w.seeded.recorded
	w.renamed.Project.Name = renamedProject
	w.renamed.MetadataHash = schema.ComputeMetadataHash(&w.renamed)
	return w
}

// request builds what the pipeline sends after a context mismatch: the
// captured proof and the metadata half built from recorded.
func (w *referenceWorld) request(t *testing.T, recorded schema.UnifiedMetadata) ingest.ManagedGenerationReferenceRequest {
	t.Helper()
	ctx := t.Context()
	state, err := w.seeded.db.ReadIndexState(ctx, w.seeded.sid)
	if err != nil || state == nil {
		t.Fatalf("read the index state: %+v %v", state, err)
	}
	half, ok := w.seeded.builder.(ingest.GenerationMetadataBuilder)
	if !ok {
		t.Fatal("the Claude builder has no metadata half")
	}
	candidate, err := half.BuildGenerationMetadata(ctx, ingest.GenerationBuildInput{
		Session: w.seeded.session, GenerationID: referenceGenerationID, RecordedMetadata: &recorded,
		CapturedTranscript: w.seeded.transcript,
	})
	if err != nil {
		t.Fatalf("build the metadata half: %v", err)
	}
	return ingest.ManagedGenerationReferenceRequest{
		Proof: ingest.ManagedGenerationUnchangedProof{
			SessionID: w.seeded.sid, Metadata: recorded, InputDigest: referenceInputHash,
			IndexerTarget: w.seeded.target.IndexerVersion, FormatTarget: w.seeded.target.IndexVersion, Expected: state,
		},
		Candidate:    candidate,
		GenerationID: referenceGenerationID,
	}
}

// install plans and installs the reference for recorded.
func (w *referenceWorld) install(t *testing.T, recorded schema.UnifiedMetadata) ingest.ManagedGenerationReferenceOutcome {
	t.Helper()
	plan, err := w.seeded.db.PlanManagedGenerationReference(t.Context(), w.request(t, recorded))
	if err != nil {
		t.Fatalf("plan the reference: %v", err)
	}
	outcome, err := w.seeded.db.InstallManagedGenerationReference(t.Context(), plan)
	if err != nil {
		t.Fatalf("install the reference: %v", err)
	}
	w.seeded.activeID = referenceGenerationID
	return outcome
}

func referenceQuery(t *testing.T, db *store.Store, query string, args ...any) []string {
	t.Helper()
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	var rows []string
	if err := sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args, ResultFunc: func(stmt *sqlite.Stmt) error {
		var cells []string
		for i := range stmt.ColumnCount() {
			cells = append(cells, fmt.Sprintf("%d:%s", stmt.ColumnType(i), stmt.ColumnText(i)))
		}
		rows = append(rows, strings.Join(cells, "|"))
		return nil
	}}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return rows
}

func referenceExec(t *testing.T, db *store.Store, query string, args ...any) {
	t.Helper()
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	if err := sqlitex.ExecuteTransient(conn, query, &sqlitex.ExecOptions{Args: args}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// referenceRowCounts counts the session's rows in every generation-scoped
// table and the stats table, per generation where the table has one.
func referenceRowCounts(t *testing.T, db *store.Store, sid schema.SessionID) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, table := range store.GenerationScopedTablesForTest() {
		for _, row := range referenceQuery(t, db, `SELECT generation_id, COUNT(*) FROM `+table+` WHERE session_id = ? GROUP BY generation_id`, string(sid)) {
			cells := strings.Split(row, "|")
			var n int
			fmt.Sscan(strings.SplitN(cells[1], ":", 2)[1], &n)
			counts[table+"@"+strings.SplitN(cells[0], ":", 2)[1]] = n
		}
	}
	counts["session_captured_stats"] = len(referenceQuery(t, db, `SELECT 1 FROM session_captured_stats WHERE session_id = ?`, string(sid)))
	return counts
}

// referenceStoreState is every row a refusal must leave unchanged: the
// generation-scoped rows, the session row and the stats row.
func referenceStoreState(t *testing.T, db *store.Store, sid schema.SessionID) []string {
	t.Helper()
	var state []string
	for _, table := range append(store.GenerationScopedTablesForTest(), "sessions", "session_captured_stats", "session_metrics") {
		for _, row := range referenceQuery(t, db, `SELECT * FROM `+table+` WHERE session_id = ?`, string(sid)) {
			state = append(state, table+": "+row)
		}
	}
	slices.Sort(state)
	return state
}

func TestGenerationReference(t *testing.T) {
	fixture := loadGenerationReferenceFixture(t)
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			w := newReferenceWorld(t, fixture.RenamedProject)
			db, sid := w.seeded.db, w.seeded.sid
			switch c.Kind {
			case referenceKindInstall:
				statsBefore := referenceQuery(t, db, `SELECT turn_count, conversation_turn_count, input_submission_count FROM session_captured_stats WHERE session_id = ?`, string(sid))
				flagBefore := referenceQuery(t, db, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(sid))
				outcome := w.install(t, w.renamed)
				if outcome.GenerationID != referenceGenerationID || outcome.OwnerGenerationID != w.corpus.GenerationID {
					t.Fatalf("outcome %+v, want %s reading %s", outcome, referenceGenerationID, w.corpus.GenerationID)
				}
				row := referenceQuery(t, db, `SELECT mapping_set_id, project_name FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), referenceGenerationID)
				if want := []string{"3:" + w.corpus.GenerationID + "|3:" + fixture.RenamedProject}; !slices.Equal(row, want) {
					t.Fatalf("reference row %v, want %v", row, want)
				}
				for _, table := range store.ParseTablesForTest() {
					if rows := referenceQuery(t, db, `SELECT 1 FROM `+table+` WHERE session_id = ? AND generation_id = ?`, string(sid), referenceGenerationID); len(rows) != 0 {
						t.Errorf("the reference wrote %d rows into parse table %s", len(rows), table)
					}
				}
				view, err := db.ReadActiveHarmonizedViewForTest(t.Context(), sid)
				if err != nil || view.GenerationID != referenceGenerationID || len(view.Mapping) == 0 {
					t.Fatalf("active view %s with %d mapped entries (%v); want the reference reading the owner's entries", view.GenerationID, len(view.Mapping), err)
				}
				if after := referenceQuery(t, db, `SELECT turn_count, conversation_turn_count, input_submission_count FROM session_captured_stats WHERE session_id = ?`, string(sid)); !slices.Equal(after, statsBefore) {
					t.Errorf("parse counts %v, want the stored %v carried", after, statsBefore)
				}
				if flag := referenceQuery(t, db, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(sid)); !slices.Equal(flag, flagBefore) {
					t.Errorf("sweep flag %v, want it unchanged at %v: the owner stays live, so nothing became collectable", flag, flagBefore)
				}
			case referenceKindRowDeltas:
				before := referenceRowCounts(t, db, sid)
				w.install(t, w.renamed)
				after := referenceRowCounts(t, db, sid)
				for key, n := range after {
					table, generation, _ := strings.Cut(key, "@")
					if before[key] == n {
						continue
					}
					if generation != referenceGenerationID || slices.Contains(store.ParseTablesForTest(), table) || before[key] != 0 {
						t.Errorf("%s changed from %d to %d rows; only the new generation's catalog and context rows may appear", key, before[key], n)
					}
				}
				if after["session_generations@"+referenceGenerationID] != 1 {
					t.Errorf("the reference wrote %d catalog rows, want 1", after["session_generations@"+referenceGenerationID])
				}
				for key, n := range before {
					if after[key] != n {
						t.Errorf("%s changed from %d to %d rows", key, n, after[key])
					}
				}
			case referenceKindRelationship:
				recorded := w.renamed
				parent := schema.SessionID(c.Parent)
				recorded.Relationships = append(slices.Clone(recorded.Relationships), schema.SessionRelationship{
					Kind: schema.SessionRelationshipStartedBy, TargetState: schema.RelationshipTargetKnown,
					TargetLocalID: &parent, Evidence: schema.EvidenceExistingAdapter,
				})
				recorded.MetadataHash = schema.ComputeMetadataHash(&recorded)
				ownerBefore := referenceQuery(t, db, `SELECT * FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ?`, string(sid), w.corpus.GenerationID)
				request := w.request(t, recorded)
				if !slices.ContainsFunc(request.Candidate.Metadata.Relationships, func(r schema.SessionRelationship) bool {
					return r.TargetLocalID != nil && *r.TargetLocalID == parent
				}) {
					t.Fatalf("the metadata half dropped the recorded started_by parent: %+v", request.Candidate.Metadata.Relationships)
				}
				w.install(t, recorded)
				rows := referenceQuery(t, db, `SELECT kind, target_local_id FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ?`, string(sid), referenceGenerationID)
				if !slices.ContainsFunc(rows, func(row string) bool { return strings.Contains(row, string(parent)) }) {
					t.Fatalf("the reference's relationship rows %v do not name the recorded parent %s", rows, parent)
				}
				if after := referenceQuery(t, db, `SELECT * FROM session_relationship_evidence WHERE session_id = ? AND generation_id = ?`, string(sid), w.corpus.GenerationID); !slices.Equal(after, ownerBefore) {
					t.Fatalf("the owner's relationship rows changed: %v -> %v", ownerBefore, after)
				}
			case referenceKindParseParity:
				twin := seedReaderEntrypointStoreWithInput(t, w.corpus, w.h, referenceInputHash)
				if outcome, err := twin.parse(t, w.renamed, referenceGenerationID, referenceInputHash); err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("parse the renamed input in the twin: %+v %v", outcome, err)
				}
				twin.activeID = referenceGenerationID
				w.install(t, w.renamed)
				sealQuery := `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`
				parsed, referenced := referenceQuery(t, twin.db, sealQuery, string(sid), referenceGenerationID), referenceQuery(t, db, sealQuery, string(sid), referenceGenerationID)
				if len(parsed) != 1 || !slices.Equal(parsed, referenced) {
					t.Fatalf("parse seal %v, reference seal %v; want one equal seal", parsed, referenced)
				}
				want, _ := readReaderEntrypoints(t, w.corpus, twin)
				got, _ := readReaderEntrypoints(t, w.corpus, w.seeded)
				for _, entry := range slices.Sorted(maps.Keys(want)) {
					if len(want[entry]) == 0 || !bytes.Equal(want[entry], got[entry]) {
						t.Errorf("%s differs between the parse and the reference:\nparse     %s\nreference %s", entry, want[entry], got[entry])
					}
				}
			case referenceKindLaterParse:
				w.install(t, w.renamed)
				outcome, err := w.seeded.parse(t, w.renamed, referenceLaterID, referenceInputHash)
				if err != nil || outcome.Disposition != ingest.ActivationSkipped {
					t.Fatalf("the later identical parse: %+v %v; want a skip", outcome, err)
				}
				if view, err := db.ReadActiveHarmonizedViewForTest(t.Context(), sid); err != nil || view.GenerationID != referenceGenerationID {
					t.Fatalf("active generation %s (%v), want the reference to stay active", view.GenerationID, err)
				}
			case referenceKindCrash:
				injected := errors.New("injected failure before the reference's first write")
				db.SetReferenceInstallSeamForTest(t, nil, func(*sqlite.Conn) error { return injected })
				plan, err := db.PlanManagedGenerationReference(t.Context(), w.request(t, w.renamed))
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.InstallManagedGenerationReference(t.Context(), plan); !errors.Is(err, injected) {
					t.Fatalf("install returned %v, want the injected failure", err)
				}
				if rows := referenceQuery(t, db, `SELECT 1 FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), referenceGenerationID); len(rows) != 0 {
					t.Fatal("the failed install left the reference's catalog row")
				}
				assertReaderGoldens(t, w.corpus, w.h, w.seeded)
			case referenceKindMoved:
				plan, err := db.PlanManagedGenerationReference(t.Context(), w.request(t, w.renamed))
				if err != nil {
					t.Fatal(err)
				}
				if outcome, err := w.seeded.parse(t, w.renamed, referenceOtherID, referenceInputHash); err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("activate G_other: %+v %v", outcome, err)
				}
				before := referenceStoreState(t, db, sid)
				_, err = db.InstallManagedGenerationReference(t.Context(), plan)
				assertReferenceRefusal(t, err, ingest.ReferenceRefusalSourceMoved)
				if after := referenceStoreState(t, db, sid); !slices.Equal(before, after) {
					t.Fatal("the refused install changed the store")
				}
			case referenceKindAnnotatedParse:
				control := seedReaderEntrypointStoreWithInput(t, w.corpus, w.h, referenceInputHash)
				createEntryAnnotation := func(targetDB *store.Store) string {
					t.Helper()
					annotatorID, err := targetDB.GetAnnotatorIDByName(t.Context(), "frustration-classifier")
					if err != nil || annotatorID == "" {
						t.Fatalf("resolve annotator: id=%q err=%v", annotatorID, err)
					}
					typeID, err := targetDB.GetAnnotationTypeID(t.Context(), "quality.frustration_signal")
					if err != nil || typeID == "" {
						t.Fatalf("resolve annotation type: id=%q err=%v", typeID, err)
					}
					id, err := targetDB.CreateEntryAnnotation(t.Context(), ingest.EntryAnnotationParams{
						SessionID:        string(sid),
						EntryIndex:       6,
						EndIndex:         7,
						AnnotatorID:      annotatorID,
						AnnotationTypeID: typeID,
						Value:            "detected",
					})
					if err != nil {
						t.Fatalf("create entry annotation: %v", err)
					}
					return id
				}
				refAnnID := createEntryAnnotation(db)
				ctrlAnnID := createEntryAnnotation(control.db)
				w.install(t, w.renamed)
				if outcome, err := control.parse(t, w.renamed, referenceGenerationID, referenceInputHash); err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("control parse for renamed context: %+v %v", outcome, err)
				}
				control.activeID = referenceGenerationID
				lines := strings.Split(strings.TrimRight(string(w.seeded.transcript), "\n"), "\n")
				shifted := strings.Join(append(append(slices.Clone(lines[:len(lines)-1]), c.AppendedLine), lines[len(lines)-1]), "\n") + "\n"
				grown := []byte(shifted)
				grownRecorded := w.renamed
				grownRecorded.ContentHash = schema.ComputeTranscriptHash(grown)
				grownRecorded.MetadataHash = schema.ComputeMetadataHash(&grownRecorded)
				grownHash := strings.Repeat("f", 64)
				laterClock := time.Now().UnixMilli() + 10_000
				w.seeded.transcript = grown
				w.seeded.corpus.ClockMs = laterClock
				w.seeded.allocator = &countingRefAllocator{entries: 100, submissions: 100}
				control.transcript = grown
				control.corpus.ClockMs = laterClock
				control.allocator = &countingRefAllocator{entries: 100, submissions: 100}
				if outcome, err := control.parse(t, grownRecorded, referenceLaterID, grownHash); err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("control later parse: %+v %v", outcome, err)
				}
				control.activeID = referenceLaterID
				if outcome, err := w.seeded.parse(t, grownRecorded, referenceLaterID, grownHash); err != nil || outcome.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("later parse after reference: %+v %v", outcome, err)
				}
				w.seeded.activeID = referenceLaterID
				wantTarget := referenceQuery(t, control.db, `SELECT entry_index, end_index FROM annotation_target_entries WHERE annotation_id = ?`, ctrlAnnID)
				if !slices.Equal(wantTarget, []string{"1:7|1:8"}) {
					t.Fatalf("control annotation %s target rows = %v, want [1:7|1:8]", ctrlAnnID, wantTarget)
				}
				if got := referenceQuery(t, db, `SELECT entry_index, end_index FROM annotation_target_entries WHERE annotation_id = ?`, refAnnID); !slices.Equal(got, wantTarget) {
					t.Fatalf("annotation %s target rows = %v, want %v (anchor lost)", refAnnID, got, wantTarget)
				}
				if got := referenceQuery(t, db, `SELECT state FROM annotation_target_anchors WHERE annotation_id = ?`, refAnnID); !slices.Equal(got, []string{"3:resolved"}) {
					t.Fatalf("annotation %s anchor state = %v, want [3:resolved]", refAnnID, got)
				}
				want, _ := readReaderEntrypoints(t, w.corpus, control)
				got, _ := readReaderEntrypoints(t, w.corpus, w.seeded)
				for _, entry := range slices.Sorted(maps.Keys(want)) {
					if len(want[entry]) == 0 || !bytes.Equal(want[entry], got[entry]) {
						t.Errorf("%s differs between the control and post-reference parse:\ncontrol   %s\nreference %s", entry, want[entry], got[entry])
					}
				}
			case referenceKindChained:
				w.install(t, w.renamed)
				secondRenamed := w.renamed
				secondRenamed.Project.Name = c.SecondRenamedProject
				secondRenamed.MetadataHash = schema.ComputeMetadataHash(&secondRenamed)
				req := w.request(t, secondRenamed)
				req.GenerationID = referenceLaterID
				plan, err := db.PlanManagedGenerationReference(t.Context(), req)
				if err != nil {
					t.Fatalf("plan the chained reference: %v", err)
				}
				outcome, err := db.InstallManagedGenerationReference(t.Context(), plan)
				if err != nil {
					t.Fatalf("install the chained reference: %v", err)
				}
				w.seeded.activeID = referenceLaterID
				if outcome.GenerationID != referenceLaterID || outcome.OwnerGenerationID != w.corpus.GenerationID {
					t.Fatalf("chained outcome %+v, want %s reading original owner %s", outcome, referenceLaterID, w.corpus.GenerationID)
				}
				row := referenceQuery(t, db, `SELECT mapping_set_id, project_name FROM session_generations WHERE session_id = ? AND generation_id = ?`, string(sid), referenceLaterID)
				if want := []string{"3:" + w.corpus.GenerationID + "|3:" + c.SecondRenamedProject}; !slices.Equal(row, want) {
					t.Fatalf("chained reference row %v, want %v", row, want)
				}
				if flag := referenceQuery(t, db, `SELECT content_sweep_pending FROM sessions WHERE session_id = ?`, string(sid)); !slices.Equal(flag, []string{"1:1"}) {
					t.Fatalf("content_sweep_pending = %v, want [1:1] because G_reference became collectable", flag)
				}
				twin := seedReaderEntrypointStoreWithInput(t, w.corpus, w.h, referenceInputHash)
				if out, err := twin.parse(t, w.renamed, referenceGenerationID, referenceInputHash); err != nil || out.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("twin parse 1: %+v %v", out, err)
				}
				if out, err := twin.parse(t, secondRenamed, referenceLaterID, referenceInputHash); err != nil || out.Disposition != ingest.ActivationCommittedNow {
					t.Fatalf("twin parse 2: %+v %v", out, err)
				}
				twin.activeID = referenceLaterID
				want, _ := readReaderEntrypoints(t, w.corpus, twin)
				got, _ := readReaderEntrypoints(t, w.corpus, w.seeded)
				for _, entry := range slices.Sorted(maps.Keys(want)) {
					if len(want[entry]) == 0 || !bytes.Equal(want[entry], got[entry]) {
						t.Errorf("%s differs between the plain parse and chained reference:\nparse     %s\nreference %s", entry, want[entry], got[entry])
					}
				}
			case referenceKindBlobPartParity:
				// Build an owner with 2 partitions and >= 2 content blobs so
				// ownerDigestsOnConn's partition and blob-ref ordering is non-vacuous.
				buildWithBlobsAndPartitions := func(genID string, recorded schema.UnifiedMetadata) (indexformat.V2, map[schema.SourceEntryRef][]byte) {
					cand, err := w.seeded.builder.BuildGeneration(t.Context(), ingest.GenerationBuildInput{
						Session: w.seeded.session, GenerationID: genID, RecordedMetadata: &recorded,
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
					partText := "partition one entry"
					v2.Generation.Earlier = []indexformat.EarlierPartition{{
						State: schema.EarlierHistoryUncertainMigrated,
						Content: indexformat.Partition{
							Entries: []schema.SessionEntry{{
								SessionID: sid, EntryIndex: 0, Harness: w.seeded.harness, EntryType: ingest.EntryTypeText,
								Role: ingest.RoleAssistant, ContentPreview: &partText, SourceEntryRef: "e_golden_part1_0",
							}},
						},
					}}
					blobRefs := []schema.SourceEntryRef{"e_golden_blob_0", "e_golden_blob_1", "e_golden_blob_2"}
					for i, ref := range blobRefs {
						payload := []byte(fmt.Sprintf("synthetic blob payload %d for %s", i, ref))
						sum := sha256.Sum256(payload)
						blobs[ref] = payload
						v2.Generation.Content = append(v2.Generation.Content, indexformat.ContentRecord{
							Ref:          ref,
							RelativeBlob: "c_" + hex.EncodeToString(sum[:]) + ".blob",
							ByteLength:   int64(len(payload)),
							Digest:       hex.EncodeToString(sum[:]),
						})
					}
					v2.Generation.Segments = append(v2.Generation.Segments, indexformat.ContextSegment{
						Ordinal:          len(v2.Generation.Segments),
						PhysicalSourceID: "source-0",
						Coordinates:      indexformat.SegmentCoordinates{Kind: indexformat.CoordinateKindSnapshotOnly},
						Inclusion:        indexformat.SegmentInclusionInherited,
						CapturedRefs:     blobRefs,
					})
					return v2, blobs
				}
				v2Owner, ownerBlobs := buildWithBlobsAndPartitions(w.corpus.GenerationID, w.seeded.recorded)
				if err := v2Owner.Generation.Validate(); err != nil {
					t.Fatalf("v2Owner.Validate: %v", err)
				}
				v2Renamed, renamedBlobs := buildWithBlobsAndPartitions(referenceGenerationID, w.renamed)
				storeR := openReaderEntrypointStore(t, t.TempDir())
				storeP := openReaderEntrypointStore(t, t.TempDir())
				activateWithProof := func(st *store.Store, v2 indexformat.V2, blobs map[schema.SourceEntryRef][]byte) {
					t.Helper()
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
					state, err := st.ReadIndexState(t.Context(), sid)
					if err != nil {
						t.Fatal(err)
					}
					act := store.GenerationActivation{
						Generation: v2, Blobs: blobs,
						IndexerVersion: w.seeded.target.IndexerVersion, IndexedAtMs: w.corpus.ClockMs,
						ContentCapture: capture, ExpectedState: state, IndexedInputHash: &referenceInputHash,
					}
					if state == nil || state.ArtifactHash == nil {
						artifact := strings.Repeat("a", 64)
						act.ArtifactIdentity = &artifact
					}
					if _, err := st.ActivateGeneration(t.Context(), act); err != nil {
						t.Fatalf("activate %s: %v", v2.Generation.ID, err)
					}
				}
				for _, st := range []*store.Store{storeR, storeP} {
					if _, err := st.InsertSessionsWithRevisions(t.Context(), []ingest.StoreEntry{{
						Metadata: &w.seeded.recorded, PublicationCapture: true, CWDProvenance: ingest.CWDSourceExact, Session: w.seeded.session,
					}}); err != nil {
						t.Fatalf("seed session: %v", err)
					}
					activateWithProof(st, v2Owner, ownerBlobs)
				}
				if got := referenceQuery(t, storeR, `SELECT COUNT(DISTINCT partition_id) FROM session_generation_entries WHERE session_id = ? AND generation_id = ?`, string(sid), w.corpus.GenerationID); !slices.Equal(got, []string{"1:2"}) {
					t.Fatalf("partition count = %v, want [1:2]", got)
				}
				if got := referenceQuery(t, storeR, `SELECT COUNT(DISTINCT source_entry_ref), COUNT(DISTINCT digest) FROM session_generation_content WHERE session_id = ? AND generation_id = ?`, string(sid), w.corpus.GenerationID); !slices.Equal(got, []string{"1:3|1:3"}) {
					t.Fatalf("blob count = %v, want [1:3|1:3]", got)
				}
				activateWithProof(storeP, v2Renamed, renamedBlobs)
				state, err := storeR.ReadIndexState(t.Context(), sid)
				if err != nil || state == nil {
					t.Fatalf("read index state: %+v %v", state, err)
				}
				candMeta := v2Renamed.Generation.Metadata
				if candMeta.AdapterVersion == nil {
					v := w.seeded.target.AdapterVersion
					candMeta.AdapterVersion = &v
				}
				plan, err := storeR.PlanManagedGenerationReference(t.Context(), ingest.ManagedGenerationReferenceRequest{
					Proof: ingest.ManagedGenerationUnchangedProof{
						SessionID: sid, Metadata: w.renamed, InputDigest: referenceInputHash,
						IndexerTarget: w.seeded.target.IndexerVersion, FormatTarget: w.seeded.target.IndexVersion, Expected: state,
					},
					Candidate: ingest.GenerationMetadataCandidate{
						Metadata: candMeta, Completeness: v2Renamed.Generation.Completeness,
						SourceEvidenceDigest: v2Renamed.Generation.SourceEvidenceDigest,
					},
					GenerationID: referenceGenerationID,
				})
				if err != nil {
					t.Fatalf("plan reference with blobs and partitions: %v", err)
				}
				if _, err := storeR.InstallManagedGenerationReference(t.Context(), plan); err != nil {
					t.Fatalf("install reference with blobs and partitions: %v", err)
				}
				sealQuery := `SELECT candidate_digest FROM session_generations WHERE session_id = ? AND generation_id = ?`
				parsed := referenceQuery(t, storeP, sealQuery, string(sid), referenceGenerationID)
				referenced := referenceQuery(t, storeR, sealQuery, string(sid), referenceGenerationID)
				if len(parsed) != 1 || !slices.Equal(parsed, referenced) {
					t.Fatalf("parse seal %v, reference seal %v; want equal seals", parsed, referenced)
				}
			default:
				t.Fatalf("reference case %s has unknown kind %q", c.Name, c.Kind)
			}
		})
	}
}

// assertReaderGoldens compares every harvest-path reader with its golden.
func assertReaderGoldens(t *testing.T, corpus readerEntrypointCorpus, h readerEntrypointHarness, seeded readerEntrypointStore) {
	t.Helper()
	_, manifest := loadReaderEntrypoints(t)
	observed, _ := readReaderEntrypoints(t, corpus, seeded)
	for _, entry := range manifest.Entries {
		name := h.Harness + "/" + entry
		if !slices.Contains(manifest.RequiredNames, name) {
			continue
		}
		want := readGolden(t, name)
		if !bytes.Equal(observed[entry], want) {
			t.Errorf("%s differs from its golden:\nwant %s\n got %s", name, want, observed[entry])
		}
	}
}

// assertReferenceRefusal requires err to be a refusal with reason, whose
// message states what, why, where, when, the impact and the fix.
func assertReferenceRefusal(t *testing.T, err error, reason ingest.ReferenceRefusalReason) {
	t.Helper()
	var refusal *store.ManagedGenerationReferenceError
	if !errors.As(err, &refusal) || refusal.Reason != reason {
		t.Fatalf("error %v, want a %s refusal", err, reason)
	}
	var generic ingest.ReferenceRefusal
	if !errors.As(err, &generic) || generic.RefusalReason() != reason {
		t.Fatalf("the refusal does not report its reason through ingest.ReferenceRefusal: %v", err)
	}
	for _, part := range []string{"refuse to reference", string(reason), "where: store.", "when: before any row changed", "impact:", "fix:"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("the refusal message does not name %q: %s", part, err)
		}
	}
}

func TestGenerationReferenceRefusals(t *testing.T) {
	fixture := loadGenerationReferenceFixture(t)
	for _, c := range loadGenerationReferenceRefusals(t) {
		t.Run(c.Name, func(t *testing.T) {
			w := newReferenceWorld(t, fixture.RenamedProject)
			db, sid := w.seeded.db, w.seeded.sid
			recorded := w.renamed
			if c.Change == refusalContextUnchanged {
				recorded = w.seeded.recorded
			}
			request := w.request(t, recorded)
			switch c.Change {
			case refusalIndexerTargetAhead:
				request.Proof.IndexerTarget++
			case refusalSourceEvidenceChanged:
				request.Candidate.SourceEvidenceDigest = strings.Repeat("e", 64)
			case refusalParseInputField:
				request.ParseInputContextFields = []string{referenceParseInputFieldValue}
			case refusalStatsRowMissing:
				referenceExec(t, db, `DELETE FROM session_captured_stats WHERE session_id = ?`, string(sid))
			case refusalSourceSealForged:
				forgeActiveSeal(t, db, sid)
			case refusalLatestAttemptHeld:
				referenceExec(t, db, `UPDATE sessions SET latest_attempt_generation_id = 'G_attempt' WHERE session_id = ?`, string(sid))
			case refusalContextUnchanged, refusalIndexStateMoved:
			default:
				t.Fatalf("refusal case %s has unknown change %q", c.Name, c.Change)
			}
			before := referenceStoreState(t, db, sid)
			plan, err := db.PlanManagedGenerationReference(t.Context(), request)
			if c.Where == refusalWherePlan {
				assertReferenceRefusal(t, err, ingest.ReferenceRefusalReason(c.Reason))
			} else {
				if err != nil {
					t.Fatalf("plan: %v", err)
				}
				if c.Change == refusalIndexStateMoved {
					referenceExec(t, db, `UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?`, strings.Repeat("c", 64), string(sid))
					before = referenceStoreState(t, db, sid)
				}
				_, err = db.InstallManagedGenerationReference(t.Context(), plan)
				assertReferenceRefusal(t, err, ingest.ReferenceRefusalReason(c.Reason))
			}
			if after := referenceStoreState(t, db, sid); !slices.Equal(before, after) {
				t.Fatal("the refusal changed the store")
			}
		})
	}
}

// forgeActiveSeal forges the active generation's stored seal with the
// immutability trigger lifted for the one update and restored from its own
// stored definition.
func forgeActiveSeal(t *testing.T, db *store.Store, sid schema.SessionID) {
	t.Helper()
	trigger := referenceQuery(t, db, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`)
	if len(trigger) != 1 {
		t.Fatalf("read the immutability trigger: %v", trigger)
	}
	definition := strings.SplitN(trigger[0], ":", 2)[1]
	conn, err := db.PoolForTest().Take(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer db.PoolForTest().Put(conn)
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`DROP TRIGGER session_generations_immutable`, nil},
		{`UPDATE session_generations SET candidate_digest = ? WHERE session_id = ? AND generation_id = (SELECT active_generation_id FROM sessions WHERE session_id = ?)`, []any{strings.Repeat("f", 64), string(sid), string(sid)}},
		{definition, nil},
	} {
		if err := sqlitex.ExecuteTransient(conn, step.query, &sqlitex.ExecOptions{Args: step.args}); err != nil {
			t.Fatalf("forge the active seal: %s: %v", step.query, err)
		}
	}
}

// TestReferenceInstallReadsNoParseTable records every table the install
// reads, on a store reopened so its connections and statement cache are
// fresh. The install must read the catalog (the positive control) and no
// parse table; a read of a parse table added inside the install must be
// seen.
func TestReferenceInstallReadsNoParseTable(t *testing.T) {
	fixture := loadGenerationReferenceFixture(t)
	for _, mutated := range []bool{false, true} {
		t.Run(fmt.Sprintf("mutated=%t", mutated), func(t *testing.T) {
			w := newReferenceWorld(t, fixture.RenamedProject)
			plan, err := w.seeded.db.PlanManagedGenerationReference(t.Context(), w.request(t, w.renamed))
			if err != nil {
				t.Fatal(err)
			}
			if err := w.seeded.db.Close(); err != nil {
				t.Fatal(err)
			}
			fresh := openReaderEntrypointStore(t, w.seeded.root)
			read := map[string]bool{}
			var authErr error
			var mutation func(*sqlite.Conn) error
			if mutated {
				mutation = func(conn *sqlite.Conn) error {
					return sqlitex.Execute(conn, `SELECT 1 FROM session_generation_entries WHERE session_id = ? LIMIT 1`, &sqlitex.ExecOptions{Args: []any{string(w.seeded.sid)}})
				}
			}
			fresh.SetReferenceInstallSeamForTest(t, func(conn *sqlite.Conn) {
				authErr = conn.SetAuthorizer(sqlite.AuthorizeFunc(func(action sqlite.Action) sqlite.AuthResult {
					if action.Type() == sqlite.OpRead {
						read[action.Table()] = true
					}
					return sqlite.AuthResultOK
				}))
			}, mutation)
			if _, err := fresh.InstallManagedGenerationReference(context.Background(), store.RebindReferencePlanForTest(plan, fresh)); err != nil {
				t.Fatal(err)
			}
			if authErr != nil {
				t.Fatal(authErr)
			}
			if !read["session_generations"] {
				t.Fatalf("the authorizer saw no read of session_generations (%v); the recording is not working", slices.Sorted(maps.Keys(read)))
			}
			var parseReads []string
			for _, table := range store.ParseTablesForTest() {
				if read[table] {
					parseReads = append(parseReads, table)
				}
			}
			if mutated && len(parseReads) == 0 {
				t.Fatal("the mutation read a parse table and the authorizer did not see it")
			}
			if !mutated && len(parseReads) != 0 {
				t.Fatalf("the install read parse tables %v", parseReads)
			}
		})
	}
}

func readGolden(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(readerEntrypointGoldensDir, name+".golden"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}
