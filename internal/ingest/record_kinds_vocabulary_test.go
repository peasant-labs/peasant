package ingest

import (
	"bytes"
	_ "embed"
	"strings"
	"testing"
)

//go:embed testdata/record_kinds_vocabulary.yaml
var recordKindsVocabularyYAML []byte

type recordKindVocabularyFixture struct {
	RequiredNames []string `yaml:"required_names"`
	Cases         []struct {
		Name      string  `yaml:"name"`
		Harness   Harness `yaml:"harness"`
		Operation string  `yaml:"operation"`
		Error     string  `yaml:"error"`
	} `yaml:"cases"`
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
			switch row.Operation {
			case "exact":
			case "remove-first", "rename-first":
				if len(candidate.Inventories) == 0 || len(candidate.Inventories[0].Rules) == 0 {
					t.Fatal("fixture harness has no declaration to mutate")
				}
				rules := candidate.Inventories[0].Rules
				if row.Operation == "remove-first" {
					candidate.Inventories[0].Rules = rules[:len(rules)-1]
				} else {
					rules[0].Kind += "-mutated"
				}
			default:
				t.Fatalf("unknown fixture operation %q", row.Operation)
			}
			err := verifyRecordKindVocabularyCompleteness(candidate)
			if row.Error == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), row.Error) {
				t.Fatalf("got %v, want error containing %q", err, row.Error)
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
