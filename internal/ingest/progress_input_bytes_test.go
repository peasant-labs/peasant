package ingest

import (
	_ "embed"
	"path/filepath"
	"testing"

	"github.com/peasant-labs/peasant/internal/defaults"
	"github.com/peasant-labs/peasant/internal/ingest/testfixture"
	"github.com/peasant-labs/peasant/internal/salt"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/progress_input_bytes.yaml
var progressInputBytesYAML []byte

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
		Name    string            `yaml:"name"`
		Kind    progressInputKind `yaml:"kind"`
		Fixture string            `yaml:"fixture"`
		Session string            `yaml:"session"`
		Known   bool              `yaml:"known"`
	} `yaml:"cases"`
}

func TestExtractInputBytes(t *testing.T) {
	var fixture progressInputBytesFixture
	if err := yaml.Unmarshal(progressInputBytesYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, row := range fixture.Cases {
		if row.Name == "" || seen[row.Name] {
			t.Fatalf("duplicate or empty case %q", row.Name)
		}
		seen[row.Name] = true
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
				sourcePath, err := NewOpenCodeSQLiteSourcePath(materialized.Path)
				if err != nil {
					t.Fatal(err)
				}
				source, err := OpenOpenCodeSQLiteSource(t.Context(), sourcePath, DefaultOpenCodeSQLiteSourceOptions())
				if err != nil {
					t.Fatal(err)
				}
				var size OpenCodePayloadSize
				if row.Kind == progressInputLegacy {
					id, err := NewOpenCodeLegacySessionID(row.Session)
					if err != nil {
						t.Fatal(err)
					}
					size, err = source.LegacySessionPayloadSize(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
				} else {
					id, err := NewOpenCodeCurrentSessionID(row.Session)
					if err != nil {
						t.Fatal(err)
					}
					size, err = source.CurrentSessionPayloadSize(t.Context(), id)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := source.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				want = size.Bytes
				if row.Known && want <= 0 {
					t.Fatal("fixture has no payload")
				}
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
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}
