package push_test

import (
	"bytes"
	"context"
	_ "embed"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/config"
	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/push"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/first_publish_decision.yaml
var firstPublishDecisionFixture []byte

//go:embed testdata/first_publish_decision.manifest.yaml
var firstPublishDecisionManifest []byte

// earlierProjectHash is a project identity a harvest has since replaced.
const earlierProjectHash = "3333333333333333333333333333333333333333333333333333333333333333"

var (
	allReceiptStates   = []string{"none", "current", "earlier-project"}
	allAttemptStages   = []string{"none", "publish", "visibility", "persist"}
	attemptStageByName = map[string]store.PublicationAttemptStage{
		"publish":    store.PublicationAttemptStagePublish,
		"visibility": store.PublicationAttemptStageVisibility,
		"persist":    store.PublicationAttemptStagePersistence,
	}
)

type firstPublishDecisionCase struct {
	Name              string            `yaml:"name"`
	Receipt           string            `yaml:"receipt"`
	LatestAttempt     string            `yaml:"latestAttempt"`
	VillageAnswer     string            `yaml:"villageAnswer"`
	VillageVisibility schema.Visibility `yaml:"villageVisibility"`
	ExpectLicense     string            `yaml:"expectLicense"`
	ExpectOwnerUpdate string            `yaml:"expectOwnerUpdate"`
}

func loadFirstPublishDecisionFixture(t *testing.T) []firstPublishDecisionCase {
	t.Helper()
	var document struct {
		Cases []firstPublishDecisionCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(firstPublishDecisionFixture, &document); err != nil {
		t.Fatal(err)
	}
	manifest, err := testutil.DecodeRequiredNamesManifest(firstPublishDecisionManifest, "first publish decision")
	if err != nil {
		t.Fatal(err)
	}
	var names, receipts, attempts []string
	for _, c := range document.Cases {
		names = append(names, c.Name)
		receipts = append(receipts, c.Receipt)
		attempts = append(attempts, c.LatestAttempt)
		if !slices.Contains(allReceiptStates, c.Receipt) || !slices.Contains(allAttemptStages, c.LatestAttempt) ||
			(c.VillageAnswer != "created" && c.VillageAnswer != "update") || !c.VillageVisibility.IsValid() ||
			(c.ExpectLicense != "none" && !schema.License(c.ExpectLicense).IsValid()) ||
			(c.ExpectOwnerUpdate != "none" && !schema.TranscriptUpdateVisibility(c.ExpectOwnerUpdate).IsValid()) {
			t.Fatalf("testdata/first_publish_decision.yaml case %q names a value outside its closed sets; every field is required", c.Name)
		}
	}
	if err := testutil.ValidateRequiredNames(manifest, names, "first publish decision"); err != nil {
		t.Fatal(err)
	}
	testutil.RequireClosedSetCoverage(t, "first publish decision", "receipt", allReceiptStates, receipts)
	testutil.RequireClosedSetCoverage(t, "first publish decision", "latestAttempt", allAttemptStages, attempts)
	return document.Cases
}

// TestPipeline_FirstPublishDecision drives one push per case through the real
// pipeline, over a store and a Village that hold the case's state, and checks
// what reached the Village: the license on the upload, and the owner
// visibility update. It pins each input the decision reads.
func TestPipeline_FirstPublishDecision(t *testing.T) {
	t.Parallel()
	for _, c := range loadFirstPublishDecisionFixture(t) {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			fs := testutil.NewMemFS()
			seedMemFS(t, fs, testutil.TestHostSlug, testutil.TestSessionUUID, defaults.HarnessClaudeCode)
			storeDouble := &testutil.StubPushStore{Sessions: []ingest.PushSessionRow{makeSession(testutil.TestSessionUUID, testutil.TestHostSlug, defaults.HarnessClaudeCode.String(), nil)}}
			creds := baseCreds()
			if c.Receipt != "none" {
				hash := testutil.TestProjectHash
				if c.Receipt == "earlier-project" {
					hash = schema.ProjectHash(earlierProjectHash)
				}
				if err := storeDouble.SavePublication(context.Background(), store.PublicationRecord{VillageOrigin: creds.VillageURL, OwnerUserID: creds.UserID, SessionID: testutil.TestSessionUUID, ProjectHash: hash}); err != nil {
					t.Fatal(err)
				}
				storeDouble.SavedPublicationIDs = nil
			}
			if c.LatestAttempt != "none" {
				if err := storeDouble.RecordPublicationAttempt(context.Background(), store.PublicationAttemptDiagnostic{VillageOrigin: creds.VillageURL, OwnerUserID: creds.UserID, SessionID: testutil.TestSessionUUID, ProjectHash: testutil.TestProjectHash, Stage: attemptStageByName[c.LatestAttempt], Message: "an earlier attempt failed"}); err != nil {
					t.Fatal(err)
				}
			}
			publisher := &testutil.StubPublisher{StatusCode: 201, ReceiptVisibility: c.VillageVisibility}
			if c.VillageAnswer == "update" {
				publisher.StatusCode = 200
			}
			cfg := baseTestConfig()
			cfg.Push.Visibility = config.VisibilityPublic
			cfg.Push.License = schema.License("CC-BY-4.0")
			var stderr bytes.Buffer
			result, err := newTestPipeline(storeDouble, publisher, fs, cfg, push.PipelineConfig{}, &stderr).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result.Errors != 0 || len(publisher.AuthoritativeCalls) != 1 {
				t.Fatalf("one upload must succeed; result=%+v uploads=%d", result, len(publisher.AuthoritativeCalls))
			}
			wantLicense := schema.License("")
			if c.ExpectLicense != "none" {
				wantLicense = schema.License(c.ExpectLicense)
			}
			if got := publisher.AuthoritativeCalls[0].License; got != wantLicense {
				t.Errorf("the upload carried license %q; want %q", got, wantLicense)
			}
			var updates []string
			for _, update := range publisher.OwnerUpdates {
				if update.Visibility != nil {
					updates = append(updates, update.Visibility.String())
				}
			}
			var wantUpdates []string
			if c.ExpectOwnerUpdate != "none" {
				wantUpdates = []string{c.ExpectOwnerUpdate}
			}
			if !slices.Equal(updates, wantUpdates) {
				t.Errorf("the Village received owner visibility updates %v; want %v", updates, wantUpdates)
			}
		})
	}
}
