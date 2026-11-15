package ingest

import (
	"context"
	_ "embed"
	"errors"
	"io/fs"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/census_availability.yaml
var censusAvailabilityYAML []byte

type unavailableCleanupFS struct{ OSFileSystem }

func (*unavailableCleanupFS) ReadDir(string) ([]fs.DirEntry, error) { return nil, fs.ErrNotExist }
func (*unavailableCleanupFS) RemoveAll(string) error                { return errors.New("unexpected cleanup") }

var _ FileSystem = (*unavailableCleanupFS)(nil)

type availabilityIndexLogger struct{ fail bool }

func (l *availabilityIndexLogger) LogIndexEntry(context.Context, IndexLogEntry) error {
	if l.fail {
		return errors.New("synthetic index log failure")
	}
	return nil
}

var _ IndexLogger = (*availabilityIndexLogger)(nil)

func TestFinalCensusRequiresStoredOutcomesAndIndexLog(t *testing.T) {
	var fixture struct {
		RequiredNames []string `yaml:"requiredNames"`
		Cases         []struct {
			Name         string `yaml:"name"`
			Audit        bool   `yaml:"audit"`
			Index        bool   `yaml:"index"`
			AuditFails   bool   `yaml:"auditFails"`
			IndexFails   bool   `yaml:"indexFails"`
			SessionFails bool   `yaml:"sessionFails"`
			EmptyIndex   bool   `yaml:"emptyIndex"`
			WantStored   bool   `yaml:"wantStored"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(censusAvailabilityYAML, &fixture); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, row := range fixture.Cases {
		if seen[row.Name] {
			t.Fatalf("duplicate case %q", row.Name)
		}
		seen[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			p := &Pipeline{fs: &unavailableCleanupFS{}}
			if row.Audit {
				logger := &outcomeAuditLogger{}
				if row.AuditFails {
					logger.err = errors.New("synthetic audit failure")
				}
				p.logger = logger
			}
			if row.Index {
				p.indexLogger = &availabilityIndexLogger{fail: row.IndexFails}
			}
			bytes := int64(1024)
			results := []SessionResult{{SessionID: "synthetic", Status: DiffNew}}
			if row.SessionFails {
				results[0].Error = errors.New("synthetic capture failure")
			}
			logs := []IndexLogEntry{{SessionID: "synthetic", Bytes: &bytes}}
			if row.EmptyIndex {
				logs = nil
				bytes = 0
			}
			got, err := p.indexComputeAndFinalize(t.Context(), nil, nil, results, nil, time.Now(), logs, IndexOutcomeIndexed, "test", nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			if got.CensusStored != row.WantStored {
				t.Fatalf("stored = %t, want %t", got.CensusStored, row.WantStored)
			}
			if got.Census == nil || got.Census.Sessions() != 1 || got.Census.Rows[0].SourceBytes != bytes {
				t.Fatalf("in-memory run census = %+v", got.Census)
			}
		})
	}
	for _, name := range fixture.RequiredNames {
		if !seen[name] {
			t.Fatalf("missing case %q", name)
		}
	}
}
