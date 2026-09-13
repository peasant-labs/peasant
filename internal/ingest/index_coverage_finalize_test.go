package ingest

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/index_coverage_finalize.yaml
var indexCoverageFinalizeFixtureData []byte

const indexCoverageFinalizeFixturePath = "internal/ingest/testdata/index_coverage_finalize.yaml"

type indexCoverageFinalizeDocument struct {
	RequiredCases []string                        `yaml:"required_cases"`
	Cases         []indexCoverageFinalizeCaseSpec `yaml:"cases"`
}

type indexCoverageFinalizeCaseSpec struct {
	Name               string                `yaml:"name"`
	LogPrefix          string                `yaml:"logPrefix"`
	Rows               []indexCoverageLogRow `yaml:"rows"`
	Without            []string              `yaml:"without"`
	PartialWithout     []string              `yaml:"partialWithout"`
	StoreError         bool                  `yaml:"storeError"`
	AbsentStore        bool                  `yaml:"absentStore"`
	WantUnavailable    bool                  `yaml:"wantUnavailable"`
	WantFailedAttempts int                   `yaml:"wantFailedAttempts"`
	WantEmpty          int                   `yaml:"wantEmpty"`
	WantRetained       int                   `yaml:"wantRetained"`
}

type indexCoverageLogRow struct {
	Session string `yaml:"session"`
	Outcome string `yaml:"outcome"`
}

// coverageFinalizeOutcomes maps the fixture's spelling to the production
// outcome. Explicit, because a blank or misspelled value would otherwise
// decode to the empty outcome, which counts as neither a failure nor a
// success - a row could then silently test nothing while reading as a case.
var coverageFinalizeOutcomes = map[string]IndexOutcome{
	"indexed":   IndexOutcomeIndexed,
	"reindexed": IndexOutcomeReindexed,
	"fallback":  IndexOutcomeFallback,
	"skipped":   IndexOutcomeSkipped,
	"error":     IndexOutcomeError,
}

func loadIndexCoverageFinalizeFixture(data []byte) (indexCoverageFinalizeDocument, error) {
	var document indexCoverageFinalizeDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf("%s: decode typed fields: %w", indexCoverageFinalizeFixturePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("found another YAML document")
		}
		return document, fmt.Errorf("%s: exactly one YAML document is allowed: %w", indexCoverageFinalizeFixturePath, err)
	}
	if len(document.Cases) == 0 {
		return document, fmt.Errorf("%s: the fixture holds no cases", indexCoverageFinalizeFixturePath)
	}
	if len(document.RequiredCases) == 0 {
		return document, fmt.Errorf("%s: no required cases are named; a fixture with no manifest protects nothing", indexCoverageFinalizeFixturePath)
	}
	required := map[string]bool{}
	for _, name := range document.RequiredCases {
		if strings.TrimSpace(name) == "" || required[name] {
			return document, fmt.Errorf("%s: required case name %q is blank or repeated", indexCoverageFinalizeFixturePath, name)
		}
		required[name] = true
	}
	seen := map[string]bool{}
	for _, spec := range document.Cases {
		if strings.TrimSpace(spec.Name) == "" || seen[spec.Name] {
			return document, fmt.Errorf("%s: case name %q is missing or duplicated", indexCoverageFinalizeFixturePath, spec.Name)
		}
		seen[spec.Name] = true
		if strings.TrimSpace(spec.LogPrefix) == "" {
			return document, fmt.Errorf("%s: case %q has no log prefix", indexCoverageFinalizeFixturePath, spec.Name)
		}
		if len(spec.Rows) == 0 {
			return document, fmt.Errorf("%s: case %q has no log rows", indexCoverageFinalizeFixturePath, spec.Name)
		}
		for _, row := range spec.Rows {
			if _, known := coverageFinalizeOutcomes[row.Outcome]; !known {
				return document, fmt.Errorf("%s: case %q names the outcome %q, which the pipeline does not record", indexCoverageFinalizeFixturePath, spec.Name, row.Outcome)
			}
		}
		if spec.AbsentStore && (spec.StoreError || len(spec.Without) != 0 || len(spec.PartialWithout) != 0) {
			return document, fmt.Errorf("%s: case %q has no store but also configures store behavior", indexCoverageFinalizeFixturePath, spec.Name)
		}
		if spec.WantUnavailable {
			if !spec.AbsentStore && !spec.StoreError {
				return document, fmt.Errorf("%s: case %q expects unavailable coverage without an absent store or a store error", indexCoverageFinalizeFixturePath, spec.Name)
			}
			continue
		}
		if spec.WantFailedAttempts != spec.WantEmpty+spec.WantRetained {
			return document, fmt.Errorf("%s: case %q states wantFailedAttempts=%d, which is not wantEmpty=%d + wantRetained=%d", indexCoverageFinalizeFixturePath, spec.Name, spec.WantFailedAttempts, spec.WantEmpty, spec.WantRetained)
		}
		// A measured split needs a store that can answer. The one exception is
		// a run with no failed attempts: it measures zero without consulting
		// the store at all, so it may configure the store to fail and prove
		// that it is never asked.
		if (spec.AbsentStore || spec.StoreError) && spec.WantFailedAttempts > 0 {
			return document, fmt.Errorf("%s: case %q expects a measured split from a store that cannot answer", indexCoverageFinalizeFixturePath, spec.Name)
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
		return document, fmt.Errorf("%s: required case %q is missing", indexCoverageFinalizeFixturePath, missing)
	}
	if extra != "" {
		return document, fmt.Errorf("%s: case %q is not in the required manifest", indexCoverageFinalizeFixturePath, extra)
	}
	return document, nil
}

