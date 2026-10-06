package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"io"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// The embedded record_kinds.yaml is generated reporting output. Production
// never reads it: LoadRecordKindRegistry lowers the adapter vocabulary
// declarations directly, and the committed YAML exists so the artifact can be
// compared against fresh codegen. The decoder below is therefore a TEST-ONLY
// reader of that artifact, quarantined here so no production path can appear to
// depend on it. It infers the interpretation IR from the reported payload
// string, which is a lossy heuristic, so it must never become a second lowering
// path: registry.validate is the only gate it feeds, and the round-trip test
// below compares its decoded reporting fields against the generated registry.
//
//go:embed record_kinds.yaml
var recordKindsYAML []byte

// decodeEmbeddedRecordKindRegistry decodes the committed generated artifact and
// validates it. Its outcome/entry-mode/coordinates binding is the quarantined
// string heuristic; every other field comes straight from the artifact.
func decodeEmbeddedRecordKindRegistry(data []byte) (RecordKindRegistry, error) {
	var registry RecordKindRegistry
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&registry); err != nil {
		return RecordKindRegistry{}, fmt.Errorf("record-kind registry: decode: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return RecordKindRegistry{}, fmt.Errorf("record-kind registry: expected one YAML document; remove trailing content (decode: %v)", err)
	}
	for harness, section := range registry.Harnesses {
		for _, inventory := range section.Inventories {
			for _, kind := range inventory.Kinds {
				kind.Context, kind.Namespace = inventory.Context, inventory.Namespace
				if kind.Match == "" {
					kind.Match = RecordKindLiteral
				}
				if kind.Source == "" {
					var refs []string
					for _, source := range inventory.Sources {
						refs = append(refs, source.File+" "+source.Symbol)
					}
					kind.Source = strings.Join(refs, "; ")
				}
				bindDecodedRecordKindSemantics(&kind)
				section.Kinds = append(section.Kinds, kind)
			}
		}
		bindDecodedRecordKindSemantics(&section.Fallback)
		registry.Harnesses[harness] = section
	}
	if err := registry.validate(); err != nil {
		return RecordKindRegistry{}, err
	}
	return registry, nil
}

// bindDecodedRecordKindSemantics is the quarantined string heuristic: it reads
// the reported payload text to recover an interpretation outcome for a decoded
// row. It exists so a hand-mutated artifact can still be validated, not so the
// artifact can be interpreted. Keep it in this test-only file.
func bindDecodedRecordKindSemantics(kind *RecordKind) {
	outcome := indexformat.OutcomeText
	switch {
	case kind.Status == RecordKindRetainedUnknown:
		outcome = indexformat.OutcomeOpaque
	case kind.Status == RecordKindIgnoredControl, kind.Status == RecordKindRefused, kind.Payload == "state on owning entry; no independent row":
		outcome = indexformat.OutcomeIgnored
	case kind.Status == RecordKindTrackedOnly, strings.HasPrefix(kind.Payload, "bounded control extra"), kind.Payload == "compactMetadata":
		outcome = indexformat.OutcomeControl
	case strings.HasPrefix(kind.Payload, "tool name and arguments"):
		outcome = indexformat.OutcomeToolCall
	case strings.HasPrefix(kind.Payload, "tool output"), strings.HasPrefix(kind.Payload, "structured tool output"):
		outcome = indexformat.OutcomeToolResult
	}
	profile := recordKindOutcomeProfiles[outcome]
	kind.Outcome = outcome
	kind.EntryMode = profile.EntryMode
	kind.Coordinates = profile.Coordinates
}

// TestRecordKindsEmbeddedYAMLRoundTrips proves the committed generated artifact
// is a faithful encoding of the registry this build generates. It also proves
// every YAML anchor in the artifact resolves on its own: a generator that
// reused one harness's anchor map for a later harness would emit aliases with no
// definition in scope, and the decode below would fail.
func TestRecordKindsEmbeddedYAMLRoundTrips(t *testing.T) {
	generated, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeEmbeddedRecordKindRegistry(recordKindsYAML)
	if err != nil {
		t.Fatalf("committed generated registry does not decode: %v", err)
	}
	if len(decoded.Harnesses) != len(generated.Harnesses) {
		t.Fatalf("decoded %d harnesses, generated %d", len(decoded.Harnesses), len(generated.Harnesses))
	}
	for harness, wantSection := range generated.Harnesses {
		gotSection, ok := decoded.Harnesses[harness]
		if !ok {
			t.Errorf("harness %q missing from the committed artifact", harness)
			continue
		}
		if gotSection.AdapterVersion != wantSection.AdapterVersion || gotSection.IndexerVersion != wantSection.IndexerVersion || gotSection.IndexVersion != wantSection.IndexVersion {
			t.Errorf("harness %q versions differ: decoded %+v, generated %+v", harness, gotSection, wantSection)
		}
		if gotSection.Fallback.Status != wantSection.Fallback.Status || gotSection.Fallback.Payload != wantSection.Fallback.Payload || gotSection.Fallback.Reason != wantSection.Fallback.Reason {
			t.Errorf("harness %q fallback differs: decoded %+v, generated %+v", harness, gotSection.Fallback, wantSection.Fallback)
		}
		got := gotSection.KindsByKey()
		for key, want := range wantSection.KindsByKey() {
			row, ok := got[key]
			if !ok {
				t.Errorf("harness %q kind %+v missing from the committed artifact", harness, key)
				continue
			}
			if row.Status != want.Status || row.Preview != want.Preview || row.Payload != want.Payload || row.Reason != want.Reason || row.Source != want.Source {
				t.Errorf("harness %q kind %+v differs: decoded %+v, generated %+v", harness, key, row, want)
			}
		}
		if len(got) != len(wantSection.KindsByKey()) {
			t.Errorf("harness %q has %d decoded kinds, generated %d", harness, len(got), len(wantSection.KindsByKey()))
		}
	}
}
