package discovery_test

import (
	_ "embed"
	"testing"
	"time"

	"github.com/peasant-labs/peasant/internal/discovery"
	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/testutil"
)

//go:embed testdata/known_store.yaml
var knownStoreData []byte

type knownStoreRow struct {
	SessionID    string `yaml:"sessionId"`
	IngestedMs   int64  `yaml:"ingestedMs"`
	Schema       string `yaml:"schema"`
	GitRemote    string `yaml:"gitRemote"`
	Branch       string `yaml:"branch"`
	CanonicalCwd string `yaml:"canonicalCwd"`
	Title        string `yaml:"title"`
}

type knownStoreCase struct {
	Name       string          `yaml:"name"`
	Rows       []knownStoreRow `yaml:"rows"`
	Discovered struct {
		SessionID string `yaml:"sessionId"`
		ModMs     int64  `yaml:"modMs"`
	} `yaml:"discovered"`
	Reusable bool `yaml:"reusable"`
	NilIndex bool `yaml:"nilIndex"`
	Want     struct {
		GitRemote        string `yaml:"gitRemote"`
		Branch           string `yaml:"branch"`
		WorkingDirectory string `yaml:"workingDirectory"`
		Title            string `yaml:"title"`
	} `yaml:"want"`
}

func TestStoreKnownSessions(t *testing.T) {
	t.Parallel()
	var document struct {
		RequiredNames []string         `yaml:"requiredNames"`
		Cases         []knownStoreCase `yaml:"cases"`
	}
	if err := testutil.DecodeFixtureYAML(knownStoreData, &document); err != nil {
		t.Fatalf("decode known store fixture: %v", err)
	}
	present := map[string]bool{}
	for _, row := range document.Cases {
		present[row.Name] = true
	}
	if err := testutil.RequireFixtureNames("known store", "case", document.RequiredNames, present); err != nil {
		t.Fatal(err)
	}
	for _, row := range document.Cases {
		t.Run(row.Name, func(t *testing.T) {
			t.Parallel()
			rows := make([]store.IngestedSessionRow, len(row.Rows))
			for i, recorded := range row.Rows {
				version := 1
				if recorded.Schema == "current" {
					version = int(ingest.CurrentSchemaVersion)
				}
				rows[i] = store.IngestedSessionRow{
					SessionID: recorded.SessionID, IngestedMs: recorded.IngestedMs, SchemaVersion: version,
					GitRemote: recorded.GitRemote, Branch: recorded.Branch, CanonicalCwd: recorded.CanonicalCwd, Title: recorded.Title,
				}
			}
			index := discovery.NewStoreKnownSessions(rows)
			if (index == nil) != row.NilIndex {
				t.Fatalf("index nil = %t, want %t", index == nil, row.NilIndex)
			}
			session := ingest.DiscoveredSession{
				SessionID: ingest.SessionID(row.Discovered.SessionID),
				ModTime:   time.UnixMilli(row.Discovered.ModMs),
			}
			got, reusable := index.Reusable(session, time.Hour)
			if reusable != row.Reusable {
				t.Fatalf("reusable = %t, want %t", reusable, row.Reusable)
			}
			if !reusable {
				return
			}
			if got.GitRemote != row.Want.GitRemote || got.Branch != row.Want.Branch || got.WorkingDirectory() != row.Want.WorkingDirectory || got.Title != row.Want.Title {
				t.Fatalf("recorded values = %+v, want %+v", got, row.Want)
			}
		})
	}
}
