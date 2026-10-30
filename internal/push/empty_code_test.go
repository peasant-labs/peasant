package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/clicontract"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/empty_code.yaml
var emptyCodeFixtureData []byte

//go:embed testdata/empty_code.manifest.yaml
var emptyCodeManifestData []byte

type emptyCodeCase struct {
	Name         string `yaml:"name"`
	World        string `yaml:"world"`
	Scoped       bool   `yaml:"scoped"`
	WantCode     string `yaml:"wantCode"`
	ReasonPrefix string `yaml:"reasonPrefix"`
}

func loadEmptyCodeCases(t *testing.T) []emptyCodeCase {
	t.Helper()
	var document struct {
		Cases []emptyCodeCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(emptyCodeFixtureData))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode empty_code fixture: %v", err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(emptyCodeManifestData, "push empty_code")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(document.Cases))
	for _, c := range document.Cases {
		names = append(names, c.Name)
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "push empty_code"); err != nil {
		t.Fatal(err)
	}
	return document.Cases
}

// TestPipeline_EmptyCodeNamesEachEmptyState pins the closed empty_code the
// pipeline sets for every state in which it sends nothing, scoped and not.
func TestPipeline_EmptyCodeNamesEachEmptyState(t *testing.T) {
	t.Parallel()
	for _, tc := range loadEmptyCodeCases(t) {
		t.Run(tc.Name, func(t *testing.T) {
			t.Parallel()
			want, err := clicontract.NewEmptyCode(tc.WantCode)
			if err != nil {
				t.Fatalf("fixture wantCode: %v", err)
			}
			pushedAt := time.Now().UnixMilli() - 1000
			session := makeSession("11111111-1111-4111-8111-111111111111", testutil.TestHostSlug,
				string(defaults.HarnessClaudeCode), &pushedAt,
				withGitRemote(scopedGitRemote), withGitBranch("main"),
				func(s *ingest.PushSessionRow) { s.ProjectHash = scopedProjectHash })
			var runCfg push.PipelineConfig
			if tc.Scoped {
				runCfg.Repository = testScope(false)
			}
			all := []ingest.PushSessionRow{}
			fs := testutil.NewMemFS()
			switch tc.World {
			case "empty-store":
			case "other-only":
				all = append(all, makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, string(defaults.HarnessClaudeCode), &pushedAt))
			case "all-published":
				all = append(all, session)
				seedMemFSForRow(t, fs, session)
			case "selection-excluded":
				all = append(all, session)
				runCfg.Selection = push.NewSessionSelection(map[ingest.SessionID]ingest.BranchMatch{
					ingest.SessionID(session.SessionID): ingest.BranchMatchNo,
				})
			default:
				t.Fatalf("unknown world %q", tc.World)
			}
			store := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{}, AllSessions: all}
			var stderr bytes.Buffer
			p := newTestPipeline(store, &testutil.StubPublisher{}, fs, baseTestConfig(), runCfg, &stderr)
			if tc.World == "all-published" {
				if first, err := p.Run(context.Background()); err != nil || first.New != 1 {
					t.Fatalf("seed the publication: result=%+v err=%v", first, err)
				}
			}
			result, err := p.Run(context.Background())
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if result.EmptyCode != want {
				t.Errorf("empty_code = %q, want %q (reason %q)", result.EmptyCode, want, result.EmptyReason)
			}
			if !strings.HasPrefix(result.EmptyReason, tc.ReasonPrefix) {
				t.Errorf("empty_reason = %q, want it to start with %q", result.EmptyReason, tc.ReasonPrefix)
			}
		})
	}
}
