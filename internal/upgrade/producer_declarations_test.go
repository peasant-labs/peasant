package upgrade

import (
	"maps"
	"slices"
	"testing"

	"github.com/peasant-labs/peasant/internal/ingest"
	"github.com/peasant-labs/peasant/internal/salt"
	"github.com/peasant-labs/peasant/internal/store"
	"github.com/peasant-labs/redact"
	"github.com/peasant-labs/schema"
)

// Every adapter's declared input must match the capability used by refresh.
func TestAdapterDeclarationInputMatchesExtractor(t *testing.T) {
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	inputs := make(map[string]ProducerInput)
	for _, declaration := range declarations {
		if declaration.Kind == ProducerKindAdapter {
			inputs[declaration.Name] = declaration.Input
		}
	}
	for harness, factory := range ingest.DefaultAdapterRegistry {
		input, ok := inputs[string(harness)]
		if !ok {
			t.Fatalf("adapter %s has no declaration", harness)
		}
		_, extracts := factory(nil, nil, salt.Salt{}).(ingest.TranscriptMetadataExtractor)
		if (input == ProducerInputStoredTranscript) != extracts {
			t.Errorf("adapter %s declares input %s; transcript extractor = %t", harness, input, extracts)
		}
	}
}

// TestEveryRegisteredProducerHasOneDeclaration enumerates each registry on
// its own and requires exactly one declaration per registered producer, and
// no declaration for anything no registry holds.
func TestEveryRegisteredProducerHasOneDeclaration(t *testing.T) {
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, harnesses := range [][]ingest.Harness{
		slices.Collect(maps.Keys(ingest.HarvesterVersionRegistry)),
		slices.Collect(maps.Keys(ingest.DefaultAdapterRegistry)),
	} {
		for _, harness := range harnesses {
			for _, kind := range []ProducerKind{ProducerKindAdapter, ProducerKindIndexer, ProducerKindFormat} {
				want[ProducerDeclaration{Kind: kind, Name: string(harness)}.String()] = true
			}
		}
	}
	for _, capability := range schema.AllContentCapabilities {
		want[ProducerDeclaration{Kind: ProducerKindContentModel, Name: string(capability)}.String()] = true
	}
	for _, kind := range []ProducerKind{ProducerKindMetadataSchema, ProducerKindRedactionRules, ProducerKindSchema} {
		want[ProducerDeclaration{Kind: kind}.String()] = true
	}
	got := map[string]bool{}
	kinds := map[ProducerKind]bool{}
	for _, declaration := range declarations {
		key := declaration.String()
		if got[key] {
			t.Fatalf("%s has more than one declaration", key)
		}
		got[key] = true
		kinds[declaration.Kind] = true
		if len(declaration.Versions) == 0 {
			t.Fatalf("%s declares no version", key)
		}
	}
	if !maps.Equal(got, want) {
		t.Fatalf("declared producers = %v, want one per registered producer %v", slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
	}
	for _, kind := range AllProducerKinds {
		if !kinds[kind] {
			t.Fatalf("producer kind %s has no declaration", kind)
		}
	}
}

// TestDeclarationVersionsAreTheRegistryValues reads each registry directly
// and requires the declaration to carry its value, so the table can never
// hold a version number of its own.
func TestDeclarationVersionsAreTheRegistryValues(t *testing.T) {
	declarations, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]ProducerDeclaration{}
	for _, declaration := range declarations {
		byKey[declaration.String()] = declaration
	}
	current := func(kind ProducerKind, name string) ProducerVersion {
		t.Helper()
		declaration, ok := byKey[ProducerDeclaration{Kind: kind, Name: name}.String()]
		if !ok {
			t.Fatalf("%s/%s has no declaration", kind, name)
		}
		return declaration.Current()
	}
	for harness, baseline := range ingest.HarvesterVersionRegistry {
		target := baseline
		if managed, ok := ingest.GenerationRepairTargets[harness]; ok {
			target = managed
		}
		for kind, value := range map[ProducerKind]int{
			ProducerKindAdapter: max(baseline.AdapterVersion, target.AdapterVersion),
			ProducerKindIndexer: max(baseline.IndexerVersion, target.IndexerVersion),
			ProducerKindFormat:  max(baseline.IndexVersion, target.IndexVersion),
		} {
			if got := current(kind, string(harness)); got != producerVersionOf(value) {
				t.Fatalf("%s/%s current version = %s, registry declares %d", kind, harness, got, value)
			}
		}
	}
	if got := current(ProducerKindMetadataSchema, ""); got != producerVersionOf(int(ingest.CurrentSchemaVersion)) {
		t.Fatalf("metadata schema version = %s, registry declares %d", got, ingest.CurrentSchemaVersion)
	}
	if got := current(ProducerKindRedactionRules, ""); got != ProducerVersion(redact.RuleSetVersion) {
		t.Fatalf("redaction rule set version = %s, registry declares %s", got, redact.RuleSetVersion)
	}
	if got := current(ProducerKindSchema, ""); got != producerVersionOf(store.CurrentSchemaVersion()) {
		t.Fatalf("store schema version = %s, registry declares %d", got, store.CurrentSchemaVersion())
	}
}

// TestProducerDeclarationsAreDeterministic pins the table's order, so a plan
// built from it is the same on every run.
func TestProducerDeclarationsAreDeterministic(t *testing.T) {
	first, err := ProducerDeclarations()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		next, err := ProducerDeclarations()
		if err != nil {
			t.Fatal(err)
		}
		if !slices.EqualFunc(first, next, func(a, b ProducerDeclaration) bool {
			return a.String() == b.String() && slices.Equal(a.Versions, b.Versions)
		}) {
			t.Fatalf("declaration order changed between runs:\n%v\n%v", first, next)
		}
	}
}
