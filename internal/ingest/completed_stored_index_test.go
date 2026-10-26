package ingest

import (
	"context"
	_ "embed"
	"testing"
)

//go:embed testdata/completed_stored_index.yaml
var completedStoredIndexFixtureData []byte

type completedStoredIndexFixtures struct {
	Required []string `yaml:"required_names"`
	Cases    []struct {
		Name           string `yaml:"name"`
		Row            bool   `yaml:"row"`
		IndexerVersion int    `yaml:"indexer_version"`
		IndexedAt      *int64 `yaml:"indexed_at"`
		FormatVersion  *int   `yaml:"format_version"`
		Want           bool   `yaml:"want"`
	} `yaml:"cases"`
}

// completedIndexStateStore answers the snapshot read the predicate uses. The
// embedded store is unused: this test only asks whether a finished index serves.
type completedIndexStateStore struct {
	MetricsStore
	state *SessionIndexState
}

func (s completedIndexStateStore) ReadIndexState(context.Context, SessionID) (*SessionIndexState, error) {
	return s.state, nil
}

// TestCompletedStoredIndexMatchesTheStoreRule pins the one definition of a
// finished index: a positive producer revision and a recorded completion time.
// A producer revision with no completion time does not serve. A completion
// time with a zero producer revision does not serve. No row does not serve.
// The format stamp is not a substitute for either half.
func TestCompletedStoredIndexMatchesTheStoreRule(t *testing.T) {
	var fixtures completedStoredIndexFixtures
	decodeOneFixture(t, completedStoredIndexFixtureData, &fixtures, "completed-index fixture")
	names := make(map[string]bool)
	for _, fixture := range fixtures.Cases {
		if fixture.Name == "" || names[fixture.Name] {
			t.Fatalf("empty or duplicate completed-index fixture %q", fixture.Name)
		}
		names[fixture.Name] = true
	}
	requireNamedCases(t, "completed-index fixture", fixtures.Required, names)
	sid := SessionID("11111111-1111-1111-1111-111111111111")
	for _, fixture := range fixtures.Cases {
		t.Run(fixture.Name, func(t *testing.T) {
			var state *SessionIndexState
			if fixture.Row {
				state = &SessionIndexState{
					SessionID:      sid,
					Harness:        HarnessClaudeCode,
					IndexerVersion: fixture.IndexerVersion,
					IndexedAt:      fixture.IndexedAt,
					IndexVersion:   fixture.FormatVersion,
				}
			}
			if got := completedStoredIndex(state); got != fixture.Want {
				t.Fatalf("completedStoredIndex(%+v) = %v, want %v", state, got, fixture.Want)
			}
			pipeline := &Pipeline{metricsStore: completedIndexStateStore{state: state}}
			if got := pipeline.hasCompletedStoredIndex(t.Context(), sid); got != fixture.Want {
				t.Fatalf("hasCompletedStoredIndex(%+v) = %v, want %v", state, got, fixture.Want)
			}
		})
	}
}
