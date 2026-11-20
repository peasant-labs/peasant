package store

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testkit/testwait"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite"
	"github.com/peasant-labs/peasant/third_party/zombiezen-sqlite/sqlitex"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/validation_terms.yaml
var validationTermsYAML []byte

//go:embed testdata/validation_terms.manifest.yaml
var validationTermsManifestYAML []byte

//go:embed testdata/refresh_read_slots.yaml
var refreshReadSlotsYAML []byte

// The closed vocabularies of validation_terms.yaml.
type (
	validationCaseKind      string
	validationCaseCandidate string
	validationCaseSetup     string
	validationCaseBetween   string
	validationCaseValidated string
)

const (
	validationKindCheapOrder validationCaseKind = "cheap-order"
	validationKindValidate   validationCaseKind = "validate"
	validationKindSettle     validationCaseKind = "settle"

	validationCandidateStored       validationCaseCandidate = "stored"
	validationCandidateNone         validationCaseCandidate = "none"
	validationCandidateContentDiffs validationCaseCandidate = "content-hash-differs"

	validationSetupStatsOverflow validationCaseSetup = "stats-overflow-diagnostic"

	validationBetweenInsertCommit    validationCaseBetween = "insert-commit"
	validationBetweenActivateAnother validationCaseBetween = "activate-another"

	validationValidatedActive validationCaseValidated = "active"
	validationValidatedOther  validationCaseValidated = "other"

	// forgedMetadataTriggerRestored is the mutation that forges the stored
	// metadata_hash with the immutability trigger restored.
	forgedMetadataTriggerRestored = "forged-metadata-trigger-restored"
	// The settle outcomes of a settle case.
	validationSettleSettled = "settled"
	validationSettleRefused = "refused"
)

type validationTermsCase struct {
	Name                string                  `yaml:"name"`
	Kind                validationCaseKind      `yaml:"kind"`
	Mutation            string                  `yaml:"mutation"`
	Candidate           validationCaseCandidate `yaml:"candidate"`
	Setup               validationCaseSetup     `yaml:"setup"`
	Between             validationCaseBetween   `yaml:"between"`
	ValidatedGeneration validationCaseValidated `yaml:"validatedGeneration"`
	SkipValidation      bool                    `yaml:"skipValidation"`
	Outcome             string                  `yaml:"outcome"`
	Mismatch            string                  `yaml:"mismatch"`
}

func loadValidationTermsCases(t *testing.T) []validationTermsCase {
	t.Helper()
	var fixture struct {
		Cases []validationTermsCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(validationTermsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	if err := yaml.Unmarshal(validationTermsManifestYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixture.Cases {
		names = append(names, c.Name)
	}
	if len(names) == 0 {
		t.Fatal("validation terms fixture has no cases")
	}
	if err := validateRecoveryRequiredNames(manifest.RequiredNames, names, "validation terms"); err != nil {
		t.Fatal(err)
	}
	return fixture.Cases
}

// storedValidationCandidate is the metadata candidate a builder would produce
// for the setup's generation: its own metadata, completeness and source
// evidence, read back from the stored row.
func storedValidationCandidate(t *testing.T, s *Store, p ManagedGenerationUnchangedProof) ingest.GenerationMetadataCandidate {
	t.Helper()
	var completeness, evidence string
	err := withTestConn(s, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, `SELECT completeness, source_evidence_digest FROM session_generations WHERE session_id=? AND generation_id=?`, &sqlitex.ExecOptions{
			Args: []any{string(p.SessionID), p.GenerationID},
			ResultFunc: func(st *sqlite.Stmt) error {
				completeness, evidence = st.ColumnText(0), st.ColumnText(1)
				return nil
			},
		})
	})
	if err != nil || evidence == "" {
		t.Fatalf("read the stored generation for the candidate: evidence %q, error %v", evidence, err)
	}
	return ingest.GenerationMetadataCandidate{
		Metadata:             p.Metadata,
		Completeness:         indexformat.GenerationCompleteness(completeness),
		SourceEvidenceDigest: evidence,
	}
}

func withTestConn(s *Store, fn func(*sqlite.Conn) error) error {
	conn, err := s.pool.Take(context.Background())
	if err != nil {
		return err
	}
	defer s.pool.Put(conn)
	return fn(conn)
}

func execTestSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if err := withTestConn(s, func(conn *sqlite.Conn) error {
		return sqlitex.Execute(conn, query, &sqlitex.ExecOptions{Args: args})
	}); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
}

