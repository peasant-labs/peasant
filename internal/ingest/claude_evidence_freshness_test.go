package ingest_test

// Freshness fixture family for the one condition this slice adds to Fresh
// (internal/ingest/claude_evidence.go): a cache row whose origin is the empty
// "predates the field" marker is never fresh. See testdata/origin/freshness.yaml
// for the four cases (root/subagent x classified-stays-fresh/pre-origin-is-stale).
//
// This reuses claudeEvidenceCacheRoot, claudeEvidenceFile, and
// writeClaudeEvidenceFiles from claude_evidence_cache_test.go (same package),
// and openEvidenceStore to get a REAL store.Store as the cache — the same
// discipline as the SQL round trip in internal/store/migration_v46_test.go:
// this proves the condition against the real Fresh predicate and a real mined
// record, not a hand-built struct that might not match what discovery
// actually produces.

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/origin/freshness.yaml
var claudeOriginFreshnessFixtureBytes []byte

type claudeOriginFreshnessFixture struct {
	RequiredCaseNames []string                    `yaml:"required_case_names"`
	Cases             []claudeOriginFreshnessCase `yaml:"cases"`
}

type claudeOriginFreshnessCase struct {
	Name          string                     `yaml:"name"`
	Scope         ingest.ClaudeEvidenceScope `yaml:"scope"`
	CorruptOrigin bool                       `yaml:"corrupt_origin"`
	WantFresh     bool                       `yaml:"want_fresh"`
	SourcePath    string                     `yaml:"source_path"`
	Files         []claudeEvidenceFile       `yaml:"files"`
}

func loadClaudeOriginFreshnessFixture(t *testing.T) claudeOriginFreshnessFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(claudeOriginFreshnessFixtureBytes))
	decoder.KnownFields(true)
	var fixture claudeOriginFreshnessFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode Claude origin freshness fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("Claude origin freshness fixture must contain exactly one YAML document: %v", err)
	}

	present := make(map[string]bool, len(fixture.Cases))
	for _, tc := range fixture.Cases {
		present[tc.Name] = true
	}
	if err := testutil.RequireFixtureNames("Claude origin freshness fixture", "case", fixture.RequiredCaseNames, present); err != nil {
		t.Fatal(err)
	}
	return fixture
}

// TestClaudeTranscriptEvidenceFreshRejectsThePreOriginMarker mines each
// fixture case's transcript exactly once through the real adapter and a real
// store-backed cache, so the record's Scope/SizeBytes/ModTimeUnixNano come
// from an actual mined evidence row rather than a hand-built struct. It then
// either leaves the cached Origin as the classifier decided it (expect Fresh)
// or overwrites ONLY the Origin field to the empty marker, through the SAME
// real cache, holding size and mod time exactly equal to the file
// (expect not Fresh) — proving the empty origin is the sole cause.
func TestClaudeTranscriptEvidenceFreshRejectsThePreOriginMarker(t *testing.T) {
	fixture := loadClaudeOriginFreshnessFixture(t)
	for _, tc := range fixture.Cases {
		tc := tc
		t.Run(tc.Name, func(t *testing.T) {
			ctx := context.Background()
			database := openEvidenceStore(t, filepath.Join(t.TempDir(), "peasant.db"))

			fs := testutil.NewCountingFS(testutil.NewMemFS())
			writeClaudeEvidenceFiles(t, fs, tc.Files)

			cfg := ingest.SourceConfig{Paths: []ingest.ResolvedPath{claudeEvidenceCacheRoot}, Enabled: true}
			adapter := ingest.NewClaudeAdapter(fs, testutil.DefaultGitResolver(), salt.Salt{})
			ingest.AttachClaudeEvidenceCache(adapter, database)

			if _, err := adapter.Discover(ctx, cfg); err != nil {
				t.Fatalf("mining discovery: %v", err)
			}

			records, err := database.LoadClaudeEvidence(ctx)
			if err != nil {
				t.Fatalf("load cached evidence: %v", err)
			}
			record, ok := records[ingest.ResolvedPath(tc.SourcePath)]
			if !ok {
				t.Fatalf("no cached record for %q after mining; fixture path and adapter directory layout have drifted", tc.SourcePath)
			}
			if record.Scope != tc.Scope {
				t.Fatalf("mined record scope = %q, want %q", record.Scope, tc.Scope)
			}
			if record.Origin == "" {
				t.Fatalf("freshly mined record carries the empty origin marker; every mined record must carry a menu value")
			}
			if err := record.Origin.Validate(); err != nil {
				t.Fatalf("freshly mined record's origin %q does not validate as a menu value: %v", record.Origin, err)
			}

			if tc.CorruptOrigin {
				// Simulate a row written before this build knew about origin:
				// overwrite ONLY Origin, through the real cache, keeping scope,
				// size, and mod time exactly as mined.
				record.Origin = ""
				if err := database.SaveClaudeEvidence(ctx, []ingest.ClaudeTranscriptEvidence{record}, nil); err != nil {
					t.Fatalf("write back the pre-origin marker: %v", err)
				}
				records, err = database.LoadClaudeEvidence(ctx)
				if err != nil {
					t.Fatalf("reload cached evidence: %v", err)
				}
				record = records[ingest.ResolvedPath(tc.SourcePath)]
				if record.Origin != "" {
					t.Fatalf("expected the corrupted record to carry the empty marker, got %q", record.Origin)
				}
			}

			info, err := fs.Stat(tc.SourcePath)
			if err != nil {
				t.Fatalf("stat %q: %v", tc.SourcePath, err)
			}
			if got := record.Fresh(tc.Scope, info); got != tc.WantFresh {
				t.Errorf("Fresh(%q, info) = %t, want %t (origin=%q, size=%d==%d, modTime=%d==%d)",
					tc.Scope, got, tc.WantFresh, record.Origin,
					record.SizeBytes, info.Size(), record.ModTimeUnixNano, info.ModTime().UnixNano())
			}

			// Belt-and-suspenders: run discovery again and confirm the
			// end-to-end behaviour agrees with the direct Fresh() call. A
			// stale record is re-mined (assigned a fresh, non-empty origin
			// again); a fresh record's session still reports the SAME origin
			// it carried before this run, unchanged.
			fs.ResetCounts()
			sessions, err := adapter.Discover(ctx, cfg)
			if err != nil {
				t.Fatalf("second discovery: %v", err)
			}
			var found bool
			for _, session := range sessions {
				if string(session.SourcePath) == tc.SourcePath {
					found = true
					if session.Origin == "" {
						t.Errorf("session for %q declares the empty origin after a second discovery; every discovered session must carry a menu value", tc.SourcePath)
					}
					if !tc.CorruptOrigin && session.Origin != record.Origin {
						t.Errorf("a fresh cache row changed origin across a second discovery: got %q, want the still-cached %q", session.Origin, record.Origin)
					}
				}
			}
			if !found && tc.Scope == ingest.ClaudeEvidenceScopeRoot {
				t.Fatalf("second discovery did not report a session for root transcript %q", tc.SourcePath)
			}
		})
	}
}
