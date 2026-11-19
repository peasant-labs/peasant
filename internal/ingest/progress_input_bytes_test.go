package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/progress_input_bytes.yaml
var progressInputBytesYAML []byte

//go:embed testdata/progress_input_bytes_invalid.yaml
var invalidProgressInputBytesYAML []byte

type progressInputKind string

const (
	progressInputStored  progressInputKind = "stored"
	progressInputLegacy  progressInputKind = "legacy"
	progressInputCurrent progressInputKind = "current"
	progressInputTree    progressInputKind = "tree"
)

type progressInputBytesFixture struct {
	RequiredNames []string `yaml:"requiredNames"`
	Cases         []struct {
		Name      string            `yaml:"name"`
		Kind      progressInputKind `yaml:"kind"`
		Fixture   string            `yaml:"fixture"`
		Session   string            `yaml:"session"`
		Known     bool              `yaml:"known"`
		WantBytes int64             `yaml:"wantBytes"`
	} `yaml:"cases"`
}

func decodeProgressInputBytes(data []byte) (progressInputBytesFixture, error) {
	var fixture progressInputBytesFixture
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&fixture); err != nil {
		return fixture, err
	}
	if len(fixture.RequiredNames) == 0 {
		return fixture, fmt.Errorf("requiredNames must not be empty")
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			return fixture, fmt.Errorf("duplicate or empty case %q", row.Name)
		}
		seen[row.Name] = true
		if row.Known && row.WantBytes <= 0 {
			return fixture, fmt.Errorf("case %q requires a positive wantBytes", row.Name)
		}
	}
	required := map[string]bool{}
	for _, name := range fixture.RequiredNames {
		if name == "" || required[name] {
			return fixture, fmt.Errorf("empty or duplicate required name %q", name)
		}
		required[name] = true
		if !seen[name] {
			return fixture, fmt.Errorf("missing required case %q", name)
		}
	}
	return fixture, nil
}

func TestProgressInputBytesFixtureRejectsMisspelledKey(t *testing.T) {
	_, err := decodeProgressInputBytes(invalidProgressInputBytesYAML)
	if err == nil || !strings.Contains(err.Error(), "wantBytess") {
		t.Fatalf("misspelled key: got %v, want unknown-field error", err)
	}
}

func TestExtractInputBytes(t *testing.T) {
	fixture, err := decodeProgressInputBytes(progressInputBytesYAML)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range fixture.Cases {
		t.Run(row.Name, func(t *testing.T) {
			filesystem := &OSFileSystem{}
			p := &Pipeline{fs: filesystem}
			var entry DiffEntry
			var want int64
			switch row.Kind {
			case progressInputStored:
				pair := loadPairSnapshotFixture(t)
				output := t.TempDir()
				sid := SessionID(pair.SessionID)
				metadataPath := filepath.Join(output, pair.HostSlug, pair.SessionID, pair.SessionID+defaults.MetadataSuffix)
				seedSnapshotPair(t, filesystem, output, metadataPath, sid, buildSnapshotMetadata(t, pair, pair.FirstTranscript), pair.FirstTranscript)
				p.config.OutputDir = ResolvedPath(output)
				p.locationCache = map[SessionID]SessionLocation{sid: {HostSlug: pair.HostSlug}}
				entry = DiffEntry{Session: DiscoveredSession{SessionID: sid, Harness: HarnessClaudeCode, SourcePath: ResolvedPath("/native/" + pair.SessionID + ".jsonl")}, Decision: RefreshDecision{Lane: RefreshLaneStoredMetadata}}
				want = int64(len(pair.FirstTranscript))
			case progressInputLegacy, progressInputCurrent:
				materialized := testfixture.MaterializeByName(t, row.Fixture)
				p.adapters = map[Harness]AdapterFactory{HarnessOpenCode: func(fs FileSystem, git GitResolver, s salt.Salt) SourceAdapter {
					return NewOpenCodeAdapter(fs, git, s)
				}}
				origin := TranscriptOriginOpenCodeLegacySQLite
				if row.Kind == progressInputCurrent {
					origin = TranscriptOriginOpenCodeCurrentSQLite
				}
				entry.Session = DiscoveredSession{SessionID: SessionID(row.Session), Harness: HarnessOpenCode, SourcePath: ResolvedPath(materialized.Path), TranscriptOrigin: origin}
				want = row.WantBytes
			case progressInputTree:
				entry.Session = DiscoveredSession{Harness: HarnessOpenCode, SourcePath: "/synthetic/session.json", OriginalRoot: "/synthetic"}
			default:
				t.Fatalf("unknown input kind %q", row.Kind)
			}
			if got := p.extractInputBytes(t.Context(), entry); got != want {
				t.Fatalf("input bytes = %d, want %d", got, want)
			}
		})
	}
}
