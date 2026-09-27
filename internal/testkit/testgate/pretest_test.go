package testgate

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// The pre-test source anchors are validated against the repository Makefile so
// the embedded fixture cannot measure a step the Makefile no longer runs. The
// mutation cases live in testdata/pretest_ref_mutations.yaml, with a
// required-name manifest, so an absent or reordered fragment cannot pass.

//go:embed testdata/pretest_ref_mutations.yaml
var pretestRefMutationsYAML []byte

type pretestRefMutation struct {
	Name  string `yaml:"name"`
	Step  string `yaml:"step"`
	Op    string `yaml:"op"`
	Value string `yaml:"value"`
	With  string `yaml:"with"`
}

type pretestRefMutationFile struct {
	RequiredNames []string             `yaml:"required_names"`
	Mutations     []pretestRefMutation `yaml:"mutations"`
}

func loadPretestRefMutations(t *testing.T) pretestRefMutationFile {
	t.Helper()
	var file pretestRefMutationFile
	decoder := yaml.NewDecoder(bytes.NewReader(pretestRefMutationsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&file); err != nil {
		t.Fatalf("decode pre-test source mutation fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("pre-test source mutation fixture must be a single document, got %v", err)
	}
	if len(file.RequiredNames) == 0 {
		t.Fatal("pre-test source mutation fixture declares no required_names manifest")
	}
	byName := map[string]bool{}
	for _, m := range file.Mutations {
		if m.Name == "" || byName[m.Name] {
			t.Fatalf("pre-test source mutation fixture has an empty or duplicate case name %q", m.Name)
		}
		byName[m.Name] = true
	}
	for _, want := range file.RequiredNames {
		if !byName[want] {
			t.Fatalf("required pre-test source mutation %q is missing from the fixture", want)
		}
	}
	return file
}

func repoMakefile(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(testRepoRoot(t), "Makefile"))
	if err != nil {
		t.Fatalf("read repository Makefile: %v", err)
	}
	return data
}

// TestPreTestSources_MatchMakefile pins the embedded fixture to the repository
// Makefile: every step's fragment must appear in its declared target's recipe,
// in the same order the `check` target executes them.
func TestPreTestSources_MatchMakefile(t *testing.T) {
	file, err := LoadPreTestFixture(preTestFixtureYAML)
	if err != nil {
		t.Fatalf("LoadPreTestFixture: %v", err)
	}
	if err := validatePreTestSources(file, repoMakefile(t)); err != nil {
		t.Fatalf("the embedded pre-test fixture does not match the Makefile: %v", err)
	}
}

// TestPreTestSources_MutationsAreRejected proves the source check is not
// vacuous: a fragment absent from its target, a target that does not exist, a
// fragment in the wrong target, and a reordered step must each fail.
func TestPreTestSources_MutationsAreRejected(t *testing.T) {
	base, err := LoadPreTestFixture(preTestFixtureYAML)
	if err != nil {
		t.Fatalf("LoadPreTestFixture: %v", err)
	}
	makefile := repoMakefile(t)
	file := loadPretestRefMutations(t)
	for _, m := range file.Mutations {
		t.Run(m.Name, func(t *testing.T) {
			mutated, err := applyPretestRefMutation(base, m)
			if err != nil {
				t.Fatalf("apply %s: %v", m.Op, err)
			}
			if err := validatePreTestSources(mutated, makefile); err == nil {
				t.Fatalf("the %s mutation was NOT detected; the source check is vacuous on that axis", m.Op)
			}
		})
	}
}

func applyPretestRefMutation(file PreTestFixture, m pretestRefMutation) (PreTestFixture, error) {
	out := file
	out.Steps = append([]PreTestCommand(nil), file.Steps...)
	idx := -1
	for i, cmd := range out.Steps {
		if string(cmd.Step) == m.Step {
			idx = i
			break
		}
	}
	if idx < 0 {
		return PreTestFixture{}, fmt.Errorf("mutation %q names step %q not in the fixture", m.Name, m.Step)
	}
	switch m.Op {
	case "set-fragment":
		out.Steps[idx].Fragment = m.Value
	case "set-target":
		out.Steps[idx].Target = m.Value
	case "swap":
		j := -1
		for i, cmd := range out.Steps {
			if string(cmd.Step) == m.With {
				j = i
				break
			}
		}
		if j < 0 {
			return PreTestFixture{}, fmt.Errorf("mutation %q names step %q not in the fixture", m.Name, m.With)
		}
		out.Steps[idx], out.Steps[j] = out.Steps[j], out.Steps[idx]
	default:
		return PreTestFixture{}, fmt.Errorf("mutation %q has unknown op %q", m.Name, m.Op)
	}
	return out, nil
}
