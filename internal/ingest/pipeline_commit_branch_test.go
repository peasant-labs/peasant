package ingest_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

// TestPipeline_CommitDetection_SessionBranch runs the branch fixture family
// through the production path. The branch a harness recorded on
// meta.Git.Branch must reach the detector, and the metadata written for the
// session must carry only the commits the branch filter kept.
func TestPipeline_CommitDetection_SessionBranch(t *testing.T) {
	fixture := loadCommitDetectorBranchFixture(t)
	for _, tc := range fixture.Cases {
		t.Run(tc.Name, func(t *testing.T) {
			mfs := testutil.NewMemFS()
			git := testutil.DefaultGitResolver() // email = testutil.TestEmail
			if tc.UserEmailMissing {
				git.Email = ""
			}

			sourcePath := fmt.Sprintf("%s/%s.jsonl", testSourceDir, testSessionID)
			setupSourceFile(t, mfs, sourcePath)

			session := makeDiscoveredSession(t, testSessionID, sourcePath, time.Now().Add(-1*time.Hour))
			meta := makeMinimalMeta(t, testSessionID)
			if tc.SessionBranch != "" {
				branch := tc.SessionBranch
				meta.Git.Branch = &branch
			}

			adapters := map[ingest.Harness]ingest.AdapterFactory{
				ingest.HarnessClaudeCode: makeStubAdapter(
					[]ingest.DiscoveredSession{session},
					map[ingest.SessionID]*ingest.UnifiedMetadata{session.SessionID: meta},
				),
			}
			gitAnalyzer := branchFixtureAnalyzer(t, tc)

			cfg := makePipelineConfig(testOutputDir)
			pipeline, err := ingest.NewPipeline(mfs, git, adapters, cfg, ingest.WithGitDiffAnalyzer(gitAnalyzer))
			if err != nil {
				t.Fatalf("NewPipeline: %v", err)
			}
			result, err := pipeline.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.Summary.Errors != 0 {
				t.Fatalf("Summary.Errors = %d, want 0", result.Summary.Errors)
			}

			written := readMetadataFromMemFS(t, mfs, testSessionID)
			if got := hashesOf(written.Git.Commits); !slices.Equal(got, tc.WantHashes) {
				t.Errorf("written Git.Commits = %v, want exactly %v", got, tc.WantHashes)
			}

			// Other stages add their own warnings here (the in-memory filesystem
			// holds no transcript for the command gate to read), so compare only
			// the branch filter's diagnostics, on both sides.
			gotTypes := branchDiagnosticTypes(errorTypesOf(written.Diagnostics.Warnings))
			wantTypes := branchDiagnosticTypes(tc.WantErrorTypes)
			if !slices.Equal(gotTypes, wantTypes) {
				t.Errorf("branch diagnostics = %v, want exactly %v (%+v)", gotTypes, wantTypes, written.Diagnostics.Warnings)
			}
			if got := gitAnalyzer.AncestorQueries(); got != tc.WantAncestryQueries {
				t.Errorf("IsAncestor was asked %d times, want %d", got, tc.WantAncestryQueries)
			}
		})
	}
}

// branchDiagnosticTypes keeps only the branch filter's diagnostic types, in order.
func branchDiagnosticTypes(types []string) []string {
	kept := make([]string, 0, len(types))
	for _, errorType := range types {
		if strings.HasPrefix(errorType, "branch_") {
			kept = append(kept, errorType)
		}
	}
	return kept
}
