package ingest_test

import (
	_ "embed"
	"errors"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/written_counts.yaml
var writtenCountsData []byte

type writtenCountsSession struct {
	ID       string `yaml:"id"`
	Status   string `yaml:"status"`
	Failed   bool   `yaml:"failed"`
	Excluded bool   `yaml:"excluded"`
}

type writtenCountsCase struct {
	Name    string `yaml:"name"`
	Summary struct {
		New     int `yaml:"new"`
		Updated int `yaml:"updated"`
	} `yaml:"summary"`
	Existed     []string               `yaml:"existed"`
	Sessions    []writtenCountsSession `yaml:"sessions"`
	NoResult    bool                   `yaml:"noResult"`
	WantNew     int                    `yaml:"wantNew"`
	WantUpdated int                    `yaml:"wantUpdated"`
}

type writtenCountsDocument struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []writtenCountsCase `yaml:"cases"`
}

// TestPipelineResultWritten runs the written-counts fixture: a session that was
// still being written when the run read it counts as written, new or updated by
// whether the store held its row before.
func TestPipelineResultWritten(t *testing.T) {
	t.Parallel()
	var document writtenCountsDocument
	if err := testutil.DecodeFixtureYAML(writtenCountsData, &document); err != nil {
		t.Fatalf("decode written_counts.yaml: %v", err)
	}
	present := make(map[string]bool, len(document.Cases))
	for _, row := range document.Cases {
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("internal/ingest/testdata/written_counts.yaml", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	statuses := map[string]ingest.DiffStatus{
		"new": ingest.DiffNew, "updated": ingest.DiffUpdated, "unchanged": ingest.DiffUnchanged, "active": ingest.DiffActive,
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			var result *ingest.PipelineResult
			if !row.NoResult {
				result = &ingest.PipelineResult{}
				result.Summary.New, result.Summary.Updated = row.Summary.New, row.Summary.Updated
				for _, session := range row.Sessions {
					status, ok := statuses[session.Status]
					if !ok {
						t.Fatalf("fixture session %q has status %q, want new, updated, unchanged, or active", session.ID, session.Status)
					}
					id, err := ingest.NewSessionID(session.ID)
					if err != nil {
						t.Fatalf("fixture session id %q: %v", session.ID, err)
					}
					entry := ingest.SessionResult{SessionID: id, Status: status, ExcludedBySelection: session.Excluded}
					if session.Failed {
						entry.Error = errors.New("the fixture session failed")
					}
					result.Sessions = append(result.Sessions, entry)
				}
			}
			existed := make(map[string]bool, len(row.Existed))
			for _, id := range row.Existed {
				existed[id] = true
			}
			got := result.Written(func(id ingest.SessionID) bool { return existed[id.String()] })
			if got.New != row.WantNew || got.Updated != row.WantUpdated {
				t.Fatalf("Written = %+v, want %d new and %d updated", got, row.WantNew, row.WantUpdated)
			}
		})
	}
}
