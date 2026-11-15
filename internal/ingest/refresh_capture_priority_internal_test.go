package ingest

import (
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/refresh_capture_priority.yaml
var refreshCapturePriorityYAML []byte

// TestRefreshLaneCapturePriority checks the evidence boundary and reason
// precedence; the production-pipeline fixture checks acquisition and binding.
func TestRefreshLaneCapturePriority(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name        string `yaml:"name"`
			Mutation    string `yaml:"mutation"`
			WantMissing bool   `yaml:"want_missing"`
			Reason      string `yaml:"reason"`
			Lane        string `yaml:"lane"`
		} `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(refreshCapturePriorityYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixture.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or repeated fixture name %q", c.Name)
		}
		names[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			ingested := int64(1)
			location := SessionLocation{
				IngestedMs: &ingested, SourceEvidenceSupported: true,
				SourceFingerprint: []byte("fingerprint"), SchemaVersion: CurrentSchemaVersion,
			}
			switch c.Mutation {
			case "unsupported":
				location.SourceEvidenceSupported = false
			case "no-fingerprint":
				location.SourceFingerprint = nil
			case "captured":
				location.CaptureRevision = 1
			}
			pipeline := &Pipeline{}
			evidence := pipeline.refreshEvidenceForLocation(DiscoveredSession{}, location, nil)
			evidence.AdapterVersion, evidence.AdapterTarget = 1, 1
			evidence.IndexerVersion, evidence.IndexerTarget = 1, 1
			evidence.IndexProofPresent = true
			switch c.Mutation {
			case "none", "unsupported", "no-fingerprint", "captured":
			case "forced":
				evidence.Forced = true
			case "new":
				evidence.StoredRow = false
			case "pair-missing":
				evidence.PairMissing = true
			case "source-changed":
				evidence.SourceChanged = true
			case "clock-moved":
				evidence.SourceClockMoved = true
			case "capture-recovery":
				evidence.CaptureRecoveryNeeded = true
			case "source-unproven":
				evidence.SourceUnproven = true
			case "old-metadata":
				evidence.MetadataSchemaOld = true
			case "adapter-behind":
				evidence.AdapterTarget = 2
				evidence.AdapterCanExtract = true
			case "indexer-behind":
				evidence.IndexerTarget = 2
			default:
				t.Fatalf("unknown fixture mutation %q", c.Mutation)
			}
			if evidence.PublicationCaptureMissing != c.WantMissing {
				t.Fatalf("capture missing = %t, want %t", evidence.PublicationCaptureMissing, c.WantMissing)
			}
			decision := decideRefresh(evidence)
			if string(decision.Reason) != c.Reason || string(decision.Lane) != c.Lane {
				t.Fatalf("decision = %s/%s, want %s/%s", decision.Reason, decision.Lane, c.Reason, c.Lane)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !names[name] {
			t.Errorf("required fixture case %q is missing", name)
		}
	}
}
