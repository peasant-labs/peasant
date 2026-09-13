package store_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
	"zombiezen.com/go/sqlite"
)

//go:embed testdata/index_coverage.yaml
var indexCoverageFixtureData []byte

const indexCoverageFixturePath = "internal/store/testdata/index_coverage.yaml"

type indexCoverageFixtureDocument struct {
	RequiredCases []string                `yaml:"required_cases"`
	Cases         []indexCoverageCaseSpec `yaml:"cases"`
}

type indexCoverageCaseSpec struct {
	Name                string   `yaml:"name"`
	Kind                string   `yaml:"kind"`
	EmptyRequests       []string `yaml:"empty_requests"`
	Requested           int      `yaml:"requested"`
	RetainedEvery       int      `yaml:"retained_every"`
	DuplicateFirst      int      `yaml:"duplicate_first"`
	UnrequestedRetained int      `yaml:"unrequested_retained"`
	SuccessfulChunks    int      `yaml:"successful_chunks"`
	FirstPosition       int      `yaml:"first_position"`
}

var indexCoverageCaseKinds = map[string]bool{
	"empty-request":       true,
	"boundary":            true,
	"unknown":             true,
	"later-chunk-failure": true,
}

func loadIndexCoverageFixture(data []byte) (indexCoverageFixtureDocument, error) {
	var document indexCoverageFixtureDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%s: decode typed fields: %w", indexCoverageFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found another YAML document")
		}
		return document, fmt.Errorf("%s: exactly one YAML document is allowed: %w", indexCoverageFixturePath, err)
	}
	if len(document.Cases) == 0 {
		return document, fmt.Errorf("%s: the fixture holds no cases", indexCoverageFixturePath)
	}
	if len(document.RequiredCases) == 0 {
		return document, fmt.Errorf("%s: no required cases are named; a fixture with no manifest protects nothing", indexCoverageFixturePath)
	}
	required := map[string]bool{}
	for _, name := range document.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return document, fmt.Errorf("%s: required case name %q is blank or repeated", indexCoverageFixturePath, name)
		}
		required[name] = true
	}
	seen := map[string]bool{}
	chunkSize := store.SessionsWithoutEntriesChunkSizeForTest()
	for _, spec := range document.Cases {
		if strings.TrimSpace(spec.Name) == "" || seen[spec.Name] {
			return document, fmt.Errorf("%s: case name %q is missing or duplicated", indexCoverageFixturePath, spec.Name)
		}
		seen[spec.Name] = true
		if !indexCoverageCaseKinds[spec.Kind] {
			return document, fmt.Errorf("%s: case %q names the unknown kind %q", indexCoverageFixturePath, spec.Name, spec.Kind)
		}
		switch spec.Kind {
		case "empty-request":
			if len(spec.EmptyRequests) == 0 {
				return document, fmt.Errorf("%s: case %q names no empty-request shapes", indexCoverageFixturePath, spec.Name)
			}
			for _, shape := range spec.EmptyRequests {
				if shape != "nil" && shape != "empty" {
					return document, fmt.Errorf("%s: case %q names the unknown empty-request shape %q", indexCoverageFixturePath, spec.Name, shape)
				}
			}
		case "boundary":
			if spec.Requested <= chunkSize {
				return document, fmt.Errorf("%s: case %q requests %d session(s), which does not exceed the %d-session chunk size", indexCoverageFixturePath, spec.Name, spec.Requested, chunkSize)
			}
			if spec.RetainedEvery <= 0 {
				return document, fmt.Errorf("%s: case %q has retained_every=%d", indexCoverageFixturePath, spec.Name, spec.RetainedEvery)
			}
			if spec.DuplicateFirst < 0 || spec.UnrequestedRetained < 0 {
				return document, fmt.Errorf("%s: case %q has a negative duplicate or unrequested count", indexCoverageFixturePath, spec.Name)
			}
		case "unknown":
			if spec.Requested <= 0 || spec.FirstPosition < 0 {
				return document, fmt.Errorf("%s: case %q needs a positive requested count and a non-negative first_position", indexCoverageFixturePath, spec.Name)
			}
		case "later-chunk-failure":
			if spec.Requested <= spec.SuccessfulChunks*chunkSize {
				return document, fmt.Errorf("%s: case %q requests %d session(s) with %d successful chunk(s); the population must reach a chunk beyond the successful ones", indexCoverageFixturePath, spec.Name, spec.Requested, spec.SuccessfulChunks)
			}
			if spec.SuccessfulChunks < 1 {
				return document, fmt.Errorf("%s: case %q must have at least one successful chunk before the failure", indexCoverageFixturePath, spec.Name)
			}
		}
	}
	missing, extra := "", ""
	for name := range required {
		if !seen[name] {
			missing = name
			break
		}
	}
	for name := range seen {
		if !required[name] {
			extra = name
			break
		}
	}
	if missing != "" {
		return document, fmt.Errorf("%s: required case %q is missing", indexCoverageFixturePath, missing)
	}
	if extra != "" {
		return document, fmt.Errorf("%s: case %q is not in the required manifest", indexCoverageFixturePath, extra)
	}
	return document, nil
}

