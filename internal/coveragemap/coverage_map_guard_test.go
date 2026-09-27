// Package coveragemap_test is an external consumer of the coverage-map
// contract. This file is the committed-document guard: it closes the map at
// docs/testing/coverage-map.yaml against the frozen branch-point inventory at
// docs/testing/pre-epoch-inventory.yaml, and proves the guard fails for the
// intended reason through fixture-driven mutations.
//
// The inventory is the set of top-level tests observed by a single
// `go test -list` run at the commit recorded in its own frozen_from field. The
// guard validates the map against that frozen set, never against a live
// listing, so a test added to the default branch after the freeze is simply
// outside the set: it can neither be demanded by the guard nor appear as an
// unmapped name. That keeps the two cases distinct — a symbol the frozen
// inventory carries with no map destination, versus a symbol that landed after
// the freeze and so is not part of the accounting at all.
package coveragemap_test

import (
	_ "embed"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/coveragemap"
	"github.com/peasant-labs/peasant/internal/testutil"
)

const (
	coverageMapRef    = "docs/testing/coverage-map.yaml"
	inventoryMapRef   = "docs/testing/pre-epoch-inventory.yaml"
	mutationLabel     = "coverage-map guard mutation"
	inventoryPathHint = "the coverage map must name the committed inventory"
)

//go:embed testdata/coverage_map_guard_mutations.yaml
var guardMutationYAML []byte

type guardMutation struct {
	Name        string `yaml:"name"`
	Op          string `yaml:"op"`
	Index       int    `yaml:"index"`
	EntryName   string `yaml:"entry_name"`
	Destination string `yaml:"destination"`
	WantError   string `yaml:"want_error"`
	ForbidError string `yaml:"forbid_error"`
}

type guardMutationFile struct {
	RequiredNames []string        `yaml:"required_names"`
	Mutations     []guardMutation `yaml:"mutations"`
}

// repoRoot walks up from the test's working directory to the directory holding
// go.mod, so the guard can address the committed documents by repository path.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("locate repository root: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("locate repository root: no go.mod above %s", dir)
		}
		dir = parent
	}
}

func loadCommitted(t *testing.T) (string, coveragemap.Inventory, coveragemap.CoverageMap) {
	t.Helper()
	root := repoRoot(t)
	inventory, err := coveragemap.LoadInventory(filepath.Join(root, filepath.FromSlash(inventoryMapRef)))
	if err != nil {
		t.Fatalf("load committed inventory: %v", err)
	}
	if err := coveragemap.ValidateInventory(inventory); err != nil {
		t.Fatalf("validate committed inventory: %v", err)
	}
	coverageMap, err := coveragemap.LoadCoverageMap(filepath.Join(root, filepath.FromSlash(coverageMapRef)))
	if err != nil {
		t.Fatalf("load committed coverage map: %v", err)
	}
	if coverageMap.Inventory != inventoryMapRef {
		t.Fatalf("%s: inventory ref = %q, want %q", inventoryPathHint, coverageMap.Inventory, inventoryMapRef)
	}
	return root, inventory, coverageMap
}

// TestCoverageMapClosesFrozenInventory is the guard: every frozen inventory
// name has exactly one destination, no entry names a symbol outside the
// inventory, and every destination is in the closed enum.
func TestCoverageMapClosesFrozenInventory(t *testing.T) {
	t.Parallel()
	root, inventory, coverageMap := loadCommitted(t)
	if err := coveragemap.ValidateCoverageMap(root, inventory, coverageMap); err != nil {
		t.Fatalf("coverage map does not close its frozen inventory: %v", err)
	}
	if len(inventory.Names) == 0 {
		t.Fatal("the frozen inventory is empty")
	}
	if len(coverageMap.Entries) != len(inventory.Names) {
		t.Fatalf("coverage map entries = %d, inventory names = %d", len(coverageMap.Entries), len(inventory.Names))
	}
}

// TestCoverageMapGuardMutations proves the guard fails for the intended reason
// on each deliberate defect, and that the two diagnostic classes stay distinct:
// a name the frozen inventory does not carry is reported as "not in the
// inventory", never as an inventory name that "has no destination".
func TestCoverageMapGuardMutations(t *testing.T) {
	t.Parallel()
	root, inventory, coverageMap := loadCommitted(t)
	fixture := decodeGuardMutations(t)
	names := make([]string, 0, len(fixture.Mutations))
	for _, mutation := range fixture.Mutations {
		names = append(names, mutation.Name)
	}
	if err := validateRequiredMutationNames(fixture.RequiredNames, names); err != nil {
		t.Fatal(err)
	}
	for _, mutation := range fixture.Mutations {
		mutation := mutation
		t.Run(mutation.Name, func(t *testing.T) {
			t.Parallel()
			mutated, applyErr := applyGuardMutation(coverageMap, mutation)
			if applyErr != nil {
				if !strings.Contains(applyErr.Error(), mutation.WantError) {
					t.Fatalf("mutation %q failed at apply with %v, want %q", mutation.Name, applyErr, mutation.WantError)
				}
				return
			}
			err := coveragemap.ValidateCoverageMap(root, inventory, mutated)
			if err == nil {
				t.Fatalf("mutation %q passed the guard, want failure containing %q", mutation.Name, mutation.WantError)
			}
			if !strings.Contains(err.Error(), mutation.WantError) {
				t.Fatalf("mutation %q error = %v, want %q", mutation.Name, err, mutation.WantError)
			}
			if mutation.ForbidError != "" && strings.Contains(err.Error(), mutation.ForbidError) {
				t.Fatalf("mutation %q error = %v, must not contain %q", mutation.Name, err, mutation.ForbidError)
			}
		})
	}
}

