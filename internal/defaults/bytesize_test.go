package defaults

import (
	"bytes"
	_ "embed"
	"errors"
	"io"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/human_byte_size.yaml
var humanByteSizeYAML []byte

//go:embed testdata/human_byte_size.manifest.yaml
var humanByteSizeManifestYAML []byte

type humanByteSizeCase struct {
	Name  string `yaml:"name"`
	Bytes int64  `yaml:"bytes"`
	Want  string `yaml:"want"`
}

type humanByteSizeFixture struct {
	RequiredNames []string            `yaml:"requiredNames"`
	Cases         []humanByteSizeCase `yaml:"cases"`
}

func loadHumanByteSizeFixture(t *testing.T) humanByteSizeFixture {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(humanByteSizeYAML))
	decoder.KnownFields(true)
	var fixture humanByteSizeFixture
	if err := decoder.Decode(&fixture); err != nil {
		t.Fatalf("decode human byte size fixture: %v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		t.Fatalf("human byte size fixture must contain exactly one YAML document: %v", err)
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	manifestDecoder := yaml.NewDecoder(bytes.NewReader(humanByteSizeManifestYAML))
	manifestDecoder.KnownFields(true)
	if err := manifestDecoder.Decode(&manifest); err != nil {
		t.Fatalf("decode human byte size manifest: %v", err)
	}
	present := make(map[string]bool, len(fixture.Cases))
	for _, fixtureCase := range fixture.Cases {
		if fixtureCase.Want == "" {
			t.Fatalf("human byte size case %q declares no expected rendering", fixtureCase.Name)
		}
		present[fixtureCase.Name] = true
	}
	for _, required := range manifest.RequiredNames {
		if !present[required] {
			t.Errorf("required human byte size case %q is missing from the fixture", required)
		}
	}
	return fixture
}

// TestHumanByteSize pins the one rendering every reader-facing note uses, so a
// bound note and an omission note in the same transcript cannot disagree about
// how a size is written.
func TestHumanByteSize(t *testing.T) {
	t.Parallel()
	for _, fixtureCase := range loadHumanByteSizeFixture(t).Cases {
		t.Run(fixtureCase.Name, func(t *testing.T) {
			t.Parallel()
			if got := HumanByteSize(fixtureCase.Bytes); got != fixtureCase.Want {
				t.Errorf("HumanByteSize(%d) = %q, want %q", fixtureCase.Bytes, got, fixtureCase.Want)
			}
		})
	}
}
