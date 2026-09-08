package store_test

import (
	_ "embed"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/peasant/internal/store/storetest"
	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/publication-commit-merge.yaml
var publicationCommitMergeYAML []byte

func TestPublicationCommitMergePreservesDurableBindings(t *testing.T) {
	var cases []struct {
		Name     string   `yaml:"name"`
		Incoming []string `yaml:"incoming"`
		Expected []string `yaml:"expected"`
		Error    bool     `yaml:"error"`
	}
	decoder := yaml.NewDecoder(strings.NewReader(string(publicationCommitMergeYAML)))
	decoder.KnownFields(true)
	if err := decoder.Decode(&cases); err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	for _, c := range cases {
		if seen[c.Name] {
			t.Fatalf("duplicate fixture %s", c.Name)
		}
		seen[c.Name] = true
		t.Run(c.Name, func(t *testing.T) {
			s, err := store.Open(storetest.CopyGoldenDB(t))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			e := publicationEntry(t, testutil.TestSessionUUID)
			id := e.Metadata.SessionID
			revision := capturePublication(t, s, e)
			if result := indexPublication(t, s, e, revision, batchTestEntries(id, "captured", 1)); result.Err != nil {
				t.Fatal(result.Err)
			}
			if err := s.UpsertSessionCommits(t.Context(), id, []ingest.CommitInfo{{Hash: testutil.TestHeadCommitHash}}); err != nil {
				t.Fatal(err)
			}
			before, err := s.LoadPublicationInput(t.Context(), id)
			if err != nil || len(before.Associations) != 1 {
				t.Fatalf("seed binding = %+v, %v", before, err)
			}
			var incoming []ingest.CommitInfo
			for _, hash := range c.Incoming {
				incoming = append(incoming, ingest.CommitInfo{Hash: hash})
			}
			err = s.MergeSessionCommits(t.Context(), id, incoming)
			if (err != nil) != c.Error {
				t.Fatalf("merge error = %v expected error %v", err, c.Error)
			}
			after, err := s.LoadPublicationInput(t.Context(), id)
			if err != nil || after.Readiness != ingest.PublicationReady || after.CaptureRevision != revision {
				t.Fatalf("merge invalidated capture: %+v, %v", after, err)
			}
			var hashes []string
			for _, association := range after.Associations {
				hashes = append(hashes, association.ObservedCommitHash)
				if association.ObservedCommitHash == testutil.TestHeadCommitHash && association.ID != before.Associations[0].ID {
					t.Fatal("replayed binding changed its durable identity")
				}
			}
			sort.Strings(hashes)
			if !reflect.DeepEqual(hashes, c.Expected) {
				t.Fatalf("current bindings = %v expected %v", hashes, c.Expected)
			}
		})
	}
	for _, name := range strings.Fields("absent-current-git-preserves-historical-binding new-observation-retains-prior-binding replay-retains-association-identity invalid-observation-rolls-back-earlier-allocation") {
		if !seen[name] {
			t.Fatalf("missing required fixture %s", name)
		}
	}
}
