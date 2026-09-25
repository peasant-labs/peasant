package ingest

import (
	"fmt"
	"sort"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// recordKindAdapterVocabulary is the concrete local equivalent of the design's
// vocabulary input. Rules carry only the parser interpretation outcome; the
// Production callback is the adapter's runtime census used by exact
// completeness tests. Storage and preview policy are lowered centrally from
// Outcome; rendering is a consumer concern and is not declared here.
type recordKindAdapterVocabulary struct {
	Harness     Harness
	Inventories []recordKindInventoryDeclaration
}

// recordKindRule is the complete adapter-owned interpretation declaration.
// Storage and preview policy are lowered centrally from Outcome.
type recordKindRule struct {
	Context   RecordKindContext
	Namespace string
	Kind      string
	Match     RecordKindMatch
	Outcome   indexformat.Outcome
}

type recordKindInventoryID struct {
	Context   RecordKindContext
	Namespace string
}

type recordKindProductionKey struct {
	Context   RecordKindContext
	Namespace string
	Kind      string
	Match     RecordKindMatch
}

type recordKindProductionSet map[recordKindProductionKey]struct{}

type recordKindInventoryDeclaration struct {
	Context    RecordKindContext
	Namespace  string
	Sources    []RecordKindSource
	Rules      []recordKindRule
	Production func() recordKindProductionSet
}

func recordKindLiteral(kind string, outcome indexformat.Outcome) recordKindRule {
	return recordKindRule{Kind: kind, Match: RecordKindLiteral, Outcome: outcome}
}

func recordKindPrefix(prefix string, outcome indexformat.Outcome) recordKindRule {
	return recordKindRule{Kind: prefix, Match: RecordKindPrefix, Outcome: outcome}
}

func recordKindSource(file, symbol string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol}
}

func recordKindSwitchSource(file, symbol, expression string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, Switch: expression}
}

func recordKindPrefixSource(file, symbol, argument string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, PrefixArgument: argument}
}

func recordKindOperandSource(file, symbol, operand string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, EqualOperand: operand}
}

func recordKindCompleteOperandSource(file, symbol, operand string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, EqualOperand: operand, Complete: true}
}

func recordKindListSource(file, symbol string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, ListsOnly: true}
}

func recordKindCompleteSource(file, symbol, expression string) RecordKindSource {
	return RecordKindSource{File: file, Symbol: symbol, Switch: expression, Complete: true}
}

// allRecordKindVocabularies is the canonical harness order used by the
// generated artifact. Each entry is co-located with its adapter's dispatch or
// production census in the same package.
func allRecordKindVocabularies() []recordKindAdapterVocabulary {
	return []recordKindAdapterVocabulary{
		claudeVocabulary,
		cursorVocabulary,
		strikeVocabulary,
		piVocabulary,
		codexVocabulary,
		openCodeVocabulary,
	}
}

func recordKindProductionLiterals[T ~string](groups ...[]T) recordKindProductionSet {
	production := make(recordKindProductionSet)
	for _, group := range groups {
		for _, value := range group {
			production[recordKindProductionKey{Kind: string(value), Match: RecordKindLiteral}] = struct{}{}
		}
	}
	return production
}

func recordKindProductionMapKeys[K ~string, V any](groups ...map[K]V) []string {
	var keys []string
	for _, group := range groups {
		for key := range group {
			keys = append(keys, string(key))
		}
	}
	sort.Strings(keys)
	return keys
}

func mergeRecordKindProduction(groups ...recordKindProductionSet) recordKindProductionSet {
	merged := make(recordKindProductionSet)
	for _, group := range groups {
		for key := range group {
			merged[key] = struct{}{}
		}
	}
	return merged
}

func addRecordKindPrefixes(set recordKindProductionSet, prefixes ...string) recordKindProductionSet {
	for _, prefix := range prefixes {
		set[recordKindProductionKey{Kind: prefix, Match: RecordKindPrefix}] = struct{}{}
	}
	return set
}

func vocabularyRules(vocabulary recordKindAdapterVocabulary) map[recordKindProductionKey]indexformat.Outcome {
	rules := make(map[recordKindProductionKey]indexformat.Outcome)
	for _, inventory := range vocabulary.Inventories {
		for _, rule := range inventory.Rules {
			rule.Context = inventory.Context
			rule.Namespace = inventory.Namespace
			key := recordKindProductionKey{Context: rule.Context, Namespace: rule.Namespace, Kind: rule.Kind, Match: rule.Match}
			rules[key] = rule.Outcome
		}
	}
	return rules
}

func cloneRecordKindVocabulary(vocabulary recordKindAdapterVocabulary) recordKindAdapterVocabulary {
	clone := vocabulary
	clone.Inventories = append([]recordKindInventoryDeclaration(nil), vocabulary.Inventories...)
	for i := range clone.Inventories {
		clone.Inventories[i].Rules = append([]recordKindRule(nil), clone.Inventories[i].Rules...)
	}
	return clone
}

func verifyRecordKindVocabularyCompleteness(vocabulary recordKindAdapterVocabulary) error {
	if !vocabulary.Harness.IsKnown() {
		return fmt.Errorf("record-kind vocabulary %q names an unknown harness; register the adapter before declaring its interpretation", vocabulary.Harness)
	}
	declared := vocabularyRules(vocabulary)
	produced := make(recordKindProductionSet)
	for _, inventory := range vocabulary.Inventories {
		id := recordKindInventoryID{Context: inventory.Context, Namespace: inventory.Namespace}
		if inventory.Production == nil {
			return fmt.Errorf("record-kind vocabulary %s/%s/%s has no production census", vocabulary.Harness, id.Context, id.Namespace)
		}
		for key := range inventory.Production() {
			key.Context = id.Context
			key.Namespace = id.Namespace
			produced[key] = struct{}{}
		}
	}
	for key := range produced {
		if _, ok := declared[key]; !ok {
			return fmt.Errorf("record-kind vocabulary %s production %s/%s/%s (%s) has no declaration", vocabulary.Harness, key.Context, key.Namespace, key.Kind, key.Match)
		}
	}
	for key := range declared {
		if _, ok := produced[key]; !ok {
			return fmt.Errorf("record-kind vocabulary %s declaration %s/%s/%s (%s) is absent from production dispatch", vocabulary.Harness, key.Context, key.Namespace, key.Kind, key.Match)
		}
	}
	return nil
}