func loadIndexCoverageFinalizeCases(t *testing.T) []indexCoverageFinalizeCaseSpec {
	t.Helper()
	document, err := loadIndexCoverageFinalizeFixture(indexCoverageFinalizeFixtureData)
	if err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

func buildIndexCoverageFinalizeLog(rows []indexCoverageLogRow) []IndexLogEntry {
	log := make([]IndexLogEntry, 0, len(rows))
	for _, row := range rows {
		log = append(log, IndexLogEntry{SessionID: SessionID(row.Session), Outcome: coverageFinalizeOutcomes[row.Outcome]})
	}
	return log
}

// coverageFinalizeStore is a SessionStore double that answers entries
// membership from a fixed map, or fails it on demand. A partial map can ride
// with the error so a case can prove finalize discards accumulated rows.
type coverageFinalizeStore struct {
	without map[SessionID]bool
	partial map[SessionID]bool
	err     error
}

func (s *coverageFinalizeStore) InsertSessions(context.Context, []StoreEntry) error {
	return nil
}
func (s *coverageFinalizeStore) LookupSessionLocation(context.Context, SessionID) (string, string, error) {
	return "", "", nil
}
func (s *coverageFinalizeStore) BulkLookupSessionLocations(context.Context, []SessionID) (map[SessionID]SessionLocation, error) {
	return map[SessionID]SessionLocation{}, nil
}
func (s *coverageFinalizeStore) UpsertSessionCommits(context.Context, SessionID, []CommitInfo) error {
	return nil
}
func (s *coverageFinalizeStore) CleanupOrphanProjects(context.Context) error { return nil }
func (s *coverageFinalizeStore) Close() error                                { return nil }
func (s *coverageFinalizeStore) SessionsWithoutEntries(_ context.Context, ids []SessionID) (map[SessionID]bool, error) {
	if s.err != nil {
		if s.partial != nil {
			out := make(map[SessionID]bool, len(s.partial))
			for id, without := range s.partial {
				out[id] = without
			}
			return out, s.err
		}
		return nil, s.err
	}
	out := make(map[SessionID]bool, len(ids))
	for _, id := range ids {
		out[id] = s.without[id]
	}
	return out, nil
}

func coverageFinalizeDiagnostics(p *Pipeline) []DiagnosticEntry {
	return p.snapshotDiagnostics()
}

// coverageFinalizePipeline builds the smallest pipeline that can run the
// production finalize: the real OS filesystem over an empty temp dir (cleanup
// finds no orphans there) plus the given store handle.
func coverageFinalizePipeline(t *testing.T, s SessionStore) *Pipeline {
	t.Helper()
	return &Pipeline{
		fs:     &OSFileSystem{},
		config: PipelineConfig{OutputDir: ResolvedPath(t.TempDir())},
		store:  s,
	}
}

func assertCoverageDiagnostic(t *testing.T, diagnostics []DiagnosticEntry, logPrefix string) {
	t.Helper()
	var found []DiagnosticEntry
	for _, diagnostic := range diagnostics {
		if diagnostic.ErrorType == "index_coverage_unavailable" {
			found = append(found, diagnostic)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one index_coverage_unavailable diagnostic, got %d: %+v", len(found), diagnostics)
	}
	if !strings.Contains(found[0].Location, logPrefix) {
		t.Errorf("diagnostic location = %q, want it to name the %q finalize step", found[0].Location, logPrefix)
	}
	if strings.TrimSpace(found[0].Message) == "" || strings.TrimSpace(found[0].Remediation) == "" {
		t.Errorf("diagnostic must carry an actionable message and remediation, got %+v", found[0])
	}
}

func assertFinalizeCoverageIdentity(t *testing.T, coverage *IndexCoverage) {
	t.Helper()
	if coverage.FailedAttempts != coverage.Empty+coverage.FailedRetained {
		t.Errorf("FailedAttempts = %d, want Empty + FailedRetained = %d", coverage.FailedAttempts, coverage.Empty+coverage.FailedRetained)
	}
}

// TestIndexComputeAndFinalize drives every named fixture case through the
// production finalize: measured splits, the clean-run measured zero, and the
// unavailable paths (missing capability, membership error, and a read that
// fails after accumulating rows). An unavailable path must leave a nil
// coverage and exactly one actionable diagnostic, never a partial count.
func TestIndexComputeAndFinalize(t *testing.T) {
	for _, spec := range loadIndexCoverageFinalizeCases(t) {
		spec := spec
		t.Run(spec.Name, func(t *testing.T) {
			t.Parallel()
			var sessionStore SessionStore
			if !spec.AbsentStore {
				double := &coverageFinalizeStore{without: map[SessionID]bool{}}
				for _, raw := range spec.Without {
					double.without[SessionID(raw)] = true
				}
				if spec.StoreError {
					double.err = errors.New("synthetic membership failure")
				}
				if len(spec.PartialWithout) > 0 {
					double.partial = map[SessionID]bool{}
					for _, raw := range spec.PartialWithout {
						double.partial[SessionID(raw)] = true
					}
				}
				sessionStore = double
			}
			p := coverageFinalizePipeline(t, sessionStore)
			log := buildIndexCoverageFinalizeLog(spec.Rows)

			result, err := p.indexComputeAndFinalize(context.Background(), nil, nil, nil, nil, time.Now(), log, IndexOutcomeIndexed, spec.LogPrefix, nil)
			if err != nil {
				t.Fatalf("indexComputeAndFinalize: %v", err)
			}
			diagnostics := coverageFinalizeDiagnostics(p)
			if spec.WantUnavailable {
				if result.IndexCoverage != nil {
					t.Errorf("IndexCoverage = %+v, want nil: unavailable is nil, never a partial count", result.IndexCoverage)
				}
				assertCoverageDiagnostic(t, diagnostics, spec.LogPrefix)
				return
			}
			if result.IndexCoverage == nil {
				t.Fatal("finalize left IndexCoverage nil for a store that answers membership")
			}
			assertFinalizeCoverageIdentity(t, result.IndexCoverage)
			if result.IndexCoverage.FailedAttempts != spec.WantFailedAttempts || result.IndexCoverage.Empty != spec.WantEmpty || result.IndexCoverage.FailedRetained != spec.WantRetained {
				t.Errorf("IndexCoverage = %+v, want {FailedAttempts:%d Empty:%d FailedRetained:%d}", result.IndexCoverage, spec.WantFailedAttempts, spec.WantEmpty, spec.WantRetained)
			}
			for _, diagnostic := range diagnostics {
				if diagnostic.ErrorType == "index_coverage_unavailable" {
					t.Errorf("measured coverage reported an unavailable diagnostic: %+v", diagnostic)
				}
			}
		})
	}
}
