package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/testutil"
	"gopkg.in/yaml.v3"
)

// The registry is the admission record for the partition, so its correctness is
// asserted against the repository tree and against named negative cases rather
// than an inline table: the case set is a fixture, and the required-name
// manifest keeps a deleted case from silently shrinking the suite.

//go:embed testdata/registry_cases.yaml
var registryCasesYAML []byte

//go:embed testdata/registry_membership.yaml
var registryMembershipYAML []byte

type registryCase struct {
	Name      string  `yaml:"name"`
	Version   *int    `yaml:"version"`
	Partition []Entry `yaml:"partition"`
	Protected []Entry `yaml:"protected"`
	WantError string  `yaml:"want_error"`
}

type registryCaseFile struct {
	RequiredNames []string       `yaml:"required_names"`
	Cases         []registryCase `yaml:"cases"`
}

func loadRegistryCases(t *testing.T) registryCaseFile {
	t.Helper()
	var file registryCaseFile
	decoder := yaml.NewDecoder(bytes.NewReader(registryCasesYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode registry cases fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("registry cases fixture must be a single document, got %v", err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("registry cases fixture has an empty required_names manifest")
	}
	byName := map[string]bool{}
	for _, c := range file.Cases {
		if c.Name == "" || byName[c.Name] {
			t.Fatalf("registry cases fixture has a duplicate or empty case name %q", c.Name)
		}
		byName[c.Name] = true
	}
	for _, name := range file.RequiredNames {
		if !byName[name] {
			t.Fatalf("required registry case %q is missing from the fixture", name)
		}
	}
	if len(byName) != len(file.RequiredNames) {
		t.Fatalf("registry cases fixture has %d cases but %d required names; a case was added or removed without updating the manifest", len(byName), len(file.RequiredNames))
	}
	return file
}

// registryMembershipFile is the required-name manifest for the committed
// registry: each entry is "<package>|<test>", the identity key the validator
// uses. The manifest names every committed unit so a deletion fails loudly
// instead of silently shrinking the no-race partition.
type registryMembershipFile struct {
	Partition []string `yaml:"partition"`
	Protected []string `yaml:"protected"`
}

func loadRegistryMembership(t *testing.T) registryMembershipFile {
	t.Helper()
	var file registryMembershipFile
	if err := testutil.DecodeFixtureYAML(registryMembershipYAML, &file); err != nil {
		t.Fatalf("decode registry membership fixture: %v", err)
	}
	if len(file.Partition) == 0 || len(file.Protected) == 0 {
		t.Fatal("registry membership fixture must list at least one partition and one protected entry")
	}
	seen := map[string]string{}
	for _, list := range []struct {
		name    string
		entries []string
	}{{"partition", file.Partition}, {"protected", file.Protected}} {
		for _, name := range list.entries {
			parts := strings.SplitN(name, "|", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				t.Fatalf("registry membership fixture entry %q is not <package>|<test>", name)
			}
			if prev, dup := seen[name]; dup {
				t.Fatalf("registry membership fixture lists %q in both %s and %s", name, prev, list.name)
			}
			seen[name] = list.name
		}
	}
	return file
}

func testRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod found above %s", dir)
		}
		dir = parent
	}
}

func TestRegistry_RealFixtureIsValid(t *testing.T) {
	root := testRepoRoot(t)
	reg, err := LoadRegistry(filepath.Join(root, "no-race-partition.yaml"))
	if err != nil {
		t.Fatalf("load the committed registry: %v", err)
	}
	if err := ValidateRegistry(root, reg); err != nil {
		t.Fatalf("the committed registry must be valid:\n%v", err)
	}
	if len(reg.Partition) == 0 {
		t.Fatal("the committed registry has no partition entries")
	}
}

