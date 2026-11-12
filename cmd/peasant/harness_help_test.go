package main

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"gopkg.in/yaml.v3"
)

//go:embed testdata/harness_help.yaml
var harnessHelpYAML []byte

//go:embed testdata/harness_help.manifest.yaml
var harnessHelpManifestYAML []byte

// harnessHelpSurface names one piece of command help that lists harnesses.
type harnessHelpSurface string

const harnessHelpPushSourceHarness harnessHelpSurface = "push-source-harness"

// harnessHelpExpectation says whether a token appears in a surface's list.
type harnessHelpExpectation string

const (
	harnessHelpListed  harnessHelpExpectation = "listed"
	harnessHelpOmitted harnessHelpExpectation = "omitted"
)

type harnessHelpCase struct {
	Name     string                 `yaml:"name"`
	Surface  harnessHelpSurface     `yaml:"surface"`
	Token    string                 `yaml:"token"`
	Want     harnessHelpExpectation `yaml:"want"`
	Mutation string                 `yaml:"mutation"`
}

type harnessHelpFixture struct {
	Cases []harnessHelpCase `yaml:"cases"`
}

func decodeHarnessHelpYAML(t *testing.T, label string, data []byte, into any) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(into); err != nil {
		t.Fatalf("decode %s: %v", label, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("%s must contain exactly one YAML document: %v", label, err)
	}
}

func loadHarnessHelpFixture(t *testing.T) harnessHelpFixture {
	t.Helper()
	var fixture harnessHelpFixture
	decodeHarnessHelpYAML(t, "harness help fixture", harnessHelpYAML, &fixture)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decodeHarnessHelpYAML(t, "harness help manifest", harnessHelpManifestYAML, &manifest)
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if present[fixtureCase.Name] {
			t.Fatalf("harness help case %q is declared twice", fixtureCase.Name)
		}
		if fixtureCase.Token == "" || fixtureCase.Mutation == "" {
			t.Fatalf("harness help case %q needs a token and a mutation", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required harness help case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// harnessHelpTokens returns the harness tokens a help surface lists, parsed
// from the help the production command carries.
func harnessHelpTokens(t *testing.T, surface harnessHelpSurface) []string {
	t.Helper()
	switch surface {
	case harnessHelpPushSourceHarness:
		flag := BuildPushCommand().Flags().Lookup("source-harness")
		if flag == nil {
			t.Fatal("--source-harness flag is not registered on the push command")
		}
		_, rest, okOpen := strings.Cut(flag.Usage, "(")
		list, _, okClose := strings.Cut(rest, ")")
		if !okOpen || !okClose {
			t.Fatalf("--source-harness usage %q has no parenthesised harness list", flag.Usage)
		}
		return strings.Split(list, ", ")
	default:
		t.Fatalf("unknown harness help surface %q; add it to harnessHelpTokens", surface)
		return nil
	}
}

func TestHarnessHelpLists(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadHarnessHelpFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			tokens := harnessHelpTokens(t, fixtureCase.Surface)
			listed := slices.Contains(tokens, fixtureCase.Token)
			switch fixtureCase.Want {
			case harnessHelpListed:
				if !listed {
					t.Errorf("%s lists %v, which omits %q (red under: %s)", fixtureCase.Surface, tokens, fixtureCase.Token, fixtureCase.Mutation)
				}
			case harnessHelpOmitted:
				if listed {
					t.Errorf("%s lists %v, which includes %q (red under: %s)", fixtureCase.Surface, tokens, fixtureCase.Token, fixtureCase.Mutation)
				}
			default:
				t.Fatalf("case %q: unknown expectation %q; use %q or %q", fixtureCase.Name, fixtureCase.Want, harnessHelpListed, harnessHelpOmitted)
			}
		})
	}
}

// TestPushSourceHarnessHelpEqualsAdapterRegistry pins the --source-harness help
// on the flag BuildPushCommand() actually wires to the adapter registry, the
// owner of the harvestable set: every harness with an adapter is listed, and
// nothing else is.
func TestPushSourceHarnessHelpEqualsAdapterRegistry(t *testing.T) {
	t.Parallel()
	tokens := harnessHelpTokens(t, harnessHelpPushSourceHarness)
	want := make([]string, 0, len(ingest.DefaultAdapterRegistry))
	for harness := range ingest.DefaultAdapterRegistry {
		want = append(want, harness.String())
	}
	got := slices.Clone(tokens)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("--source-harness help lists %v; want exactly the adapter registry's harnesses %v", tokens, want)
	}
}
