package ingest_test

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_coverage_compute.yaml
var indexCoverageComputeFixtureData []byte

const indexCoverageComputeFixturePath = "internal/ingest/testdata/index_coverage_compute.yaml"

type indexCoverageComputeDocument struct {
	RequiredCases []string                       `yaml:"required_cases"`
	Cases         []indexCoverageComputeCaseSpec `yaml:"cases"`
}

type indexCoverageComputeCaseSpec struct {
	Name                   string   `yaml:"name"`
	Kind                   string   `yaml:"kind"`
	Failed                 []string `yaml:"failed"`
	Without                []string `yaml:"without"`
	WantEmpty              int      `yaml:"wantEmpty"`
	WantRetained           int      `yaml:"wantRetained"`
	WantEmptyMembership    []string `yaml:"wantEmptyMembership"`
	WantRetainedMembership []string `yaml:"wantRetainedMembership"`
	WantStoreCalls         int      `yaml:"wantStoreCalls"`
}

var indexCoverageComputeKinds = map[string]bool{
	"split":              true,
	"empty-set":          true,
	"missing-capability": true,
	"query-error":        true,
}

func loadIndexCoverageComputeFixture(data []byte) (indexCoverageComputeDocument, error) {
	var document indexCoverageComputeDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%s: decode typed fields: %w", indexCoverageComputeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found another YAML document")
		}
		return document, fmt.Errorf("%s: exactly one YAML document is allowed: %w", indexCoverageComputeFixturePath, err)
	}
	if len(document.Cases) == 0 {
		return document, fmt.Errorf("%s: the fixture holds no cases", indexCoverageComputeFixturePath)
	}
	if len(document.RequiredCases) == 0 {
		return document, fmt.Errorf("%s: no required cases are named; a fixture with no manifest protects nothing", indexCoverageComputeFixturePath)
	}
	required := map[string]bool{}
	for _, name := range document.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return document, fmt.Errorf("%s: required case name %q is blank or repeated", indexCoverageComputeFixturePath, name)
		}
		required[name] = true
	}
	seen := map[string]bool{}
	for _, spec := range document.Cases {
		if strings.TrimSpace(spec.Name) == "" || seen[spec.Name] {
			return document, fmt.Errorf("%s: case name %q is missing or duplicated", indexCoverageComputeFixturePath, spec.Name)
		}
		seen[spec.Name] = true
		if !indexCoverageComputeKinds[spec.Kind] {
			return document, fmt.Errorf("%s: case %q names the unknown kind %q", indexCoverageComputeFixturePath, spec.Name, spec.Kind)
		}
		switch spec.Kind {
		case "split":
			if len(spec.Failed) == 0 {
				return document, fmt.Errorf("%s: case %q names no failed sessions", indexCoverageComputeFixturePath, spec.Name)
			}
			if spec.WantEmpty+spec.WantRetained != len(spec.Failed) {
				return document, fmt.Errorf("%s: case %q states wantEmpty=%d + wantRetained=%d, which is not the %d failed session(s)", indexCoverageComputeFixturePath, spec.Name, spec.WantEmpty, spec.WantRetained, len(spec.Failed))
			}
			if len(spec.WantEmptyMembership) != spec.WantEmpty || len(spec.WantRetainedMembership) != spec.WantRetained {
				return document, fmt.Errorf("%s: case %q names %d empty and %d retained membership entries, want %d and %d", indexCoverageComputeFixturePath, spec.Name, len(spec.WantEmptyMembership), len(spec.WantRetainedMembership), spec.WantEmpty, spec.WantRetained)
			}
		case "empty-set":
			if len(spec.Failed) != 0 || spec.WantStoreCalls != 0 {
				return document, fmt.Errorf("%s: case %q is an empty-set case and must name no failed sessions and no store calls", indexCoverageComputeFixturePath, spec.Name)
			}
		case "missing-capability", "query-error":
			if len(spec.Failed) == 0 {
				return document, fmt.Errorf("%s: case %q must name the failed session(s) it asks about", indexCoverageComputeFixturePath, spec.Name)
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
		return document, fmt.Errorf("%s: required case %q is missing", indexCoverageComputeFixturePath, missing)
	}
	if extra != "" {
		return document, fmt.Errorf("%s: case %q is not in the required manifest", indexCoverageComputeFixturePath, extra)
	}
	return document, nil
}

func loadIndexCoverageComputeCases(t *testing.T) []indexCoverageComputeCaseSpec {
	t.Helper()
	document, err := loadIndexCoverageComputeFixture(indexCoverageComputeFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestLoadIndexCoverageComputeFixture_RejectsAManifestThatDoesNotMatchTheCases
// proves the required-name manifest is enforced: renaming a required name
// without renaming its case goes red.
func TestLoadIndexCoverageComputeFixture_RejectsAManifestThatDoesNotMatchTheCases(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(indexCoverageComputeFixtureData, []byte("empty-failed-set-measures-zero"), []byte("empty-failed-set-renamed"), 1)
	if _, err := loadIndexCoverageComputeFixture(mutated); err == nil {
		t.Fatal("the loader accepted a manifest whose names do not match its cases; a deletion could then hide behind the counts")
	}
}

func parseCoverageSessionIDs(t *testing.T, raws []string) []ingest.SessionID {
	t.Helper()
	ids := make([]ingest.SessionID, 0, len(raws))
	for _, raw := range raws {
		id, err := ingest.NewSessionID(raw)
		if err != nil {
			t.Fatalf("NewSessionID(%q): %v", raw, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// stubCoverageReader answers entries membership from a fixed map, or fails.
type stubCoverageReader struct {
	without map[ingest.SessionID]bool
	err     error
	calls   int
}

func (s *stubCoverageReader) SessionsWithoutEntries(_ context.Context, ids []ingest.SessionID) (map[ingest.SessionID]bool, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	out := make(map[ingest.SessionID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.without[id]
	}
	return out, nil
}

func assertCoverageIdentity(t *testing.T, coverage *ingest.IndexCoverage) {
	t.Helper()
	if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
		t.Errorf("FailedAttempts = %d, but Empty + FailedRetained = %d + %d; the three must always agree",
			coverage.FailedAttempts, coverage.Empty, coverage.FailedRetained)
	}
}

// TestComputeIndexCoverage drives every named fixture case through the
// production computation and pins the independent counts and the aggregation
// identity. The membership lists below are a fixture-consistency check, not
// production output: ComputeIndexCoverage reports counts only, so the loop
// recomputes the split from the same reader answer the fixture supplies. The
// real-store integration test covers membership end-to-end.
func TestComputeIndexCoverage(t *testing.T) {
	for _, spec := range loadIndexCoverageComputeCases(t) {
		spec := spec
		t.Run(spec.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			failed := parseCoverageSessionIDs(t, spec.Failed)
			switch spec.Kind {
			case "empty-set":
				reader := &stubCoverageReader{}
				coverage, err := ingest.ComputeIndexCoverage(ctx, reader, nil)
				if err != nil {
					t.Fatalf("ComputeIndexCoverage: %v", err)
				}
				if coverage == nil {
					t.Fatal("ComputeIndexCoverage answered nil for an empty failed set; nil means unavailable, and there is nothing unavailable about zero failures")
				}
				if *coverage != (ingest.IndexCoverage{}) {
					t.Errorf("ComputeIndexCoverage = %+v, want all zeros", coverage)
				}
				if reader.calls != spec.WantStoreCalls {
					t.Errorf("ComputeIndexCoverage consulted the store %d time(s) for zero failed sessions", reader.calls)
				}
			case "split":
				without := map[ingest.SessionID]bool{}
				for _, raw := range spec.Without {
					without[ingest.SessionID(raw)] = true
				}
				coverage, err := ingest.ComputeIndexCoverage(ctx, &stubCoverageReader{without: without}, failed)
				if err != nil {
					t.Fatalf("ComputeIndexCoverage: %v", err)
				}
				if coverage == nil {
					t.Fatal("ComputeIndexCoverage answered nil for a readable store")
				}
				assertCoverageIdentity(t, coverage)
				if coverage.FailedAttempts != len(failed) || coverage.Empty != spec.WantEmpty || coverage.FailedRetained != spec.WantRetained {
					t.Errorf("ComputeIndexCoverage = %+v, want {FailedAttempts:%d Empty:%d FailedRetained:%d}", coverage, len(failed), spec.WantEmpty, spec.WantRetained)
				}
				// Fixture consistency, not production output:
				// ComputeIndexCoverage answers with counts, so this recomputes
				// the split from the same without map the stub reader is built
				// from. It only confirms the fixture's membership lists agree
				// with its own failed list and reader answer; the counts and
				// identity above exercise production, and real-store
				// membership is covered end-to-end by the integration test.
				gotEmpty, gotRetained := []string{}, []string{}
				for _, id := range failed {
					if without[id] {
						gotEmpty = append(gotEmpty, string(id))
					} else {
						gotRetained = append(gotRetained, string(id))
					}
				}
				if fmt.Sprintf("%v", gotEmpty) != fmt.Sprintf("%v", spec.WantEmptyMembership) {
					t.Errorf("empty membership = %v, want %v", gotEmpty, spec.WantEmptyMembership)
				}
				if fmt.Sprintf("%v", gotRetained) != fmt.Sprintf("%v", spec.WantRetainedMembership) {
					t.Errorf("retained membership = %v, want %v", gotRetained, spec.WantRetainedMembership)
				}
			case "missing-capability":
				coverage, err := ingest.ComputeIndexCoverage(ctx, nil, failed)
				if err == nil {
					t.Fatal("ComputeIndexCoverage succeeded without a membership capability; a missing capability must leave coverage unavailable, not zero")
				}
				if coverage != nil {
					t.Errorf("ComputeIndexCoverage = %+v, want nil: unavailable is nil, never zero", coverage)
				}
			case "query-error":
				sentinel := errors.New("synthetic membership failure")
				coverage, err := ingest.ComputeIndexCoverage(ctx, &stubCoverageReader{err: sentinel}, failed)
				if !errors.Is(err, sentinel) {
					t.Fatalf("ComputeIndexCoverage error = %v, want it to wrap the store failure", err)
				}
				if coverage != nil {
					t.Errorf("ComputeIndexCoverage = %+v, want nil: a failed chunk must not leave a partial count behind", coverage)
				}
			default:
				t.Fatalf("case names the unhandled kind %q", spec.Kind)
			}
		})
	}
}
