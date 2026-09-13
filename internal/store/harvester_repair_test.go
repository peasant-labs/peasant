package store_test

import (
	"context"
	_ "embed"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/store"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harvester_repair.yaml
var harvesterRepairYAML []byte

type harvesterRepairSession struct {
	Name                              string           `yaml:"name"`
	ID                                ingest.SessionID `yaml:"id"`
	Harness                           ingest.Harness   `yaml:"harness"`
	AdapterVersion                    *int             `yaml:"adapter_version"`
	ArtifactHash                      string           `yaml:"artifact_hash"`      // "set" | "clear" | ""
	IndexedInputHash                  string           `yaml:"indexed_input_hash"` // "set" | "clear" | ""
	PublicationCaptureRevision        int              `yaml:"publication_capture_revision"`
	IndexedPublicationCaptureRevision int              `yaml:"indexed_publication_capture_revision"`
	MetadataRowAtRevision             *int             `yaml:"metadata_row_at_revision"`
}

type harvesterRepairCase struct {
	Name                      string                   `yaml:"name"`
	Selector                  string                   `yaml:"selector"` // "stale_adapter" | "repair"
	AdapterTarget             int                      `yaml:"adapter_target"`
	BaselineTargetSelectsNone bool                     `yaml:"baseline_target_selects_none"`
	HarnessTargets            []ingest.Harness         `yaml:"harness_targets"`
	Sessions                  []harvesterRepairSession `yaml:"sessions"`
	Expected                  []string                 `yaml:"expected"`
}

type harvesterRepairFixtures struct {
	Required []string              `yaml:"required_names"`
	Cases    []harvesterRepairCase `yaml:"cases"`
}

func loadHarvesterRepairFixtures(t *testing.T) harvesterRepairFixtures {
	t.Helper()
	var fixtures harvesterRepairFixtures
	if err := yaml.Unmarshal(harvesterRepairYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	names := make(map[string]bool)
	for _, c := range fixtures.Cases {
		if c.Name == "" || names[c.Name] {
			t.Fatalf("empty or duplicate repair fixture %q", c.Name)
		}
		names[c.Name] = true
	}
	for _, name := range fixtures.Required {
		if !names[name] {
			t.Fatalf("missing required repair fixture %q", name)
		}
	}
	return fixtures
}

const repairProjectHash = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// seedRepairSession inserts one session row and forces it into the exact column
// state a case needs: its adapter revision, the presence or absence of the pair
// hash and the index-input proof, its publication capture revisions, and an
// optional publication metadata row at a given capture revision.
func seedRepairSession(t *testing.T, s *store.Store, session harvesterRepairSession) {
	t.Helper()
	ctx := context.Background()
	entry := makeStoreEntry(t, string(session.ID), repairProjectHash, "fixture-project", session.Harness, 1700000000000, 0, 0)
	if err := s.InsertSessions(ctx, []ingest.StoreEntry{entry}); err != nil {
		t.Fatal(err)
	}
	if session.AdapterVersion != nil {
		inputSQL(t, s, session.ID, "UPDATE sessions SET adapter_version = ? WHERE session_id = ?", *session.AdapterVersion)
	}
	switch session.ArtifactHash {
	case "set":
		inputSQL(t, s, session.ID, "UPDATE sessions SET artifact_hash = ? WHERE session_id = ?", strings.Repeat("a", 64))
	case "clear":
		inputSQL(t, s, session.ID, "UPDATE sessions SET artifact_hash = NULL WHERE session_id = ?")
	}
	switch session.IndexedInputHash {
	case "set":
		inputSQL(t, s, session.ID, "UPDATE sessions SET indexed_input_hash = ? WHERE session_id = ?", strings.Repeat("b", 64))
	case "clear":
		inputSQL(t, s, session.ID, "UPDATE sessions SET indexed_input_hash = NULL WHERE session_id = ?")
	}
	if session.PublicationCaptureRevision > 0 || session.IndexedPublicationCaptureRevision > 0 {
		inputSQL(t, s, session.ID,
			"UPDATE sessions SET publication_capture_revision = ?, indexed_publication_capture_revision = ? WHERE session_id = ?",
			session.PublicationCaptureRevision, session.IndexedPublicationCaptureRevision)
	}
	if session.MetadataRowAtRevision != nil {
		inputSQL(t, s, session.ID,
			`INSERT INTO session_publication_metadata
(capture_revision,schema_version,metadata_json,metadata_hash,content_hash,captured_at,session_id)
VALUES (?,?,?,?,?,?,?)`,
			*session.MetadataRowAtRevision, int64(ingest.CurrentSchemaVersion), `{"seed":true}`,
			strings.Repeat("c", 64), strings.Repeat("d", 64), int64(1700000000000))
	}
}

// TestStore_HarvesterRepairInventories proves the two database-driven
// inventories that replace the retained-tree walks: ListStaleAdapterSessions,
// which selects a session behind this build's adapter target by its stored
// revision alone, and ListSessionsNeedingRepair, whose revision half needs a
// publication metadata row and whose whole predicate is harness-scoped. Neither
// reads a file, so the "zero tree reads" property is inherent.
func TestStore_HarvesterRepairInventories(t *testing.T) {
	t.Parallel()
	fixtures := loadHarvesterRepairFixtures(t)
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			t.Parallel()
			s := openTestStore(t)
			ctx := context.Background()
			byID := make(map[ingest.SessionID]string, len(c.Sessions))
			for _, session := range c.Sessions {
				seedRepairSession(t, s, session)
				byID[session.ID] = session.Name
			}
			names := func(ids []ingest.SessionID) []string {
				got := make([]string, 0, len(ids))
				for _, id := range ids {
					got = append(got, byID[id])
				}
				slices.Sort(got)
				return got
			}
			expected := append([]string(nil), c.Expected...)
			slices.Sort(expected)

			switch c.Selector {
			case "stale_adapter":
				ids, err := s.ListStaleAdapterSessions(ctx, adapterTargets(c, c.AdapterTarget), nil)
				if err != nil {
					t.Fatal(err)
				}
				if got := names(ids); !slices.Equal(got, expected) {
					t.Fatalf("adapter target %d selected %v, want %v", c.AdapterTarget, got, expected)
				}
				if c.BaselineTargetSelectsNone {
					baseline, err := s.ListStaleAdapterSessions(ctx, adapterTargets(c, 1), nil)
					if err != nil {
						t.Fatal(err)
					}
					if len(baseline) != 0 {
						t.Fatalf("adapter target 1 selected %v, want none (an unchanged harvest must re-extract nothing)", names(baseline))
					}
				}
			case "repair":
				ids, err := s.ListSessionsNeedingRepair(ctx, repairTargets(c.HarnessTargets))
				if err != nil {
					t.Fatal(err)
				}
				if got := names(ids); !slices.Equal(got, expected) {
					t.Fatalf("repair selected %v, want %v", got, expected)
				}
			default:
				t.Fatalf("unknown selector %q", c.Selector)
			}
		})
	}
}

// adapterTargets builds a target set covering every harness the case seeds, so
// the selection is scoped to exactly those harnesses at the requested adapter
// revision.
func adapterTargets(c harvesterRepairCase, adapter int) map[ingest.Harness]ingest.HarvesterVersions {
	targets := make(map[ingest.Harness]ingest.HarvesterVersions)
	for _, session := range c.Sessions {
		targets[session.Harness] = ingest.HarvesterVersions{AdapterVersion: adapter, IndexerVersion: 1, IndexVersion: 1}
	}
	return targets
}

func repairTargets(harnesses []ingest.Harness) map[ingest.Harness]ingest.HarvesterVersions {
	targets := make(map[ingest.Harness]ingest.HarvesterVersions)
	for _, harness := range harnesses {
		targets[harness] = ingest.HarvesterVersions{AdapterVersion: 1, IndexerVersion: 1, IndexVersion: 1}
	}
	return targets
}
