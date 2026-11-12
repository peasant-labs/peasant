package defaults

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/harnesses_in_known_order.yaml
var harnessesInKnownOrderYAML []byte

//go:embed testdata/harnesses_in_known_order.manifest.yaml
var harnessesInKnownOrderManifestYAML []byte

// harnessListToken names a harness list the test derives from the contract
// instead of restating it in the fixture.
type harnessListToken string

const (
	harnessListKnown         harnessListToken = "known"
	harnessListKnownReversed harnessListToken = "known-reversed"
)

type harnessesInKnownOrderCase struct {
	Name         string           `yaml:"name"`
	Input        []string         `yaml:"input"`
	InputToken   harnessListToken `yaml:"inputToken"`
	Want         []string         `yaml:"want"`
	WantToken    harnessListToken `yaml:"wantToken"`
	Unrecognised []string         `yaml:"unrecognised"`
	Mutation     string           `yaml:"mutation"`
}

type harnessesInKnownOrderFixture struct {
	Cases []harnessesInKnownOrderCase `yaml:"cases"`
}

func decodeSingleYAMLDocument(t *testing.T, label string, data []byte, into any) {
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

func loadHarnessesInKnownOrderFixture(t *testing.T) harnessesInKnownOrderFixture {
	t.Helper()
	var fixture harnessesInKnownOrderFixture
	decodeSingleYAMLDocument(t, "harnesses in known order fixture", harnessesInKnownOrderYAML, &fixture)
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	decodeSingleYAMLDocument(t, "harnesses in known order manifest", harnessesInKnownOrderManifestYAML, &manifest)
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if present[fixtureCase.Name] {
			t.Fatalf("harnesses in known order case %q is declared twice", fixtureCase.Name)
		}
		if fixtureCase.Mutation == "" {
			t.Fatalf("harnesses in known order case %q names no mutation that turns it red", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required harnesses in known order case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// resolveHarnessList turns a fixture list into harness values: the token when
// one is set, otherwise the literal values. Every literal value must be known
// unless the case declares it unrecognised.
func resolveHarnessList(t *testing.T, token harnessListToken, literal []string, unrecognised []string) []Harness {
	t.Helper()
	switch token {
	case harnessListKnown:
		return Harnesses()
	case harnessListKnownReversed:
		reversed := Harnesses()
		slices.Reverse(reversed)
		return reversed
	case "":
	default:
		t.Fatalf("unknown harness list token %q; the test knows %q and %q", token, harnessListKnown, harnessListKnownReversed)
	}
	out := make([]Harness, 0, len(literal))
	for _, value := range literal {
		harness := Harness(value)
		if harness.IsKnown() == slices.Contains(unrecognised, value) {
			t.Fatalf("fixture value %q: known=%v, but the case lists it as unrecognised=%v; fix the value or the unrecognised list", value, harness.IsKnown(), slices.Contains(unrecognised, value))
		}
		out = append(out, harness)
	}
	return out
}

func TestInKnownOrder(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadHarnessesInKnownOrderFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			input := resolveHarnessList(t, fixtureCase.InputToken, fixtureCase.Input, fixtureCase.Unrecognised)
			want := resolveHarnessList(t, fixtureCase.WantToken, fixtureCase.Want, fixtureCase.Unrecognised)
			inputCopy := slices.Clone(input)
			got := InKnownOrder(input)
			if !slices.Equal(got, want) {
				t.Errorf("InKnownOrder(%v) = %v, want %v (red under: %s)", inputCopy, got, want, fixtureCase.Mutation)
			}
			if !slices.Equal(input, inputCopy) {
				t.Errorf("InKnownOrder modified its argument: before %v, after %v", inputCopy, input)
			}
		})
	}
}
