package ingest

import (
	"bytes"
	_ "embed"
	"io"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/metadata_refresh_versions.yaml
var metadataRefreshVersionsYAML []byte

// metadataRefreshOutcome is what the rule owes a stored sidecar: rebuild it
// from native data, or read it as it stands. There is no third answer.
type metadataRefreshOutcome string

const (
	// metadataRefreshRebuild: the sidecar is older than something required.
	metadataRefreshRebuild metadataRefreshOutcome = "refresh"
	// metadataRefreshKeep: the sidecar is readable as it stands, so
	// re-harvesting it would cost a full pass and change nothing.
	metadataRefreshKeep metadataRefreshOutcome = "keep"
)

type metadataRefreshCase struct {
	Name     string                 `yaml:"name"`
	Recorded int                    `yaml:"recorded"`
	Current  int                    `yaml:"current"`
	Want     metadataRefreshOutcome `yaml:"want"`
}

func loadMetadataRefreshCases(t *testing.T) []metadataRefreshCase {
	t.Helper()
	var document struct {
		RequiredNames []string              `yaml:"requiredNames"`
		Cases         []metadataRefreshCase `yaml:"cases"`
	}
	decoder := yaml.NewDecoder(bytes.NewReader(metadataRefreshVersionsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatalf("decode the metadata refresh version fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		t.Fatalf("the metadata refresh version fixture must hold exactly one YAML document: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	bumped := false
	for _, row := range document.Cases {
		if row.Name == "" || present[row.Name] {
			t.Fatalf("the metadata refresh version fixture has an empty or repeated case name %q", row.Name)
		}
		present[row.Name] = true
		switch row.Want {
		case metadataRefreshRebuild, metadataRefreshKeep:
		default:
			t.Fatalf("fixture case %q wants the unknown outcome %q; a stored sidecar is either refreshed or read as it stands", row.Name, row.Want)
		}
		if row.Current > int(CurrentSchemaVersion) {
			bumped = true
		}
	}
	// Every declared refresh-free version must be proved to earn its place: a
	// case that records it under a HIGHER current version is the only thing
	// that tells a real claim from an inert one, because a version at or above
	// the current one is never refreshed anyway.
	for version := range refreshFreeMetadataVersions {
		load := false
		for _, row := range document.Cases {
			if row.Recorded == int(version) && row.Current > int(version) && row.Want == metadataRefreshKeep {
				load = true
			}
		}
		if !load {
			t.Fatalf("version %d is declared refresh-free but no case records it under a higher current version; an unexercised entry claims a contract nobody checked", version)
		}
	}
	if !bumped {
		t.Fatalf("no fixture case states a current version above %d; without one, a rule written against today's current version would pass and only break at the next contract re-pin, which is the failure this corpus exists to catch", CurrentSchemaVersion)
	}
	for _, required := range document.RequiredNames {
		if !present[required] {
			t.Fatalf("required fixture case %q is missing; the rule it pins would stop being tested", required)
		}
	}
	return document.Cases
}

// TestMetadataRefreshFollowsTheDeclaredVersionSet pins that a contract re-pin
// which adds nothing required does not mark a whole stored corpus stale. The
// rule reads a declared set of versions, so it is stated against a current
// version this build does not write yet as well as against today's, and the
// current version is a parameter: no global is changed to run these cases.
func TestMetadataRefreshFollowsTheDeclaredVersionSet(t *testing.T) {
	for _, row := range loadMetadataRefreshCases(t) {
		t.Run(row.Name, func(t *testing.T) {
			recorded, err := newMetadataSchemaVersion(row.Recorded)
			if err != nil {
				t.Fatal(err)
			}
			current, err := newMetadataSchemaVersion(row.Current)
			if err != nil {
				t.Fatal(err)
			}
			got := metadataRefreshKeep
			if metadataNeedsRefresh(recorded, current) {
				got = metadataRefreshRebuild
			}
			if got != row.Want {
				t.Fatalf("a sidecar recorded at version %d under a build writing version %d is %q, want %q; re-harvesting a corpus for a contract change that added nothing required costs the user a full pass and shows them nothing new",
					row.Recorded, row.Current, got, row.Want)
			}
		})
	}
}

// TestMetadataSchemaVersionRefusesAnImpossibleRecording pins the boundary: a
// number no build ever wrote is refused where it enters, rather than comparing
// as older than every version and quietly forcing a rebuild.
func TestMetadataSchemaVersionRefusesAnImpossibleRecording(t *testing.T) {
	if _, err := newMetadataSchemaVersion(-1); err == nil {
		t.Fatal("a negative recorded metadata schema version was admitted; it would then compare as older than every named version and silently force a rebuild")
	}
	// The live caller turns that refusal into the safe answer rather than
	// certifying a version it cannot name.
	if !metadataNeedsNativeRefresh(-1) {
		t.Fatal("an unreadable recorded version was treated as current; the safe answer is the reporting refresh path, which preserves the existing artifact and index")
	}
}
