package harnesslayout_test

import (
	"bytes"
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/harnesslayout"
	"github.com/peasant-labs/schema"
)

//go:embed testdata/layout_harness.yaml
var layoutHarnessYAML []byte

//go:embed testdata/layout_harness.manifest.yaml
var layoutHarnessManifestYAML []byte

// TestLayoutHarnessPairs pins the harness identity of every declared layout.
// A tool that is its own product names its own known value; a tool that is one
// more store of a product already known names that product. The fixture is the
// deletion guard; the production registry is the subject.
func TestLayoutHarnessPairs(t *testing.T) {
	var fixture struct {
		Cases []struct {
			Name    string `yaml:"name"`
			Tool    string `yaml:"tool"`
			Harness string `yaml:"harness"`
		} `yaml:"cases"`
	}
	var manifest struct {
		RequiredNames []string `yaml:"requiredNames"`
	}
	for _, target := range []struct {
		name string
		data []byte
		out  any
	}{
		{"layout_harness.yaml", layoutHarnessYAML, &fixture},
		{"layout_harness.manifest.yaml", layoutHarnessManifestYAML, &manifest},
	} {
		decoder := yaml.NewDecoder(bytes.NewReader(target.data))
		decoder.KnownFields(true)
		if err := decoder.Decode(target.out); err != nil {
			t.Fatalf("decode %s: %v", target.name, err)
		}
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("layout harness fixture has no cases")
	}

	seen := map[string]bool{}
	for _, tc := range fixture.Cases {
		if tc.Name == "" || seen[tc.Name] {
			t.Fatalf("invalid or duplicate case name %q", tc.Name)
		}
		seen[tc.Name] = true
		layout, ok := harnesslayout.Lookup(harnesslayout.Tool(tc.Tool))
		if !ok {
			t.Errorf("case %q: tool %q is not registered", tc.Name, tc.Tool)
			continue
		}
		if got, want := string(layout.Harness), tc.Harness; got != want {
			t.Errorf("case %q: tool %q declares harness %q, want %q", tc.Name, tc.Tool, got, want)
		}
		if !layout.Harness.IsKnown() {
			t.Errorf("case %q: harness %q is not known to the schema", tc.Name, layout.Harness)
		}
	}
	for _, name := range manifest.RequiredNames {
		if !seen[name] {
			t.Errorf("missing required case %q", name)
		}
		delete(seen, name)
	}
	for name := range seen {
		t.Errorf("unexpected case %q; update the required-name manifest together", name)
	}

	for _, tc := range fixture.Cases {
		isKnown := false
		for _, h := range schema.Harnesses() {
			if string(h) == tc.Tool {
				isKnown = true
				break
			}
		}
		switch tc.Tool {
		case "cursor-cli", "cursor-ide-state":
			if isKnown {
				t.Errorf("cursor surface %q must not be its own harness value", tc.Tool)
			}
		default:
			if !isKnown {
				t.Errorf("tool %q is its own harness but is not in schema.Harnesses()", tc.Tool)
			}
		}
	}
}
