package ingest

import (
	"context"
	_ "embed"
	"strings"
	"testing"
	"time"

	"github.com/peasant-labs/schema"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/adapter_stamp_cases.yaml
var adapterStampCasesYAML []byte

const (
	adapterStampReasonUnspecified RefreshReason = ""
	adapterStampLaneUnspecified   RefreshLane   = ""
)

type adapterStampCase struct {
	Name                 string            `yaml:"name"`
	StoredRevisionOffset *int              `yaml:"storedRevisionOffset"`
	Want                 fixtureDiffStatus `yaml:"want"`
	CaptureMissing       bool              `yaml:"captureMissing"`
	WantReason           RefreshReason     `yaml:"wantReason"`
	WantLane             RefreshLane       `yaml:"wantLane"`
}

type adapterStampFixture struct {
	RequiredNames   []string `yaml:"requiredNames"`
	CaptureRevision int64    `yaml:"captureRevision"`
	TargetSets      []struct {
		Name              string `yaml:"name"`
		GenerationOverlay bool   `yaml:"generationOverlay"`
	} `yaml:"targetSets"`
	Cases []adapterStampCase `yaml:"cases"`
}

func loadAdapterStampFixture(t *testing.T) adapterStampFixture {
	t.Helper()
	var fixture adapterStampFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(adapterStampCasesYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("load adapter stamp cases: %v", err)
	}
	seen := make(map[string]bool, len(fixture.Cases))
	for _, testCase := range fixture.Cases {
		if testCase.Name == "" || seen[testCase.Name] {
			t.Fatalf("invalid adapter stamp case %q", testCase.Name)
		}
		seen[testCase.Name] = true
		if _, err := testCase.Want.Status(); err != nil {
			t.Fatalf("adapter stamp case %q: %v", testCase.Name, err)
		}
		if testCase.Want != fixtureDiffUpdated && testCase.Want != fixtureDiffUnchanged {
			t.Fatalf("adapter stamp case %q wants %q; use %q or %q", testCase.Name, testCase.Want, fixtureDiffUpdated, fixtureDiffUnchanged)
		}
		if testCase.CaptureMissing {
			if _, err := NewRefreshReason(string(testCase.WantReason)); err != nil {
				t.Fatalf("adapter stamp case %q reason: %v", testCase.Name, err)
			}
			if _, err := NewRefreshLane(string(testCase.WantLane)); err != nil {
				t.Fatalf("adapter stamp case %q lane: %v", testCase.Name, err)
			}
		} else if testCase.WantReason != adapterStampReasonUnspecified || testCase.WantLane != adapterStampLaneUnspecified {
			t.Fatalf("adapter stamp case %q supplies a decision without captureMissing; remove it or enable captureMissing", testCase.Name)
		}
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("adapter stamp cases declare no required names; list every case the fixture must retain")
	}
	if fixture.CaptureRevision <= 0 {
		t.Fatal("adapter stamp cases must seed an existing publication capture")
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing adapter stamp case %q", name)
		}
	}
	overlays := 0
	seenSets := make(map[string]bool, len(fixture.TargetSets))
	for _, set := range fixture.TargetSets {
		if set.Name == "" || seenSets[set.Name] {
			t.Fatalf("invalid adapter stamp target set %q", set.Name)
		}
		seenSets[set.Name] = true
		if set.GenerationOverlay {
			overlays++
		}
	}
	if len(fixture.TargetSets) < 2 || overlays == 0 || overlays == len(fixture.TargetSets) {
		t.Fatal("adapter stamp cases must cover the managed-generation overlay and the baseline registry")
	}
	return fixture
}

// TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent protects: identical
// bytes republish once while the stored adapter revision is behind this build's
// target, a NULL stamp counts as baseline 1, and the row settles once the
// revision matches. Both target sets are covered because a generation-capable
// store and a store-free harvest resolve different OpenCode revisions.
//
// This pins the captured classifier only. The harvest test proves the status
// reaches the write that stamps the revision.
func TestCapturedSourceSettlesOnlyWhenTheAdapterStampIsCurrent(t *testing.T) {
	fixture := loadAdapterStampFixture(t)
	if GenerationTargets(HarvesterVersionRegistry)[HarnessOpenCode].AdapterVersion ==
		HarvesterVersionRegistry[HarnessOpenCode].AdapterVersion {
		t.Fatal("the managed-generation and baseline targets are equal; the two target sets would test the same threshold")
	}
	sessionID, err := NewSessionID("ses_3cd91f52effeXd3QAJ54jStmp")
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := []byte("captured-source-fingerprint")
	ingested := time.Now().Add(-time.Hour).UnixMilli()
	for _, targetSet := range fixture.TargetSets {
		t.Run(targetSet.Name, func(t *testing.T) {
			targets := HarvesterVersionRegistry
			if targetSet.GenerationOverlay {
				targets = GenerationTargets(HarvesterVersionRegistry)
			}
			target := targets[HarnessOpenCode].AdapterVersion
			for _, testCase := range fixture.Cases {
				t.Run(testCase.Name, func(t *testing.T) {
					var storedRevision *int
					if testCase.StoredRevisionOffset != nil {
						stored := target + *testCase.StoredRevisionOffset
						storedRevision = &stored
					}
					outputDir, err := NewResolvedPath(t.TempDir())
					if err != nil {
						t.Fatal(err)
					}
					pipeline := &Pipeline{
						fs:            &OSFileSystem{},
						config:        PipelineConfig{OutputDir: outputDir},
						locationCache: make(map[SessionID]SessionLocation, 1),
					}
					pipeline.resolvedVersions = targets
					pipeline.locationCache[sessionID] = SessionLocation{
						IngestedMs:              &ingested,
						SchemaVersion:           CurrentSchemaVersion,
						AdapterVersion:          storedRevision,
						SourceEvidenceSupported: true,
						SourceFingerprint:       fingerprint,
						PublicationReadiness:    PublicationReady,
						CaptureRevision:         fixture.CaptureRevision,
						PublicationBound:        true,
					}
					if testCase.CaptureMissing {
						location := pipeline.locationCache[sessionID]
						location.CaptureRevision = 0
						location.PublicationBound = false
						location.PublicationReadiness = PublicationNeedsIngest
						pipeline.locationCache[sessionID] = location
					}
					session := DiscoveredSession{
						SessionID:        sessionID,
						Harness:          HarnessOpenCode,
						TranscriptOrigin: TranscriptOriginOpenCodeCurrentSQLite,
						SourceFormat:     SourceFormatJSON,
					}
					captured := &MaterializedTranscript{
						Metadata:          &UnifiedMetadata{SessionID: schema.SessionID(string(sessionID))},
						Data:              []byte("{}\n"),
						SourceFingerprint: fingerprint,
					}

					status, err := pipeline.classifyCapturedSession(context.Background(), session, captured)
					if err != nil {
						t.Fatalf("classify captured source: %v", err)
					}
					wantStatus, _ := testCase.Want.Status() // validated by the fixture loader
					if status != wantStatus {
						t.Fatalf("status = %v, want %v", status, wantStatus)
					}
					if testCase.CaptureMissing {
						decision := pipeline.classificationDecision(session, captured)
						if decision.Reason != testCase.WantReason || decision.Lane != testCase.WantLane {
							t.Fatalf("missing-capture decision = %s/%s, want %s/%s", decision.Reason, decision.Lane, testCase.WantReason, testCase.WantLane)
						}
						// Model the capture persisted by the native write; the
						// production pipeline fixture verifies that write itself.
						location := pipeline.locationCache[sessionID]
						location.CaptureRevision = fixture.CaptureRevision
						location.PublicationBound = true
						location.PublicationReadiness = PublicationReady
						pipeline.locationCache[sessionID] = location
						settled, err := pipeline.classifyCapturedSession(context.Background(), session, captured)
						if err != nil || settled != DiffUnchanged {
							t.Fatalf("after capture, status = %v, err = %v; want unchanged", settled, err)
						}
					}
				})
			}
		})
	}
}
