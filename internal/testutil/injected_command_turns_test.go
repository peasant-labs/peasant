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
	if len(fixture.Cases) != injectedCommandTurnFixtureCaseCount {
		t.Fatalf("loaded %d injected command turn cases, want %d", len(fixture.Cases), injectedCommandTurnFixtureCaseCount)
	}
}

func TestInjectedCommandTurnFixtureRejectsUnknownField(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("expectedCaseCount:"),
		[]byte("unknownFixtureField: true\nexpectedCaseCount:"),
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

func TestInjectedCommandTurnFixtureGuardsExactRowCount(t *testing.T) {
	t.Parallel()
	mutated := bytes.Replace(
		injectedCommandTurnFixtureYAML,
		[]byte("expectedCaseCount: 18"),
		[]byte("expectedCaseCount: 17"),
		1,
	)
	if _, err := decodeInjectedCommandTurnFixture(mutated); err == nil || !strings.Contains(err.Error(), "case count mismatch") {
		t.Fatalf("row-count error = %v, want exact-count rejection", err)
	}
}
