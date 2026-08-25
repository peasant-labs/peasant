package testutil

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoadInjectedCommandTurnFixture(t *testing.T) {
	t.Parallel()
	fixture, err := LoadInjectedCommandTurnFixture()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("loaded zero injected command turn cases")
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("loaded zero required injected command turn case names")
	}
}

func TestInjectedCommandTurnFixtureRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("requiredNames:"),
		[]byte("unknownFixtureField: true\nrequiredNames:"),
		1,
	)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil {
		t.Fatal("fixture decoder accepted an unknown field")
	}
}

func TestInjectedCommandTurnFixtureRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte{}, injectedCommandTurnFixtureYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document error = %v, want exact single-document rejection", err)
	}
}

// TestInjectedCommandTurnFixtureGuardsRequiredCaseDeletion mutation-proves the
// required-name manifest: deleting a required case's row (while leaving it
// named in requiredNames) must fail the load with a message naming the
// missing case. This replaces the old exact-row-count guard, which would
// have also failed on any addition to the corpus.
func TestInjectedCommandTurnFixtureGuardsRequiredCaseDeletion(t *testing.T) {
	t.Parallel()

	// Baseline: the real, unmutated fixture must load cleanly first, so a
	// failure below is known to come from the mutation and not a broken
	// manifest.
	if _, err := decodeInjectedCommandTurnFixture(injectedCommandTurnFixtureYAML); err != nil {
		t.Fatalf("baseline fixture failed to decode before mutation: %v", err)
	}

	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("  - name: command_name_only\n    harness: claude-code\n    sourceRole: user\n    content: <command-name>/project:review</command-name>\n    expectedRole: system\n\n"),
		nil,
		1,
	)
	_, err := decodeInjectedCommandTurnFixture(mutated)
	if err == nil {
		t.Fatal("fixture decoder accepted a corpus missing a required case row")
	}
	if !strings.Contains(err.Error(), `required case "command_name_only" is missing`) {
		t.Fatalf("deleted-required-case error = %v, want it to name the missing case", err)
	}
}
