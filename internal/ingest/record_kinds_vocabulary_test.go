package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"testing"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

//go:embed testdata/record_kinds_vocabulary.yaml
var recordKindsVocabularyYAML []byte

type recordKindVocabularyFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name      string            `yaml:"name"`
		Harness   Harness           `yaml:"harness"`
		Operation string            `yaml:"operation"`
		Context   RecordKindContext `yaml:"context"`
		Namespace string            `yaml:"namespace"`
		Kind      string            `yaml:"kind"`
		Outcome   string            `yaml:"outcome"`
		Status    RecordKindStatus  `yaml:"status"`
		Preview   RecordKindPreview `yaml:"preview"`
		Error     string            `yaml:"error"`
	} `yaml:"cases"`
}

// recordKindFixtureOutcome resolves a fixture outcome name through the shared
// closed set, so a fixture cannot name an outcome the vocabulary does not own.
func recordKindFixtureOutcome(t *testing.T, name string) indexformat.Outcome {
	t.Helper()
	for _, outcome := range indexformat.AllOutcomes() {
		if outcome.String() == name {
			return outcome
		}
	}
	t.Fatalf("fixture names unknown outcome %q", name)
	return indexformat.OutcomeOpaque
}

// recordKindFixtureInventory selects the mutated inventory. A fixture that
// names no context or namespace mutates the first inventory, keeping the
// historical cases' meaning; a named context and namespace mutate that
// inventory wherever it sits, so an inventory beyond the first is covered.
func recordKindFixtureInventory(t *testing.T, candidate recordKindAdapterVocabulary, context RecordKindContext, namespace string) int {
	t.Helper()
	if context == "" && namespace == "" {
		return 0
	}
	for i, inventory := range candidate.Inventories {
		if inventory.Context == context && inventory.Namespace == namespace {
			return i
		}
	}
	t.Fatalf("fixture names unknown inventory %s/%s", context, namespace)
	return 0
}

// recordKindFixtureRule locates the declared rule a fixture names, so an
// alteration case fails loudly on a renamed or removed declaration.
func recordKindFixtureRule(t *testing.T, candidate recordKindAdapterVocabulary, inventory int, kind string) int {
	t.Helper()
	for i, rule := range candidate.Inventories[inventory].Rules {
		if rule.Kind == kind {
			return i
		}
	}
	t.Fatalf("fixture names undeclared kind %q in %s/%s", kind, candidate.Inventories[inventory].Context, candidate.Inventories[inventory].Namespace)
	return 0
}

// recordKindRegistryWithCandidate lowers one harness's mutated candidate beside
// the untouched registrations, so a fixture observes what the declarations
// alone decide about the registry.
func recordKindRegistryWithCandidate(t *testing.T, candidate recordKindAdapterVocabulary) RecordKindRegistry {
	t.Helper()
	vocabularies := make([]recordKindAdapterVocabulary, 0, len(allRecordKindVocabularies()))
	for _, vocabulary := range allRecordKindVocabularies() {
		if vocabulary.Harness == candidate.Harness {
			vocabulary = candidate
		}
		vocabularies = append(vocabularies, vocabulary)
	}
	registry, err := generateRecordKindRegistryFrom(vocabularies)
	if err != nil {
		t.Fatal(err)
	}
	return registry
}

