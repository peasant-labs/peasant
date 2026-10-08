package store

import (
	"context"
	_ "embed"
	"errors"
	"math"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/search_health_entry.yaml
var searchHealthEntryYAML []byte

//go:embed testdata/search_health_entry.manifest.yaml
var searchHealthEntryManifestYAML []byte

type searchHealthEntryCase struct {
	Name              string `yaml:"name"`
	Entry             string `yaml:"entry"`
	Free              string `yaml:"free"`
	Refuses           bool   `yaml:"refuses"`
	NeedsRebuild      bool   `yaml:"needsRebuild"`
	CancelAtPreflight bool   `yaml:"cancelAtPreflight"`
	Rebuilt           bool   `yaml:"rebuilt"`
}

func TestSearchHealthEntry(t *testing.T) {
	var fixtures struct {
		Cases []searchHealthEntryCase `yaml:"cases"`
	}
	if err := yaml.Unmarshal(searchHealthEntryYAML, &fixtures); err != nil {
		t.Fatal(err)
	}
	manifest, err := decodeRecoveryRequiredNames(searchHealthEntryManifestYAML)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range fixtures.Cases {
		names = append(names, c.Name)
	}
	if err := validateRecoveryRequiredNames(manifest, names, "search health entry"); err != nil {
		t.Fatal(err)
	}
	for _, c := range fixtures.Cases {
		t.Run(c.Name, func(t *testing.T) {
			s, _ := openGenerationStore(t)
			recallExec(t, s, `UPDATE session_search_state SET needs_rebuild=1 WHERE id=1`)
			previous := migrateFreeBytes
			t.Cleanup(func() {
				migrateFreeBytes = previous
			})
			plan, err := s.PlanMigration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if plan.DiskRequiredBytes != int64(math.Ceil(float64(plan.StoreBytes)*0.60)) {
				t.Fatalf("reserve is not proportional: %+v", plan)
			}
			migrateFreeBytes = func(string) (uint64, bool) {
				switch c.Free {
				case "below":
					return uint64(plan.DiskRequiredBytes - 1), true
				case "at":
					return uint64(plan.DiskRequiredBytes), true
				case "unknown":
					return 0, false
				default:
					return math.MaxInt64, true
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch c.Entry {
			case "migrate":
				_, err = s.Migrate(ctx, MigrateOptions{Progress: func(p MigrateProgress) {
					if c.CancelAtPreflight && p.Phase == MigrationPhasePreflight {
						cancel()
					}
				}})
				if c.Refuses && err == nil {
					t.Fatal("low disk migration did not refuse")
				}
				if c.CancelAtPreflight && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancel = %v", err)
				}
				if queryMigrateInt(t, s, `SELECT count(*) FROM sqlite_master WHERE name='session_entries_fts'`) != 1 {
					t.Fatal("migration wrote consolidation before refusal/cancellation")
				}
			case "reclaim":
				_, err = s.ReclaimSupersededGenerations(ctx, 0)
			case "verify":
				var report ContentVerifyReport
				report, err = s.VerifyContent(ctx, false)
				if report.IndexRebuilt != c.Rebuilt {
					t.Fatalf("rebuilt=%v, want %v", report.IndexRebuilt, c.Rebuilt)
				}
			default:
				t.Fatalf("unhandled entry %s", c.Entry)
			}
			if err != nil && !c.Refuses && !c.CancelAtPreflight {
				t.Fatal(err)
			}
			state, stateErr := s.SearchState(t.Context())
			if stateErr != nil {
				t.Fatal(stateErr)
			}
			if state.NeedsRebuild != c.NeedsRebuild {
				t.Fatalf("needs_rebuild=%v, want %v", state.NeedsRebuild, c.NeedsRebuild)
			}
		})
	}
}
