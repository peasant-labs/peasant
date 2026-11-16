package ingest

import (
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_capture_priority.yaml
var refreshCapturePriorityYAML []byte

type capturePriorityMutation string

const (
	capturePriorityNone                 capturePriorityMutation = "none"
	capturePriorityUnsupported          capturePriorityMutation = "unsupported"
	capturePriorityNoFingerprint        capturePriorityMutation = "no-fingerprint"
	capturePriorityCaptured             capturePriorityMutation = "captured"
	capturePriorityStrictRefusal        capturePriorityMutation = "strict-refusal"
	capturePriorityRefusalIndexerBehind capturePriorityMutation = "refusal-indexer-behind"
	capturePriorityOmittedRefusal       capturePriorityMutation = "omitted-refusal"
	capturePriorityUnknownRefusal       capturePriorityMutation = "unknown-refusal"
	capturePriorityOmittedPublishable   capturePriorityMutation = "omitted-publishable"
	capturePriorityUnknownPublishable   capturePriorityMutation = "unknown-publishable"
	capturePriorityForced               capturePriorityMutation = "forced"
	capturePriorityNew                  capturePriorityMutation = "new"
	capturePriorityPairMissing          capturePriorityMutation = "pair-missing"
	capturePrioritySourceChanged        capturePriorityMutation = "source-changed"
	capturePriorityClockMoved           capturePriorityMutation = "clock-moved"
	capturePriorityRecovery             capturePriorityMutation = "capture-recovery"
	capturePrioritySourceUnproven       capturePriorityMutation = "source-unproven"
	capturePriorityOldMetadata          capturePriorityMutation = "old-metadata"
	capturePriorityAdapterBehind        capturePriorityMutation = "adapter-behind"
	capturePriorityIndexerBehind        capturePriorityMutation = "indexer-behind"
)

func (m capturePriorityMutation) Validate() error {
	switch m {
	case capturePriorityNone, capturePriorityUnsupported, capturePriorityNoFingerprint, capturePriorityCaptured,
		capturePriorityStrictRefusal, capturePriorityRefusalIndexerBehind, capturePriorityOmittedRefusal,
		capturePriorityUnknownRefusal, capturePriorityOmittedPublishable, capturePriorityUnknownPublishable,
		capturePriorityForced, capturePriorityNew, capturePriorityPairMissing,
		capturePrioritySourceChanged, capturePriorityClockMoved, capturePriorityRecovery,
		capturePrioritySourceUnproven, capturePriorityOldMetadata, capturePriorityAdapterBehind,
		capturePriorityIndexerBehind:
		return nil
	default:
		return fmt.Errorf("unknown mutation %q; use a declared capture-priority mutation in refresh_capture_priority.yaml", m)
	}
}

type capturePriorityCase struct {
	Name        string                  `yaml:"name"`
	Mutation    capturePriorityMutation `yaml:"mutation"`
	WantMissing bool                    `yaml:"want_missing"`
	Reason      RefreshReason           `yaml:"reason"`
	Lane        RefreshLane             `yaml:"lane"`
}

type capturePriorityFixture struct {
	RequiredNames []string              `yaml:"requiredNames"`
	Cases         []capturePriorityCase `yaml:"cases"`
}

func decodeCapturePriorityFixture(data []byte) (capturePriorityFixture, error) {
	var fixture capturePriorityFixture
	decoder := yaml.NewDecoder(strings.NewReader(string(data)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return capturePriorityFixture{}, fmt.Errorf("decode refresh_capture_priority.yaml: %w", err)
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || names[c.Name] {
			return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml has an empty or repeated case name %q", c.Name)
		}
		names[c.Name] = true
		if err := c.Mutation.Validate(); err != nil {
			return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml case %s: %w", c.Name, err)
		}
		if _, err := NewRefreshReason(string(c.Reason)); err != nil {
			return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml case %s reason: %w", c.Name, err)
		}
		if _, err := NewRefreshLane(string(c.Lane)); err != nil {
			return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml case %s lane: %w", c.Name, err)
		}
	}
	if len(fixture.RequiredNames) == 0 {
		return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml must list its required cases")
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			return capturePriorityFixture{}, fmt.Errorf("refresh_capture_priority.yaml required case %q is missing", name)
		}
	}
	return fixture, nil
}

// TestRefreshLaneCapturePriority checks the evidence boundary and reason
// precedence; the production-pipeline fixture checks acquisition and binding.
func TestRefreshLaneCapturePriority(t *testing.T) {
	fixture, err := decodeCapturePriorityFixture(refreshCapturePriorityYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range fixture.Cases {
		t.Run(c.Name, func(t *testing.T) {
			ingested := int64(1)
			location := SessionLocation{
				IngestedMs: &ingested, SourceEvidenceSupported: true,
				SourceFingerprint: []byte("fingerprint"), SchemaVersion: CurrentSchemaVersion,
			}
			switch c.Mutation {
			case capturePriorityUnsupported:
				location.SourceEvidenceSupported = false
			case capturePriorityNoFingerprint:
				location.SourceFingerprint = nil
			case capturePriorityCaptured:
				location.CaptureRevision = 1
			case capturePriorityStrictRefusal, capturePriorityRefusalIndexerBehind:
				location.ContentFailureCode = ContentCaptureStrictRefused
			case capturePriorityOmittedRefusal:
				location.ContentFailureCode = ContentCaptureSourceRecordsOmitted
			case capturePriorityUnknownRefusal:
				location.ContentFailureCode = ContentCaptureUnknownDataRetained
			case capturePriorityOmittedPublishable:
				location.ContentFailureCode = ContentCaptureSourceRecordsOmitted
				location.ContentPublishable = true
			case capturePriorityUnknownPublishable:
				location.ContentFailureCode = ContentCaptureUnknownDataRetained
				location.ContentPublishable = true
			}
			pipeline := &Pipeline{}
			evidence := pipeline.refreshEvidenceForLocation(DiscoveredSession{}, location, nil)
			evidence.AdapterVersion, evidence.AdapterTarget = 1, 1
			evidence.IndexerVersion, evidence.IndexerTarget = 1, 1
			evidence.IndexProofPresent = true
			switch c.Mutation {
			case capturePriorityNone, capturePriorityUnsupported, capturePriorityNoFingerprint, capturePriorityCaptured,
				capturePriorityStrictRefusal, capturePriorityOmittedRefusal, capturePriorityUnknownRefusal,
				capturePriorityOmittedPublishable, capturePriorityUnknownPublishable:
			case capturePriorityForced:
				evidence.Forced = true
			case capturePriorityNew:
				evidence.StoredRow = false
			case capturePriorityPairMissing:
				evidence.PairMissing = true
			case capturePrioritySourceChanged:
				evidence.SourceChanged = true
			case capturePriorityClockMoved:
				evidence.SourceClockMoved = true
			case capturePriorityRecovery:
				evidence.CaptureRecoveryNeeded = true
			case capturePrioritySourceUnproven:
				evidence.SourceUnproven = true
			case capturePriorityOldMetadata:
				evidence.MetadataSchemaOld = true
			case capturePriorityAdapterBehind:
				evidence.AdapterTarget = 2
				evidence.AdapterCanExtract = true
			case capturePriorityIndexerBehind:
				evidence.IndexerTarget = 2
			case capturePriorityRefusalIndexerBehind:
				evidence.IndexerTarget = 2
			default:
				t.Fatalf("unknown fixture mutation %q", c.Mutation)
			}
			if evidence.PublicationCaptureMissing != c.WantMissing {
				t.Fatalf("capture missing = %t, want %t", evidence.PublicationCaptureMissing, c.WantMissing)
			}
			decision := decideRefresh(evidence)
			if decision.Reason != c.Reason || decision.Lane != c.Lane {
				t.Fatalf("decision = %s/%s, want %s/%s", decision.Reason, decision.Lane, c.Reason, c.Lane)
			}
		})
	}
}