func TestRecordKindVocabularyCompleteness(t *testing.T) {
	var fixture recordKindVocabularyFixture
	decodeRegistryFixture(t, recordKindsVocabularyYAML, &fixture)
	names := make(map[string]bool, len(fixture.Cases))
	vocabularies := make(map[Harness]recordKindAdapterVocabulary, len(allRecordKindVocabularies()))
	for _, vocabulary := range allRecordKindVocabularies() {
		vocabularies[vocabulary.Harness] = vocabulary
	}
	for _, row := range fixture.Cases {
		if names[row.Name] {
			t.Fatalf("duplicate fixture %q", row.Name)
		}
		names[row.Name] = true
		t.Run(row.Name, func(t *testing.T) {
			vocabulary, ok := vocabularies[row.Harness]
			if !ok {
				t.Fatalf("fixture names unknown harness %q", row.Harness)
			}
			candidate := cloneRecordKindVocabulary(vocabulary)
			inventory := recordKindFixtureInventory(t, candidate, row.Context, row.Namespace)
			var result error
			switch row.Operation {
			case "exact":
			case "remove-first", "rename-first":
				rules := candidate.Inventories[inventory].Rules
				if len(rules) == 0 {
					t.Fatal("fixture inventory has no declaration to mutate")
				}
				if row.Operation == "remove-first" {
					candidate.Inventories[inventory].Rules = rules[:len(rules)-1]
				} else {
					rules[0].Kind += "-mutated"
				}
			case "remove-production":
				// The reverse mutation: the declaration keeps the kind while the
				// production census stops reporting it.
				if row.Kind == "" {
					t.Fatal("fixture names no kind to remove from the production census")
				}
				production := candidate.Inventories[inventory].Production
				candidate.Inventories[inventory].Production = func() recordKindProductionSet {
					mutated := production()
					delete(mutated, recordKindProductionKey{Kind: row.Kind, Match: RecordKindLiteral})
					return mutated
				}
			case "alter-outcome":
				// Altering a declared outcome must change what the registry
				// reports, which proves this inventory's rules are the ones the
				// generated registry lowers.
				rule := recordKindFixtureRule(t, candidate, inventory, row.Kind)
				outcome := recordKindFixtureOutcome(t, row.Outcome)
				candidate.Inventories[inventory].Rules[rule].Outcome = outcome
				registry := recordKindRegistryWithCandidate(t, candidate)
				declared := candidate.Inventories[inventory]
				lowered := registry.Harnesses[row.Harness].Lookup(declared.Context, declared.Namespace, row.Kind)
				if lowered.Outcome != outcome || lowered.Status != row.Status || lowered.Preview != row.Preview {
					result = fmt.Errorf("declared outcome %s for %s/%s/%s lowers to outcome %s, status %s, preview %s, not the declared %s/%s/%s", outcome, declared.Context, declared.Namespace, row.Kind, lowered.Outcome, lowered.Status, lowered.Preview, outcome, row.Status, row.Preview)
				}
			default:
				t.Fatalf("unknown fixture operation %q", row.Operation)
			}
			if result == nil {
				result = verifyRecordKindVocabularyCompleteness(candidate)
			}
			if row.Error == "" {
				if result != nil {
					t.Fatal(result)
				}
				return
			}
			if result == nil || !strings.Contains(result.Error(), row.Error) {
				t.Fatalf("got %v, want error containing %q", result, row.Error)
			}
		})
	}
	checkRegistryFixtureNames(t, names, fixture.RequiredNames)
	for harness := range DefaultAdapterRegistry {
		if _, ok := vocabularies[harness]; !ok {
			t.Errorf("missing vocabulary declaration for %s", harness)
		}
	}
}

func TestRecordKindVocabularyResolutionDefaultsToOpaque(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	for _, vocabulary := range allRecordKindVocabularies() {
		for _, inventory := range vocabulary.Inventories {
			kind := registry.Harnesses[vocabulary.Harness].Lookup(inventory.Context, inventory.Namespace, "valid-but-undeclared-kind")
			if kind.Outcome.String() != "opaque" || kind.EntryMode != RecordKindEntryModeRetainedEvidence || kind.Coordinates != RecordKindCoordinatesRequired {
				t.Errorf("%s/%s/%s fallback = outcome %s, entry %s, coordinates %s", vocabulary.Harness, inventory.Context, inventory.Namespace, kind.Outcome, kind.EntryMode, kind.Coordinates)
			}
		}
	}
}

func TestRecordKindIgnoredVocabularyRemainsEntryless(t *testing.T) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := GenerateRecordKindRegistryYAML()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, recordKindsYAML) {
		t.Fatal("entryless lowering proof used a stale generated registry")
	}
	for _, vocabulary := range allRecordKindVocabularies() {
		for _, inventory := range vocabulary.Inventories {
			for _, rule := range inventory.Rules {
				if rule.Outcome.String() != "ignored" {
					continue
				}
				kind := registry.Harnesses[vocabulary.Harness].Lookup(inventory.Context, inventory.Namespace, rule.Kind)
				if kind.EntryMode != RecordKindEntryModeNone {
					t.Errorf("%s ignored kind %q has entry mode %s", vocabulary.Harness, rule.Kind, kind.EntryMode)
				}
			}
		}
	}
}
