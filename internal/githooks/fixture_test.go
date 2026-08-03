package githooks_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// decodeFixtureDocument decodes exactly one strictly-typed YAML document.
//
// Both guards exist because a silently-accepted fixture turns a green test into
// no evidence at all: an unknown field means a case is not driving what its
// author thinks it drives, and a trailing document means whole cases are being
// ignored. path names the file so a failure points at the thing to edit.
func decodeFixtureDocument[T any](data []byte, path string) (T, error) {
	var document T
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		return document, fmt.Errorf(
			"fixture rule failed: typed YAML fields must match the document schema; unknown or malformed data invalidates "+
				"the evidence this corpus is the only source of; where=%s loader=first-document decode; when=test fixture loading; "+
				"impact=the behaviour these cases prove cannot be trusted; fix=remove unknown fields and match the typed schema: %w",
			path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("found another YAML document")
		}
		return document, fmt.Errorf(
			"fixture rule failed: exactly one YAML document is allowed; trailing data is silently ignored, so cases below it "+
				"prove nothing; where=%s loader=end-of-document check; when=test fixture loading; "+
				"impact=the behaviour these cases prove cannot be trusted; fix=remove the second document so the next decode returns EOF: %w",
			path, err)
	}
	return document, nil
}

// fixtureCountGuard rejects a corpus whose declared and actual case counts
// disagree, or that is empty. A corpus that silently shrinks still passes every
// assertion it still contains.
func fixtureCountGuard(path string, declared, actual int) error {
	if actual == 0 || declared != actual {
		return fmt.Errorf(
			"fixture rule failed: declared and actual case counts must match and be non-zero, got expectedCaseCount=%d cases=%d; "+
				"a silently shrinking corpus still passes; where=%s loader=case-count validation; when=test fixture loading; "+
				"impact=the behaviour these cases prove cannot be trusted; fix=set expectedCaseCount to the number of cases present",
			declared, actual, path)
	}
	return nil
}

// fixtureCaseError reports one malformed case, naming the file and index.
func fixtureCaseError(path string, index int, what, fix string) error {
	return fmt.Errorf(
		"fixture rule failed: %s; a malformed case invalidates the evidence this corpus is the only source of; "+
			"where=%s case index %d; when=test fixture loading; impact=the behaviour this case proves cannot be trusted; %s",
		what, path, index, fix)
}

// fixtureUniqueNames rejects blank or duplicated case names: a failure has to
// name exactly one scenario.
func fixtureUniqueNames(path string, names []string) error {
	seen := make(map[string]bool, len(names))
	for index, name := range names {
		if strings.TrimSpace(name) == "" {
			return fixtureCaseError(path, index, "every case needs a name", "fix=name the case after the behaviour it proves")
		}
		if seen[name] {
			return fixtureCaseError(path, index, fmt.Sprintf("duplicate case name %q", name),
				"fix=give every case a unique name so a failure names exactly one scenario")
		}
		seen[name] = true
	}
	return nil
}