func loadIndexCoverageCases(t *testing.T) []indexCoverageCaseSpec {
	t.Helper()
	document, err := loadIndexCoverageFixture(indexCoverageFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// coverageSessionID returns a deterministic valid session UUID for index i.
// Generated, not fixture-listed: the chunk-boundary case needs more sessions
// than any readable manifest should carry, and every ID here is shaped only by
// its position (requested, seeded, duplicated), never by an individual value.
func coverageSessionID(i int) ingest.SessionID {
	return ingest.SessionID(fmt.Sprintf("11111111-1111-4111-8111-%012x", i))
}

// seedCoverageEntries stores one entry row for the session so it counts as
// retained. The sessions row must exist first: the entry write refuses a
// session whose metadata was never stored.
func seedCoverageEntries(t *testing.T, s *store.Store, id ingest.SessionID) {
	t.Helper()
	seedSession(t, s, string(id))
	preview := "retained entry content"
	writes := []ingest.SessionEntryWrite{{
		SessionID:    id,
		Result:       indexformat.V1{Entries: []schema.SessionEntry{{SessionID: id, Harness: schema.HarnessClaudeCode, EntryIndex: 0, EntryType: schema.EntryTypeText, Role: schema.RoleUser, ContentPreview: &preview}}},
		IndexVersion: 1,
	}}
	if result := s.IndexSessionEntryBatch(context.Background(), writes); result[0].Err != nil {
		t.Fatalf("seed entries for %s: %v", id, result[0].Err)
	}
}

// assertCoverageIdentity pins FailedAttempts == Empty + FailedRetained, the
// identity the coverage report promises whenever it is measured.
func assertCoverageIdentity(t *testing.T, coverage *ingest.IndexCoverage) {
	t.Helper()
	if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
		t.Errorf("FailedAttempts = %d, want Empty + FailedRetained = %d + %d",
			coverage.FailedAttempts, coverage.Empty, coverage.FailedRetained)
	}
}

// TestStore_SessionsWithoutEntries drives each named fixture case against the
// real store. The boundary case past one variable-limit chunk also feeds its
// distinct requested IDs through the production coverage computation and pins
// the aggregation identity.
func TestStore_SessionsWithoutEntries(t *testing.T) {
	for _, spec := range loadIndexCoverageCases(t) {
		spec := spec
		t.Run(spec.Name, func(t *testing.T) {
			t.Parallel()
			switch spec.Kind {
			case "empty-request":
				runEmptyRequestCase(t, spec)
			case "boundary":
				runBoundaryCase(t, spec)
			case "unknown":
				runUnknownCase(t, spec)
			case "later-chunk-failure":
				runLaterChunkFailureCase(t, spec)
			default:
				t.Fatalf("case names the unhandled kind %q", spec.Kind)
			}
		})
	}
}

func runEmptyRequestCase(t *testing.T, spec indexCoverageCaseSpec) {
	s := openTestStore(t)
	for _, shape := range spec.EmptyRequests {
		var ids []ingest.SessionID
		switch shape {
		case "nil":
			ids = nil
		case "empty":
			ids = []ingest.SessionID{}
		default:
			t.Fatalf("unknown empty-request shape %q", shape)
		}
		got, err := s.SessionsWithoutEntries(context.Background(), ids)
		if err != nil {
			t.Fatalf("SessionsWithoutEntries(%s request): %v", shape, err)
		}
		if len(got) != 0 {
			t.Fatalf("SessionsWithoutEntries(%s request) = %v, want empty", shape, got)
		}
	}
}

func runBoundaryCase(t *testing.T, spec indexCoverageCaseSpec) {
	s := openTestStore(t)
	ctx := context.Background()

	ids := make([]ingest.SessionID, 0, spec.Requested+spec.DuplicateFirst)
	want := make(map[ingest.SessionID]bool, spec.Requested)
	for i := 0; i < spec.Requested; i++ {
		id := coverageSessionID(i)
		ids = append(ids, id)
		hasEntries := i%spec.RetainedEvery == 0
		if hasEntries {
			seedCoverageEntries(t, s, id)
		}
		want[id] = !hasEntries
	}
	// Duplicates collapse: the first sessions are asked for twice.
	ids = append(ids, ids[:spec.DuplicateFirst]...)
	// Exclusion: sessions with entries that were never requested must not
	// appear in the answer.
	for i := spec.Requested; i < spec.Requested+spec.UnrequestedRetained; i++ {
		seedCoverageEntries(t, s, coverageSessionID(i))
	}

	got, err := s.SessionsWithoutEntries(ctx, ids)
	if err != nil {
		t.Fatalf("SessionsWithoutEntries: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("SessionsWithoutEntries reported %d session(s), want %d; duplicates must collapse and unrequested sessions must stay out", len(got), len(want))
	}
	for id, wantWithout := range want {
		gotWithout, ok := got[id]
		if !ok {
			t.Errorf("SessionsWithoutEntries omits requested session %s", id)
			continue
		}
		if gotWithout != wantWithout {
			t.Errorf("SessionsWithoutEntries(%s) = %v, want %v", id, gotWithout, wantWithout)
		}
	}
	for i := spec.Requested; i < spec.Requested+spec.UnrequestedRetained; i++ {
		if _, ok := got[coverageSessionID(i)]; ok {
			t.Errorf("SessionsWithoutEntries names unrequested session %s", coverageSessionID(i))
		}
	}

	// The same distinct requested population through the production coverage
	// computation: the aggregation identity must hold on the boundary, and the
	// split must match the fixture's retained-every rule rather than a
	// hand-built count.
	failed := make([]ingest.SessionID, 0, spec.Requested)
	for i := 0; i < spec.Requested; i++ {
		failed = append(failed, coverageSessionID(i))
	}
	coverage, err := ingest.ComputeIndexCoverage(ctx, s, failed)
	if err != nil {
		t.Fatalf("ComputeIndexCoverage over the boundary population: %v", err)
	}
	if coverage == nil {
		t.Fatal("ComputeIndexCoverage answered nil for a readable store; nil means unavailable, never zero")
	}
	assertCoverageIdentity(t, coverage)
	if coverage.FailedAttempts != spec.Requested {
		t.Errorf("FailedAttempts = %d, want %d distinct requested sessions", coverage.FailedAttempts, spec.Requested)
	}
	wantRetained := (spec.Requested + spec.RetainedEvery - 1) / spec.RetainedEvery
	if coverage.FailedRetained != wantRetained || coverage.Empty != spec.Requested-wantRetained {
		t.Errorf("coverage = %+v, want {Empty:%d FailedRetained:%d} from the retained-every rule",
			coverage, spec.Requested-wantRetained, wantRetained)
	}
}

func runUnknownCase(t *testing.T, spec indexCoverageCaseSpec) {
	s := openTestStore(t)
	ids := make([]ingest.SessionID, 0, spec.Requested)
	for i := 0; i < spec.Requested; i++ {
		ids = append(ids, coverageSessionID(spec.FirstPosition+i))
	}
	got, err := s.SessionsWithoutEntries(context.Background(), ids)
	if err != nil {
		t.Fatalf("SessionsWithoutEntries: %v", err)
	}
	for _, id := range ids {
		if !got[id] {
			t.Errorf("SessionsWithoutEntries(%s) = false, want true: a session with no rows holds no entries", id)
		}
	}
}

func runLaterChunkFailureCase(t *testing.T, spec indexCoverageCaseSpec) {
	s := openTestStore(t)
	ctx := context.Background()

	ids := make([]ingest.SessionID, 0, spec.Requested)
	for i := 0; i < spec.Requested; i++ {
		ids = append(ids, coverageSessionID(i+100000))
	}
	calls := 0
	s.SetSessionMembershipChunk(func(_ *sqlite.Conn, chunk []ingest.SessionID, record func(ingest.SessionID)) error {
		calls++
		if calls > spec.SuccessfulChunks {
			return errors.New("synthetic membership failure on a later chunk")
		}
		// A successful chunk reports entries for the sessions it sees and
		// marks itself retained, so the production map holds accumulated rows
		// by the time the later chunk fails.
		for _, id := range chunk {
			record(id)
		}
		return nil
	})

	got, err := s.SessionsWithoutEntries(ctx, ids)
	if err == nil {
		t.Fatalf("SessionsWithoutEntries succeeded after a later chunk failed; the read is all-or-nothing")
	}
	if got != nil {
		t.Errorf("SessionsWithoutEntries returned %d accumulated row(s) alongside the error; a failed later chunk must discard the earlier chunks, never leak a partial map", len(got))
	}

	// Downstream, the production computation must see unavailable, never a
	// partial count.
	coverage, cerr := ingest.ComputeIndexCoverage(ctx, s, ids)
	if cerr == nil {
		t.Fatal("ComputeIndexCoverage succeeded over a store whose membership read failed")
	}
	if coverage != nil {
		t.Errorf("ComputeIndexCoverage = %+v, want nil: a failed later chunk must leave no partial count", coverage)
	}
}
