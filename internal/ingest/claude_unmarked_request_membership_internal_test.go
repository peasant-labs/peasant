package ingest

import (
	_ "embed"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_identity.yaml
var claudeIdentityMembershipYAML []byte

// TestClaudeIdentityUnmarkedRequestMembership pins the admission-proof set of
// request-accounting record kinds three ways: every fixture manifest kind is
// admitted by the production map, every admitted production kind appears in
// the manifest, and every declared kind appears in at least one fixture case
// line. Adding a kind to both the manifest and the map without a fixture case
// exercising it fails here.
func TestClaudeIdentityUnmarkedRequestMembership(t *testing.T) {
	var manifest struct {
		Required []string `yaml:"required_unmarked_request_record_kinds"`
	}
	if err := yaml.Unmarshal(claudeIdentityMembershipYAML, &manifest); err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool, len(manifest.Required))
	for _, kind := range manifest.Required {
		if declared[kind] {
			t.Fatalf("duplicate unmarked request record kind %q", kind)
		}
		declared[kind] = true
		if !claudeUnmarkedRequestRecordTypes[kind] {
			t.Fatalf("fixture kind %q is not admitted", kind)
		}
	}
	for kind := range claudeUnmarkedRequestRecordTypes {
		if !declared[kind] {
			t.Fatalf("admitted kind %q is missing from the fixture manifest", kind)
		}
	}
	var corpus struct {
		Cases []struct {
			Files []struct {
				Lines []string `yaml:"lines"`
			} `yaml:"files"`
		} `yaml:"cases"`
	}
	if err := yaml.Unmarshal(claudeIdentityMembershipYAML, &corpus); err != nil {
		t.Fatal(err)
	}
	for _, kind := range manifest.Required {
		needle := `"type":"` + kind + `"`
		var found bool
		for _, fixture := range corpus.Cases {
			for _, file := range fixture.Files {
				for _, line := range file.Lines {
					if strings.Contains(line, needle) {
						found = true
						break
					}
				}
				if found {
					break
				}
			}
			if found {
				break
			}
		}
		if !found {
			t.Fatalf("unmarked request record kind %q has no fixture case line", kind)
		}
	}
}