// TestRegistry_CounterExampleIsProtected is the acceptance guard: the
// race-instrumented subprocess test must be pinned in protected, and moving it
// to partition must fail validation.
func TestRegistry_CounterExampleIsProtected(t *testing.T) {
	root := testRepoRoot(t)
	reg, err := LoadRegistry(filepath.Join(root, "no-race-partition.yaml"))
	if err != nil {
		t.Fatalf("load the committed registry: %v", err)
	}

	var counter Entry
	foundProtected := false
	for _, e := range reg.Protected {
		if e.Test == "TestOpenCodeNativeCLI" {
			counter = e
			foundProtected = true
		}
	}
	if !foundProtected {
		t.Fatal("TestOpenCodeNativeCLI must be in protected; it is the pinned race-instrumented counter-example")
	}
	for _, e := range reg.Partition {
		if e.Test == "TestOpenCodeNativeCLI" {
			t.Fatal("TestOpenCodeNativeCLI must not be in partition")
		}
	}

	// The mutation: the same entry moved into partition must fail.
	mutated := Registry{Version: 1, Partition: []Entry{counter}}
	if err := ValidateRegistry(root, mutated); err == nil {
		t.Fatal("moving the race-instrumented counter-example into partition must fail validation")
	} else if !strings.Contains(err.Error(), "must be built without -race") {
		t.Fatalf("mutation failed for the wrong reason: %v", err)
	}
}

// TestRegistry_CommittedMembershipMatchesManifest pins the committed
// registry's membership in both directions against the required-name manifest
// in testdata/registry_membership.yaml. A deleted partition entry would
// otherwise silently return that test to the race pass, and the gate would
// still pass: this test is the deletion guard.
func TestRegistry_CommittedMembershipMatchesManifest(t *testing.T) {
	root := testRepoRoot(t)
	reg, err := LoadRegistry(filepath.Join(root, "no-race-partition.yaml"))
	if err != nil {
		t.Fatalf("load the committed registry: %v", err)
	}
	manifest := loadRegistryMembership(t)

	require := func(list string, entries []Entry, required []string) {
		key := func(e Entry) string { return e.Package + "|" + e.Test }
		got := make(map[string]bool, len(entries))
		for _, e := range entries {
			got[key(e)] = true
		}
		want := make(map[string]bool, len(required))
		for _, name := range required {
			want[name] = true
		}
		for _, name := range required {
			if !got[name] {
				t.Errorf(
					"committed registry %s is missing required entry %q.\n"+
						"why: the required-name manifest is the deletion guard; losing this entry "+
						"silently returns the test to the race pass.\n"+
						"where: no-race-partition.yaml vs testdata/registry_membership.yaml.\n"+
						"fix: restore the registry entry, or, if the removal is intended, delete the "+
						"name from the manifest in the same change.",
					list, name)
			}
		}
		for _, e := range entries {
			if !want[key(e)] {
				t.Errorf(
					"committed registry %s carries entry %q that the manifest does not list.\n"+
						"why: deletion protection is only as strong as the manifest, so every "+
						"registered unit must be named there.\n"+
						"where: no-race-partition.yaml vs testdata/registry_membership.yaml.\n"+
						"fix: add %q to the %s list in testdata/registry_membership.yaml.",
					list, key(e), key(e), list)
			}
		}
	}
	require("partition", reg.Partition, manifest.Partition)
	require("protected", reg.Protected, manifest.Protected)
}

func TestRegistry_InvalidCasesFail(t *testing.T) {
	root := testRepoRoot(t)
	file := loadRegistryCases(t)
	for _, c := range file.Cases {
		t.Run(c.Name, func(t *testing.T) {
			version := 1
			if c.Version != nil {
				version = *c.Version
			}
			reg := Registry{Version: version, Partition: c.Partition, Protected: c.Protected}
			err := ValidateRegistry(root, reg)
			if err == nil {
				t.Fatalf("case %q was accepted; want error containing %q", c.Name, c.WantError)
			}
			if !strings.Contains(err.Error(), c.WantError) {
				t.Fatalf("case %q failed for the wrong reason:\n  got: %v\n want: %q", c.Name, err, c.WantError)
			}
		})
	}
}

func TestRegistry_ControlIsValid(t *testing.T) {
	root := testRepoRoot(t)
	reg, err := LoadRegistry(filepath.Join(root, "no-race-partition.yaml"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// A single-entry registry built from a real partition entry must validate,
	// proving the negative cases fail on their defect, not on the shape.
	if err := ValidateRegistry(root, Registry{Version: 1, Partition: reg.Partition[:1]}); err != nil {
		t.Fatalf("a valid single-entry registry was rejected: %v", err)
	}
}
