package testutil

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoadCommandInvocationTurnFixture(t *testing.T) {
	t.Parallel()
	fixture, err := LoadCommandInvocationTurnFixture()
	if err != nil {
		t.Fatal(err)
	}
	if len(fixture.Cases) == 0 {
		t.Fatal("loaded zero command invocation turn cases")
	}
	if len(fixture.RequiredNames) == 0 {
		t.Fatal("loaded zero required command invocation turn case names")
	}
}

func TestCommandInvocationTurnFixtureRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		commandInvocationTurnFixtureYAML,
		[]byte("requiredNames:"),
		[]byte("unknownFixtureField: true\nrequiredNames:"),
		1,
	)
	if _, err := decodeCommandInvocationTurnFixture(mutated); err == nil {
		t.Fatal("fixture decoder accepted an unknown field")
	}
}

func TestCommandInvocationTurnFixtureRejectsTrailingDocument(t *testing.T) {
	t.Parallel()
	mutated := append(append([]byte{}, commandInvocationTurnFixtureYAML...), []byte("\n---\nextra: true\n")...)
	if _, err := decodeCommandInvocationTurnFixture(mutated); err == nil || !strings.Contains(err.Error(), "exactly one YAML document") {
		t.Fatalf("trailing-document error = %v, want exact single-document rejection", err)
	}
}

// TestCommandInvocationTurnFixtureGuardsRequiredCaseDeletion mutation-proves the
// required-name manifest: deleting a required case's row (while leaving it
// named in requiredNames) must fail the load with a message naming the missing
// case. It is a deletion guard, not a row count, so adding a case to the corpus
// never touches it.
func TestCommandInvocationTurnFixtureGuardsRequiredCaseDeletion(t *testing.T) {
	t.Parallel()

	// Baseline: the real, unmutated fixture must load cleanly first, so a
	// failure below is known to come from the mutation and not a broken
	// manifest.
	if _, err := decodeCommandInvocationTurnFixture(commandInvocationTurnFixtureYAML); err != nil {
		t.Fatalf("baseline fixture failed to decode before mutation: %v", err)
	}

	const removedCase = "  - name: opencode_assistant_skill_invocation\n" +
		"    harness: opencode\n" +
		"    sourceRole: assistant\n" +
		"    content: \"\"\n" +
		"    storedName: /project:changelog\n" +
		"    expectedRole: assistant\n" +
		"    expectedCommand: true\n" +
		"    expectedName: /project:changelog\n\n"

	mutated := bytes.Replace(commandInvocationTurnFixtureYAML, []byte(removedCase), nil, 1)
	if bytes.Equal(mutated, commandInvocationTurnFixtureYAML) {
		t.Fatal("mutation removed nothing; the corpus row this guard deletes was reformatted, so update the literal above")
	}

	_, err := decodeCommandInvocationTurnFixture(mutated)
	if err == nil {
		t.Fatal("fixture decoder accepted a corpus missing a required case row")
	}
	if !strings.Contains(err.Error(), `is missing required case "opencode_assistant_skill_invocation"`) {
		t.Fatalf("deleted-required-case error = %v, want it to name the missing case", err)
	}
}