// forgeMetadataHashTriggerRestored forges the active generation's stored
// metadata_hash to another non-zero value. The immutability trigger is lifted
// for the one update and restored from its own stored definition, which the
// test asserts is unchanged afterwards.
func forgeMetadataHashTriggerRestored(t *testing.T, s *Store, p ManagedGenerationUnchangedProof) {
	t.Helper()
	readTrigger := func() string {
		var sql string
		if err := withTestConn(s, func(conn *sqlite.Conn) error {
			return sqlitex.Execute(conn, `SELECT sql FROM sqlite_schema WHERE type='trigger' AND name='session_generations_immutable'`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
				sql = st.ColumnText(0)
				return nil
			}})
		}); err != nil || sql == "" {
			t.Fatalf("read the immutability trigger: %q, %v", sql, err)
		}
		return sql
	}
	trigger := readTrigger()
	execTestSQL(t, s, `DROP TRIGGER session_generations_immutable`)
	execTestSQL(t, s, `UPDATE session_generations SET metadata_hash=? WHERE session_id=? AND generation_id=?`, strings.Repeat("f", 64), string(p.SessionID), p.GenerationID)
	execTestSQL(t, s, trigger)
	if restored := readTrigger(); restored != trigger {
		t.Fatalf("the immutability trigger was not restored exactly:\n%s\nwant\n%s", restored, trigger)
	}
}

// dumpStoreRows renders every row of every table, sorted, so a test can show
// that an operation wrote nothing.
func dumpStoreRows(t *testing.T, s *Store) []string {
	t.Helper()
	var rows []string
	err := withTestConn(s, func(conn *sqlite.Conn) error {
		var tables []string
		if err := sqlitex.Execute(conn, `SELECT name FROM sqlite_schema WHERE type='table' AND name NOT LIKE 'sqlite_%' AND sql NOT LIKE 'CREATE VIRTUAL TABLE%' ORDER BY name`, &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
			tables = append(tables, st.ColumnText(0))
			return nil
		}}); err != nil {
			return err
		}
		for _, table := range tables {
			if err := sqlitex.Execute(conn, fmt.Sprintf(`SELECT * FROM "%s"`, table), &sqlitex.ExecOptions{ResultFunc: func(st *sqlite.Stmt) error {
				var cells []string
				for i := range st.ColumnCount() {
					cells = append(cells, st.ColumnName(i)+"="+st.ColumnText(i))
				}
				rows = append(rows, table+": "+strings.Join(cells, "|"))
				return nil
			}}); err != nil {
				return fmt.Errorf("dump %s: %w", table, err)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(rows)
	return rows
}

func validationRequest(p ManagedGenerationUnchangedProof, candidate *ingest.GenerationMetadataCandidate) ingest.ManagedGenerationValidationRequest {
	return ingest.ManagedGenerationValidationRequest{
		SessionID: p.SessionID, GenerationID: p.GenerationID, Metadata: p.Metadata,
		InputDigest: p.InputDigest, IndexerTarget: p.IndexerTarget, FormatTarget: p.FormatTarget,
		Expected: p.Expected, Candidate: candidate,
	}
}

// TestManagedGenerationContextValidation drives the validator and the
// validated settle through the cases of validation_terms.yaml.
func TestManagedGenerationContextValidation(t *testing.T) {
	cases := loadValidationTermsCases(t)
	shared := LoadUnchangedProofFixtures(t).Cases
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			switch c.Kind {
			case validationKindCheapOrder:
				runCheapOrderCase(t, shared)
			case validationKindValidate:
				runValidateCase(t, c)
			case validationKindSettle:
				runValidatedSettleCase(t, c)
			default:
				t.Fatalf("unknown case kind %q", c.Kind)
			}
		})
	}
}