// TestCoverageMapCarriesNoTaskTaxonomy holds the shipped-artifact rule that no
// task identifier rides in the documents: a task id may appear only as the
// machine-readable followup destination value.
func TestCoverageMapCarriesNoTaskTaxonomy(t *testing.T) {
	t.Parallel()
	root := repoRoot(t)
	taskID := regexp.MustCompile(`\bplabs-[a-z0-9]+\b`)
	for _, ref := range []string{coverageMapRef, inventoryMapRef} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(ref)))
		if err != nil {
			t.Fatalf("read %s: %v", ref, err)
		}
		for lineIndex, line := range strings.Split(string(data), "\n") {
			for _, found := range taskID.FindAllString(line, -1) {
				if strings.Contains(line, "followup:"+found) {
					continue
				}
				t.Errorf("%s:%d carries task id %q outside a followup destination: %s", ref, lineIndex+1, found, line)
			}
		}
	}
}

func decodeGuardMutations(t *testing.T) guardMutationFile {
	t.Helper()
	var fixture guardMutationFile
	if err := testutil.DecodeFixtureYAML(guardMutationYAML, &fixture); err != nil {
		t.Fatalf("decode %s fixture: %v", mutationLabel, err)
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("mutation fixture declares no required_names")
	}
	seen := map[string]bool{}
	for _, name := range fixture.RequiredNames {
		if name == "" || seen[name] {
			t.Fatalf("mutation fixture required_names has an empty or duplicate name %q", name)
		}
		seen[name] = true
	}
	for _, mutation := range fixture.Mutations {
		if mutation.Name == "" || mutation.WantError == "" {
			t.Fatalf("invalid mutation row: %+v", mutation)
		}
	}
	return fixture
}

func validateRequiredMutationNames(required, actual []string) error {
	requiredSet := map[string]bool{}
	for _, name := range required {
		requiredSet[name] = true
	}
	actualSet := map[string]bool{}
	for _, name := range actual {
		if actualSet[name] {
			return fmt.Errorf("%s repeats case %q", mutationLabel, name)
		}
		actualSet[name] = true
	}
	for _, name := range required {
		if !actualSet[name] {
			return fmt.Errorf("%s is missing required case %q", mutationLabel, name)
		}
	}
	for _, name := range actual {
		if !requiredSet[name] {
			return fmt.Errorf("%s carries undeclared case %q", mutationLabel, name)
		}
	}
	return nil
}

func applyGuardMutation(m coveragemap.CoverageMap, mutation guardMutation) (coveragemap.CoverageMap, error) {
	entries := make([]coveragemap.MapEntry, len(m.Entries))
	copy(entries, m.Entries)
	m.Entries = entries
	switch mutation.Op {
	case "append-entry":
		destination, err := coveragemap.ParseDestination(mutation.Destination)
		if err != nil {
			return m, err
		}
		m.Entries = append(m.Entries, coveragemap.MapEntry{Name: mutation.EntryName, Destination: destination})
		return m, nil
	case "duplicate-entry":
		if mutation.Index < 0 || mutation.Index >= len(m.Entries) {
			return m, fmt.Errorf("duplicate-entry index %d out of range", mutation.Index)
		}
		m.Entries = append(m.Entries, m.Entries[mutation.Index])
		return m, nil
	case "drop-entry":
		if mutation.Index < 0 || mutation.Index >= len(m.Entries) {
			return m, fmt.Errorf("drop-entry index %d out of range", mutation.Index)
		}
		m.Entries = append(m.Entries[:mutation.Index], m.Entries[mutation.Index+1:]...)
		return m, nil
	case "set-destination":
		if mutation.Index < 0 || mutation.Index >= len(m.Entries) {
			return m, fmt.Errorf("set-destination index %d out of range", mutation.Index)
		}
		destination, err := coveragemap.ParseDestination(mutation.Destination)
		if err != nil {
			return m, err
		}
		m.Entries[mutation.Index].Destination = destination
		return m, nil
	default:
		return m, fmt.Errorf("unknown mutation op %q", mutation.Op)
	}
}
