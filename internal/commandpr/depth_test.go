package commandpr

import (
	"encoding/json"
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestWrapperDepthBound pins that the nested-script fixtures fail on the depth
// bound, not on a quoting artifact: the five-level command detects with one
// more level of budget, and the four-level command already detects at the
// shipped bound.
func TestWrapperDepthBound(t *testing.T) {
	raw, err := os.ReadFile("testdata/detection.yaml")
	if err != nil {
		t.Fatalf("read detection fixture: %v", err)
	}
	var fixture struct {
		Cases []struct {
			Name  string `yaml:"name"`
			Calls []struct {
				Arguments string `yaml:"arguments"`
			} `yaml:"calls"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode detection fixture: %v", err)
	}
	command := func(name string) string {
		t.Helper()
		for _, c := range fixture.Cases {
			if c.Name != name {
				continue
			}
			if len(c.Calls) != 1 {
				t.Fatalf("case %q has %d calls, want 1", name, len(c.Calls))
			}
			var args map[string]string
			if err := json.Unmarshal([]byte(c.Calls[0].Arguments), &args); err != nil {
				t.Fatalf("decode %q arguments: %v", name, err)
			}
			return args["command"]
		}
		t.Fatalf("fixture case %q not found", name)
		return ""
	}

	four := command("bash -c nested four levels")
	five := command("bash -c nested five levels")
	if !commandInvokesGHPRCreate(four, maxWrapperDepth) {
		t.Fatalf("the four-level command must detect at maxWrapperDepth=%d", maxWrapperDepth)
	}
	if commandInvokesGHPRCreate(five, maxWrapperDepth) {
		t.Fatalf("the five-level command must not detect at maxWrapperDepth=%d", maxWrapperDepth)
	}
	if !commandInvokesGHPRCreate(five, maxWrapperDepth+1) {
		t.Fatalf("the five-level command must detect with one more level of budget; the fixture would otherwise be a quoting artifact, not the bound")
	}
}