// runCheapOrderCase proves every shared mismatch case through the evaluator's
// stored-digest mode: the cheap proof keeps its order and its answers.
func runCheapOrderCase(t *testing.T, shared []unchangedProofCase) {
	if len(shared) == 0 {
		t.Fatal("no shared unchanged-proof cases")
	}
	for _, tc := range shared {
		t.Run(tc.Name, func(t *testing.T) {
			s, p := unchangedProofSetup(t)
			applyUnchangedProofMutation(t, s, &p, tc.Mutation)
			var got ingest.UnchangedProof
			if err := withTestConn(s, func(conn *sqlite.Conn) (err error) {
				got, err = evaluateUnchangedTermsOnConn(conn, p, termModeStoredDigests)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want, err := ingest.NewUnchangedProofMismatch(tc.Mismatch)
			if err != nil {
				t.Fatal(err)
			}
			if got.Mismatch != want || got.Proven != (want == ingest.UnchangedMismatchNone) {
				t.Fatalf("stored-digest terms = %+v, want mismatch %q", got, want)
			}
		})
	}
}

func prepareValidationCase(t *testing.T, c validationTermsCase) (*Store, ManagedGenerationUnchangedProof, *ingest.GenerationMetadataCandidate) {
	t.Helper()
	s, p := unchangedProofSetup(t)
	candidate := storedValidationCandidate(t, s, p)
	switch c.Mutation {
	case "":
	case forgedMetadataTriggerRestored:
		forgeMetadataHashTriggerRestored(t, s, p)
		if proof, err := s.ProveManagedGenerationUnchanged(context.Background(), p); err != nil || proof.Mismatch != ingest.UnchangedNotProvenMetadata {
			t.Fatalf("a forged metadata hash must fail the cheap proof on metadata-context: %+v, %v", proof, err)
		}
	default:
		applyUnchangedProofMutation(t, s, &p, c.Mutation)
	}
	switch c.Setup {
	case "":
	case validationSetupStatsOverflow:
		execTestSQL(t, s, `INSERT INTO session_generation_diagnostics(session_id,generation_id,ordinal,error_type,location,message,remediation) VALUES(?,?,0,'stats-overflow','stats','synthetic overflow','none')`, string(p.SessionID), p.GenerationID)
	default:
		t.Fatalf("unknown setup %q", c.Setup)
	}
	switch c.Candidate {
	case validationCandidateStored:
		return s, p, &candidate
	case validationCandidateNone:
		return s, p, nil
	case validationCandidateContentDiffs:
		candidate.Metadata.ContentHash = strings.Repeat("c", 64)
		return s, p, &candidate
	default:
		t.Fatalf("unknown candidate %q", c.Candidate)
	}
	return nil, p, nil
}

func assertValidationOutcome(t *testing.T, c validationTermsCase, got ingest.ManagedGenerationValidationResult) {
	t.Helper()
	want, err := ingest.NewValidationOutcome(c.Outcome)
	if err != nil {
		t.Fatal(err)
	}
	mismatch, err := ingest.NewUnchangedProofMismatch(c.Mismatch)
	if err != nil {
		t.Fatal(err)
	}
	if got.Outcome != want || got.Mismatch != mismatch {
		t.Fatalf("validation = %+v, want %q with mismatch %q", got, want, mismatch)
	}
	if _, err := ingest.NewManagedGenerationValidationResult(got.Outcome, got.Mismatch, got.Detail); err != nil {
		t.Fatalf("the validator answered an inconsistent result: %v", err)
	}
}

func runValidateCase(t *testing.T, c validationTermsCase) {
	s, p, candidate := prepareValidationCase(t, c)
	before := dumpStoreRows(t, s)
	if candidate == nil {
		// With the only pooled connection held, a validator that took a
		// connection would wait; an unsupported answer comes back at once.
		conn, err := s.pool.Take(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		got, err := s.ValidateManagedGenerationUnchanged(testwait.Context(t), validationRequest(p, nil))
		s.pool.Put(conn)
		if err != nil {
			t.Fatal(err)
		}
		assertValidationOutcome(t, c, got)
		return
	}
	got, err := s.ValidateManagedGenerationUnchanged(context.Background(), validationRequest(p, candidate))
	if err != nil {
		t.Fatal(err)
	}
	assertValidationOutcome(t, c, got)
	if after := dumpStoreRows(t, s); !slices.Equal(before, after) {
		t.Fatal("validation changed a stored row")
	}
}

func runValidatedSettleCase(t *testing.T, c validationTermsCase) {
	s, p, candidate := prepareValidationCase(t, c)
	if candidate == nil {
		t.Fatal("a settle case needs a candidate")
	}
	if !c.SkipValidation {
		got, err := s.ValidateManagedGenerationUnchanged(context.Background(), validationRequest(p, candidate))
		if err != nil || got.Outcome != ingest.ValidationProven {
			t.Fatalf("validation before the settle = %+v, %v; want proven", got, err)
		}
	}
	validated := &ingest.ValidatedContext{GenerationID: p.GenerationID, Candidate: *candidate}
	switch c.ValidatedGeneration {
	case validationValidatedActive:
	case validationValidatedOther:
		validated.GenerationID = "another-generation"
	default:
		t.Fatalf("unknown validated generation %q", c.ValidatedGeneration)
	}
	switch c.Between {
	case "":
	case validationBetweenInsertCommit:
		execTestSQL(t, s, `INSERT INTO session_generation_commits(session_id,generation_id,ordinal,hash,message,author_name,author_email,commit_time,author_time) VALUES(?,?,0,?,'synthetic','a','a@example.invalid',1,1)`, string(p.SessionID), p.GenerationID, strings.Repeat("e", 40))
	case validationBetweenActivateAnother:
		next, blobs := buildTestGeneration(t, p.SessionID, "unchanged_next", "another body", "another input", "another output")
		next.Generation.Metadata.ContentHash = strings.Repeat("b", 64)
		next = filledCandidateForValidation(t, next, blobs)
		if _, err := s.ActivateGeneration(context.Background(), GenerationActivation{Generation: next, Blobs: blobs, IndexerVersion: 1, IndexedAtMs: 150}); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown between step %q", c.Between)
	}
	before := dumpStoreRows(t, s)
	statsBefore, err := s.ReadCapturedStats(context.Background(), p.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	p.Validated = validated
	p.UpdatedAtMs = 777
	result, err := s.SettleManagedGenerationUnchanged(context.Background(), p)
	switch c.Outcome {
	case validationSettleSettled:
		if err != nil || result.GenerationID != p.GenerationID {
			t.Fatalf("validated settle = %+v, %v; want the active generation %s", result, err, p.GenerationID)
		}
		stats, err := s.ReadCapturedStats(context.Background(), p.SessionID)
		if err != nil || stats.UpdatedAtMs != 777 || statsBefore.UpdatedAtMs == 777 {
			t.Fatalf("the validated settle did not write the stats row: %+v, %v", stats, err)
		}
		state, err := s.ReadIndexState(context.Background(), p.SessionID)
		if err != nil || state == nil || state.ActiveGenerationID != p.GenerationID {
			t.Fatalf("index state after the validated settle = %+v, %v", state, err)
		}
		assertNoNewGenerationRow(t, before, dumpStoreRows(t, s))
	case validationSettleRefused:
		var stale *ingest.StaleIndexWorkError
		if !errors.As(err, &stale) {
			t.Fatalf("validated settle error = %v (result %+v), want a stale-work refusal", err, result)
		}
		if after := dumpStoreRows(t, s); !slices.Equal(before, after) {
			t.Fatal("a refused validated settle changed a stored row")
		}
	default:
		t.Fatalf("unknown settle outcome %q", c.Outcome)
	}
}

func assertNoNewGenerationRow(t *testing.T, before, after []string) {
	t.Helper()
	count := func(rows []string) int {
		n := 0
		for _, row := range rows {
			if strings.HasPrefix(row, "session_generations: ") {
				n++
			}
		}
		return n
	}
	if count(before) == 0 || count(before) != count(after) {
		t.Fatalf("generation rows %d -> %d; the validated settle must write none", count(before), count(after))
	}
}

// TestContextValidationRunsOnAReaderConnection shows the validator never needs
// the SQLite writer: it completes while another connection holds an
// IMMEDIATE transaction.
func TestContextValidationRunsOnAReaderConnection(t *testing.T) {
	s, p := unchangedProofSetupWithPool(t, 2)
	candidate := storedValidationCandidate(t, s, p)
	forgeMetadataHashTriggerRestored(t, s, p)
	writer, err := s.pool.Take(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.pool.Put(writer)
	if err := sqlitex.ExecuteTransient(writer, "BEGIN IMMEDIATE", nil); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sqlitex.ExecuteTransient(writer, "ROLLBACK", nil) }()
	got, err := s.ValidateManagedGenerationUnchanged(testwait.Context(t), validationRequest(p, &candidate))
	if err != nil || got.Outcome != ingest.ValidationProven {
		t.Fatalf("validation beside a held writer = %+v, %v; want proven", got, err)
	}
}

// TestRefreshReadSlotsCapValidations holds two validations inside their read
// slots on a pool of four, and shows a third waits until one releases.
func TestRefreshReadSlotsCapValidations(t *testing.T) {
	s, p := unchangedProofSetupWithPool(t, 4)
	candidate := storedValidationCandidate(t, s, p)
	held := make(chan struct{}, 3)
	waiting := make(chan struct{}, 3)
	gate := make(chan struct{})
	s.refreshReadSlotSeam = func(ctx context.Context, stage refreshReadSlotStage) {
		switch stage {
		case refreshReadSlotWaiting:
			waiting <- struct{}{}
		case refreshReadSlotHeld:
			held <- struct{}{}
			<-gate
		}
	}
	results := make(chan error, 3)
	validate := func() {
		got, err := s.ValidateManagedGenerationUnchanged(context.Background(), validationRequest(p, &candidate))
		if err == nil && got.Outcome != ingest.ValidationProven {
			err = fmt.Errorf("validation = %+v, want proven", got)
		}
		results <- err
	}
	go validate()
	go validate()
	testwait.Receive(t, held, "the first validation to hold a slot")
	testwait.Receive(t, held, "the second validation to hold a slot")
	testwait.Receive(t, waiting, "the first validation to ask for a slot")
	testwait.Receive(t, waiting, "the second validation to ask for a slot")
	go validate()
	testwait.Receive(t, waiting, "the third validation to ask for a slot")
	// Both slots are held and the third caller has asked; it must not hold
	// one yet.
	select {
	case <-held:
		t.Fatal("a third validation took a read slot while two were held on a pool of four")
	default:
	}
	close(gate)
	testwait.Receive(t, held, "the third validation to hold a slot once one was released")
	for range 3 {
		if err := testwait.Receive(t, results, "a validation result"); err != nil {
			t.Fatal(err)
		}
	}
}

// TestRefreshReadSlotWaitHonorsCancellation shows a cancelled wait returns the
// context's error and leaves every pooled connection free.
func TestRefreshReadSlotWaitHonorsCancellation(t *testing.T) {
	s, p := unchangedProofSetupWithPool(t, 2)
	candidate := storedValidationCandidate(t, s, p)
	gate := make(chan struct{})
	held := make(chan struct{}, 1)
	waiting := make(chan struct{}, 2)
	s.refreshReadSlotSeam = func(ctx context.Context, stage refreshReadSlotStage) {
		switch stage {
		case refreshReadSlotWaiting:
			waiting <- struct{}{}
		case refreshReadSlotHeld:
			held <- struct{}{}
			<-gate
		}
	}
	first := make(chan error, 1)
	go func() {
		_, err := s.ValidateManagedGenerationUnchanged(context.Background(), validationRequest(p, &candidate))
		first <- err
	}()
	testwait.Receive(t, held, "the first validation to hold the only slot")
	testwait.Receive(t, waiting, "the first validation's request")
	ctx, cancel := context.WithCancel(context.Background())
	second := make(chan error, 1)
	go func() {
		_, err := s.ValidateManagedGenerationUnchanged(ctx, validationRequest(p, &candidate))
		second <- err
	}()
	testwait.Receive(t, waiting, "the second validation to ask for a slot")
	cancel()
	if err := testwait.Receive(t, second, "the cancelled validation"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait returned %v, want context.Canceled", err)
	}
	close(gate)
	if err := testwait.Receive(t, first, "the first validation"); err != nil {
		t.Fatal(err)
	}
	// Every pooled connection is free again: the cancelled caller took none.
	ctxTake := testwait.Context(t)
	var conns []*sqlite.Conn
	for range 2 {
		conn, err := s.pool.Take(ctxTake)
		if err != nil {
			t.Fatalf("take pooled connection %d after the cancelled wait: %v", len(conns)+1, err)
		}
		conns = append(conns, conn)
	}
	for _, conn := range conns {
		s.pool.Put(conn)
	}
}

// TestRefreshReadSlotCap pins the default cap for the pool sizes the fixture
// names.
func TestRefreshReadSlotCap(t *testing.T) {
	var fixture struct {
		Cases []struct {
			PoolSize int `yaml:"poolSize"`
			Slots    int `yaml:"slots"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(refreshReadSlotsYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("no read-slot cases")
	}
	for _, c := range fixture.Cases {
		t.Run(fmt.Sprintf("pool-%d", c.PoolSize), func(t *testing.T) {
			s, _ := openGenerationStoreWith(t, WithPoolSize(c.PoolSize))
			release, err := s.acquireRefreshReadSlot(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			release()
			if got := cap(s.refreshReadSlots); got != c.Slots {
				t.Fatalf("pool %d: read slots = %d, want %d", c.PoolSize, got, c.Slots)
			}
		})
	}
}

// unchangedProofSetupWithPool is unchangedProofSetup on a pool of the given
// size.
func unchangedProofSetupWithPool(t *testing.T, poolSize int) (*Store, ManagedGenerationUnchangedProof) {
	t.Helper()
	return unchangedProofSetupWith(t, WithPoolSize(poolSize), WithWALAutocheckpointDisabled())
}
