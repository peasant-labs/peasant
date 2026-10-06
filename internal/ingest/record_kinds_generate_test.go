package ingest

import (
	"regexp"
	"strings"
	"testing"
)

var (
	recordKindHarnessSectionRE = regexp.MustCompile(`^  ([a-z0-9-]+):$`)
	recordKindAnchorDefRE      = regexp.MustCompile(`&([a-z0-9_]+)`)
	recordKindAliasRE          = regexp.MustCompile(`\*(fallback|[a-z0-9_]+)`)
)

// TestRecordKindYAMLAnchorsAreHarnessScoped proves every generated alias resolves
// inside its own harness section. YAML anchors are document-scoped, so a
// generator that shares one anchor map across harnesses emits rows whose meaning
// is defined by whichever harness happened to be generated first.
func TestRecordKindYAMLAnchorsAreHarnessScoped(t *testing.T) {
	raw, err := GenerateRecordKindRegistryYAML()
	if err != nil {
		t.Fatal(err)
	}
	harness := ""
	declared := map[string]bool{}
	sections := 0
	for i, line := range strings.Split(string(raw), "\n") {
		if match := recordKindHarnessSectionRE.FindStringSubmatch(line); match != nil {
			harness, declared, sections = match[1], map[string]bool{}, sections+1
			continue
		}
		// Only the two shapes that can carry an anchor or reference one: a kind
		// row and the harness fallback.
		if !strings.HasPrefix(line, "          - ") && !strings.HasPrefix(line, "    fallback:") {
			continue
		}
		for _, def := range recordKindAnchorDefRE.FindAllStringSubmatch(line, -1) {
			declared[def[1]] = true
		}
		for _, alias := range recordKindAliasRE.FindAllStringSubmatch(line, -1) {
			if !declared[alias[1]] {
				t.Errorf("line %d: harness %s references *%s, which that section never declares", i+1, harness, alias[1])
			}
		}
	}
	if sections != len(allRecordKindVocabularies()) {
		t.Errorf("generated %d harness sections, want %d", sections, len(allRecordKindVocabularies()))
	}
}
