package ingest

import (
	_ "embed"
	"testing"

	"gopkg.in/yaml.v3"
)

//go:embed testdata/claude_identity.yaml
var claudeIdentityMembershipYAML []byte

// TestClaudeIdentityUnmarkedRequestMembership pins the admission-proof set of
// request-accounting record kinds: the fixture manifest names every admitted
// kind, and no production key exists outside the manifest. Adding a kind (or
// quietly widening the map) without a fixture case fails here.
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
}
