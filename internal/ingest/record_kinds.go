package ingest

import (
	"fmt"
	"sort"
	"strings"

	"github.com/peasant-labs/peasant/internal/indexformat"
)

// RecordKindStatus is the closed set of dispositions for one harness record
// or part kind. It answers what the parser does with the kind.
type RecordKindStatus string

const (
	// RecordKindRepresented stores the kind as session entries that read as
	// transcript content.
	RecordKindRepresented RecordKindStatus = "represented"
	// RecordKindTrackedOnly stores the kind as non-conversation evidence
	// outside interpreted transcript entries.
	RecordKindTrackedOnly RecordKindStatus = "tracked-only"
	// RecordKindIgnoredControl writes no entry; the capture accounts for the
	// kind with a recorded reason and can still certify complete.
	RecordKindIgnoredControl RecordKindStatus = "ignored-control"
	// RecordKindRefused writes no entry; the typed refusal names the kind and
	// the capture stays incomplete until a build represents it.
	RecordKindRefused RecordKindStatus = "refused"
	// RecordKindRetainedUnknown preserves evidence without interpreting it.
	RecordKindRetainedUnknown RecordKindStatus = "retained-unknown"
)

// RecordKindPreview is the closed set for whether a kind carries a
// human-readable action line.
type RecordKindPreview string

const (
	// RecordKindPreviewYes stores a ContentPreview for the kind.
	RecordKindPreviewYes RecordKindPreview = "yes"
	// RecordKindPreviewNo stores no preview for the kind.
	RecordKindPreviewNo RecordKindPreview = "no"
)

// RecordKind is one row of the record-kind registry: the parser disposition
// and the stored shape of one harness kind. Rendering is a consumer concern and
// is intentionally outside this registry.
type RecordKind struct {
	Context     RecordKindContext               `yaml:"-"`
	Namespace   string                          `yaml:"-"`
	Match       RecordKindMatch                 `yaml:"match,omitempty"`
	Kind        string                          `yaml:"kind"`
	Status      RecordKindStatus                `yaml:"status"`
	Preview     RecordKindPreview               `yaml:"preview"`
	Payload     string                          `yaml:"payload"`
	Reason      string                          `yaml:"reason"`
	Source      string                          `yaml:"source"`
	Outcome     indexformat.Outcome             `yaml:"-"`
	EntryMode   RecordKindEntryMode             `yaml:"-"`
	Coordinates RecordKindCoordinateRequirement `yaml:"-"`
}

// RecordKindHarness is the registry section for one harness: the harvester
// versions the mapping was written against, plus every mapped kind.
type RecordKindHarness struct {
	AdapterVersion int                   `yaml:"adapter_version"`
	IndexerVersion int                   `yaml:"indexer_version"`
	IndexVersion   int                   `yaml:"index_version"`
	NativeVersions *RecordKindVersions   `yaml:"native_versions,omitempty"`
	Fallback       RecordKind            `yaml:"fallback"`
	Inventories    []RecordKindInventory `yaml:"inventories"`
	Kinds          []RecordKind          `yaml:"-"`
}

// RecordKindVersions identifies verification targets, never per-session stamps.
type RecordKindVersions struct {
	AdapterVersion int `yaml:"adapter_version"`
	IndexerVersion int `yaml:"indexer_version"`
	IndexVersion   int `yaml:"index_version"`
}

// RecordKindContext separates different producers of the same native spelling.
type RecordKindContext string

const (
	RecordKindRetained RecordKindContext = "retained-format-1"
	RecordKindNative   RecordKindContext = "native-generation"
)

// RecordKindMatch makes prefix families explicit, rather than fake literal kinds.
type RecordKindMatch string

const (
	RecordKindLiteral RecordKindMatch = "literal"
	RecordKindPrefix  RecordKindMatch = "prefix"
)

// RecordKindSource is generated reporting metadata that names first-party
// production code. Validation never parses these fields as Go syntax; exact
// vocabulary completeness is checked against adapter runtime censuses.
type RecordKindSource struct {
	File           string `yaml:"file"`
	Symbol         string `yaml:"symbol"`
	Switch         string `yaml:"switch,omitempty"`
	PrefixArgument string `yaml:"prefix_argument,omitempty"`
	EqualOperand   string `yaml:"equal_operand,omitempty"`
	Complete       bool   `yaml:"complete,omitempty"`
	ListsOnly      bool   `yaml:"lists_only,omitempty"`
}

type RecordKindInventory struct {
	Context   RecordKindContext  `yaml:"context"`
	Namespace string             `yaml:"namespace"`
	Sources   []RecordKindSource `yaml:"sources"`
	Kinds     []RecordKind       `yaml:"kinds"`
}

// RecordKindKey is the unambiguous identity used by qualified lookups.
type RecordKindKey struct {
	Context   RecordKindContext
	Namespace string
	Kind      string
	Match     RecordKindMatch
}

func (k RecordKind) Key() RecordKindKey {
	return RecordKindKey{k.Context, k.Namespace, k.Kind, k.Match}
}

// RecordKindRegistry is the generated local view of adapter vocabularies: the
// per-harness reporting artifact for peasant-labs/peasant#397.
//
//go:generate go run ../../scripts/record-kinds-docgen record_kinds.yaml ../../docs/record-kinds.md
type RecordKindRegistry struct {
	Version   int                           `yaml:"version"`
	Harnesses map[Harness]RecordKindHarness `yaml:"harnesses"`
}

// recordKindsFormatVersion is the registry schema this build reads.
const recordKindsFormatVersion = 3

// LoadRecordKindRegistry builds and validates the registry from adapter
// vocabulary declarations. The committed record_kinds.yaml is generated
// reporting output compared against fresh codegen by the drift gate; it is not
// a parser, interpretation, or runtime source.
func LoadRecordKindRegistry() (RecordKindRegistry, error) {
	return generateRecordKindRegistry()
}

func generateRecordKindRegistry() (RecordKindRegistry, error) {
	return generateRecordKindRegistryFrom(allRecordKindVocabularies())
}

// generateRecordKindRegistryFrom lowers the given adapter vocabularies. The
// production path passes the registered vocabularies; a mutation fixture
// passes a changed candidate to observe what the declarations alone decide.
func generateRecordKindRegistryFrom(vocabularies []recordKindAdapterVocabulary) (RecordKindRegistry, error) {
	registry := RecordKindRegistry{Version: recordKindsFormatVersion, Harnesses: make(map[Harness]RecordKindHarness, len(DefaultAdapterRegistry))}
	for _, vocabulary := range vocabularies {
		versions, ok := HarvesterVersionRegistry[vocabulary.Harness]
		if !ok {
			return RecordKindRegistry{}, fmt.Errorf("record-kind registry: harness %q has no harvester versions; register its parser targets before generating", vocabulary.Harness)
		}
		section := RecordKindHarness{
			AdapterVersion: versions.AdapterVersion,
			IndexerVersion: versions.IndexerVersion,
			IndexVersion:   versions.IndexVersion,
			Fallback:       lowerRecordKindFallback(RecordKindRetained, "", ""),
		}
		if native, ok := NativeGenerationRepairTargets[vocabulary.Harness]; ok {
			section.NativeVersions = &RecordKindVersions{AdapterVersion: native.AdapterVersion, IndexerVersion: native.IndexerVersion, IndexVersion: native.IndexVersion}
		}
		for _, declaration := range vocabulary.Inventories {
			inventory := RecordKindInventory{Context: declaration.Context, Namespace: declaration.Namespace, Sources: declaration.Sources}
			for _, declared := range declaration.Rules {
				declared.Context = declaration.Context
				declared.Namespace = declaration.Namespace
				kind := lowerRecordKindRule(declared)
				kind.Source = recordKindSourceLabel(inventory)
				inventory.Kinds = append(inventory.Kinds, kind)
				section.Kinds = append(section.Kinds, kind)
			}
			section.Inventories = append(section.Inventories, inventory)
		}
		registry.Harnesses[vocabulary.Harness] = section
	}
	if err := registry.validate(); err != nil {
		return RecordKindRegistry{}, err
	}
	return registry, nil
}

func (r RecordKindRegistry) validate() error {
	if r.Version != recordKindsFormatVersion {
		return fmt.Errorf("record-kind registry: version %d is not the supported version %d", r.Version, recordKindsFormatVersion)
	}
	for harness := range DefaultAdapterRegistry {
		if _, ok := r.Harnesses[harness]; !ok {
			return fmt.Errorf("record-kind registry: registered adapter %q has no section; declare its behavior", harness)
		}
	}
	for harness := range HarvesterVersionRegistry {
		if _, ok := r.Harnesses[harness]; !ok {
			return fmt.Errorf("record-kind registry: versioned harness %q has no section", harness)
		}
	}
	for harness, section := range r.Harnesses {
		if _, ok := DefaultAdapterRegistry[harness]; !ok {
			return fmt.Errorf("record-kind registry: harness %q has no supported adapter", string(harness))
		}
		target, ok := HarvesterVersionRegistry[harness]
		if !ok || target != (HarvesterVersions{section.AdapterVersion, section.IndexerVersion, section.IndexVersion}) {
			return fmt.Errorf("record-kind registry: harness %q baseline versions differ; verify against HarvesterVersionRegistry", harness)
		}
		native, hasNative := NativeGenerationRepairTargets[harness]
		if hasNative != (section.NativeVersions != nil) {
			return fmt.Errorf("record-kind registry: harness %q native version declaration missing or unexpected", harness)
		}
		if v := section.NativeVersions; v != nil && native != (HarvesterVersions{v.AdapterVersion, v.IndexerVersion, v.IndexVersion}) {
			return fmt.Errorf("record-kind registry: harness %q native versions differ; verify against NativeGenerationRepairTargets", harness)
		}
		fallback := section.Fallback
		if fallback.Kind != "" || fallback.Match != "" || fallback.Status != RecordKindRetainedUnknown || fallback.Preview != RecordKindPreviewNo || fallback.Payload == "" || fallback.Reason == "" || fallback.Source == "" || fallback.Outcome != indexformat.OutcomeOpaque || fallback.EntryMode != RecordKindEntryModeRetainedEvidence || fallback.Coordinates != RecordKindCoordinatesRequired {
			return fmt.Errorf("record-kind registry: harness %q must lower an unnamed valid-kind fallback to retained evidence with payload, coordinates, reason and source", harness)
		}
		inventories := make(map[string]bool)
		contexts := make(map[RecordKindContext]bool)
		for _, inventory := range section.Inventories {
			contexts[inventory.Context] = true
			key := string(inventory.Context) + "/" + inventory.Namespace
			if inventories[key] || inventory.Namespace == "" || len(inventory.Sources) == 0 || len(inventory.Kinds) == 0 {
				return fmt.Errorf("record-kind registry: harness %q invalid or duplicate inventory %q", harness, key)
			}
			inventories[key] = true
			if inventory.Context != RecordKindRetained && (inventory.Context != RecordKindNative || !hasNative) {
				return fmt.Errorf("record-kind registry: harness %q unsupported context %q", harness, inventory.Context)
			}
			for _, source := range inventory.Sources {
				if source.File == "" || source.Symbol == "" || strings.Contains(source.File, "..") || strings.HasSuffix(source.File, "_test.go") {
					return fmt.Errorf("record-kind registry: harness %q requires a production source selector", harness)
				}
				modes := 0
				if source.Switch != "" {
					modes++
				}
				if source.EqualOperand != "" {
					modes++
				}
				if source.PrefixArgument != "" {
					modes++
				}
				if modes > 1 || (modes > 0 && source.ListsOnly) {
					return fmt.Errorf("record-kind registry: harness %q source %s/%s has conflicting selection modes", harness, source.File, source.Symbol)
				}
			}
		}
		if len(inventories) == 0 {
			return fmt.Errorf("record-kind registry: harness %q has no inventories", harness)
		}
		if !contexts[RecordKindRetained] || hasNative != contexts[RecordKindNative] {
			return fmt.Errorf("record-kind registry: harness %q context inventory does not cover its producer targets", harness)
		}
		seen := make(map[RecordKindKey]bool, len(section.Kinds))
		for i, kind := range section.Kinds {
			where := fmt.Sprintf("record-kind registry: harness %q kind %d", string(harness), i)
			if kind.Kind == "" {
				return fmt.Errorf("%s: empty kind name", where)
			}
			if seen[kind.Key()] {
				return fmt.Errorf("%s: duplicate kind %q", where, kind.Kind)
			}
			seen[kind.Key()] = true
			for _, other := range section.Kinds[:i] {
				if kind.Context != other.Context || kind.Namespace != other.Namespace {
					continue
				}
				if kind.Kind == other.Kind || (kind.Match == RecordKindPrefix && strings.HasPrefix(other.Kind, kind.Kind)) || (other.Match == RecordKindPrefix && strings.HasPrefix(kind.Kind, other.Kind)) {
					return fmt.Errorf("%s: ambiguous overlapping match for %q", where, kind.Kind)
				}
			}
			if kind.Match != RecordKindLiteral && kind.Match != RecordKindPrefix {
				return fmt.Errorf("%s: invalid match %q", where, kind.Match)
			}
			switch kind.Status {
			case RecordKindRepresented, RecordKindTrackedOnly, RecordKindIgnoredControl, RecordKindRefused, RecordKindRetainedUnknown:
			default:
				return fmt.Errorf("%s %q: unknown status %q", where, kind.Kind, string(kind.Status))
			}
			switch kind.Preview {
			case RecordKindPreviewYes, RecordKindPreviewNo:
			default:
				return fmt.Errorf("%s %q: preview must be yes or no, got %q", where, kind.Kind, string(kind.Preview))
			}
			if kind.Payload == "" {
				return fmt.Errorf("%s %q: empty payload; state the retained extra shape or none", where, kind.Kind)
			}
			if kind.Source == "" {
				return fmt.Errorf("%s %q: empty source; name the code reference or census", where, kind.Kind)
			}
			needsReason := kind.Status == RecordKindRefused || kind.Status == RecordKindIgnoredControl || kind.Status == RecordKindRetainedUnknown
			if needsReason && kind.Reason == "" {
				return fmt.Errorf("%s %q: status %q requires a reason", where, kind.Kind, string(kind.Status))
			}
			if !needsReason && kind.Reason != "" {
				return fmt.Errorf("%s %q: status %q carries no reason, got %q", where, kind.Kind, string(kind.Status), kind.Reason)
			}
			if !kind.Outcome.IsValid() {
				return fmt.Errorf("%s %q: outcome %q is outside the shared closed set", where, kind.Kind, kind.Outcome.String())
			}
			switch kind.Outcome {
			case indexformat.OutcomeOpaque:
				if kind.EntryMode != RecordKindEntryModeRetainedEvidence || kind.Coordinates != RecordKindCoordinatesRequired {
					return fmt.Errorf("%s %q: opaque evidence must retain complete payload coordinates", where, kind.Kind)
				}
			case indexformat.OutcomeIgnored:
				if kind.EntryMode != RecordKindEntryModeNone || kind.Coordinates != RecordKindCoordinatesNone {
					return fmt.Errorf("%s %q: recognized ignored input must remain entryless", where, kind.Kind)
				}
			default:
				if kind.EntryMode != RecordKindEntryModeRepresented || kind.Coordinates != RecordKindCoordinatesNone {
					return fmt.Errorf("%s %q: represented interpretation must emit an ordinary entry without fallback coordinates", where, kind.Kind)
				}
			}
		}
	}
	return nil
}

// KindsByName is the legacy name-only view. Ambiguous names are omitted, never
// overwritten. New consumers must use KindsByKey to retain context/namespace.
func (h RecordKindHarness) KindsByName() map[string]RecordKind {
	byName := make(map[string]RecordKind, len(h.Kinds))
	duplicates := make(map[string]bool)
	for _, kind := range h.Kinds {
		if _, exists := byName[kind.Kind]; exists {
			duplicates[kind.Kind] = true
		}
		byName[kind.Kind] = kind
	}
	for name := range duplicates {
		delete(byName, name)
	}
	return byName
}

func (h RecordKindHarness) KindsByKey() map[RecordKindKey]RecordKind {
	result := make(map[RecordKindKey]RecordKind, len(h.Kinds))
	for _, kind := range h.Kinds {
		result[kind.Key()] = kind
	}
	return result
}

// Lookup describes handling; it never gates acceptance of arbitrary native names.
// Malformed known input remains an error in the owning parser, not a new kind.
func (h RecordKindHarness) Lookup(context RecordKindContext, namespace, name string) RecordKind {
	for _, kind := range h.Kinds {
		if kind.Context == context && kind.Namespace == namespace && ((kind.Match == RecordKindLiteral && kind.Kind == name) || (kind.Match == RecordKindPrefix && strings.HasPrefix(name, kind.Kind))) {
			return kind
		}
	}
	fallback := h.Fallback
	fallback.Context, fallback.Namespace, fallback.Kind, fallback.Match = context, namespace, name, RecordKindLiteral
	return fallback
}

// RecordKindRefusal is one strict-parser refusal: the harness and the kind
// this build does not represent. The pipeline aggregates one entry per
// refused session into the run report.
type RecordKindRefusal struct {
	Harness Harness `json:"harness"`
	Kind    string  `json:"kind"`
}

// RecordKindRefusalCount is the run-report row for one refused kind.
type RecordKindRefusalCount struct {
	Harness Harness `json:"harness"`
	Kind    string  `json:"kind"`
	Count   int     `json:"count"`
}

// AggregateRecordKindRefusals folds per-session refusals into deterministic
// per-kind counts, ordered by harness then kind.
func AggregateRecordKindRefusals(refusals []RecordKindRefusal) []RecordKindRefusalCount {
	counts := make(map[RecordKindRefusal]int, len(refusals))
	for _, refusal := range refusals {
		counts[refusal]++
	}
	out := make([]RecordKindRefusalCount, 0, len(counts))
	for refusal, count := range counts {
		out = append(out, RecordKindRefusalCount{Harness: refusal.Harness, Kind: refusal.Kind, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if string(out[i].Harness) != string(out[j].Harness) {
			return string(out[i].Harness) < string(out[j].Harness)
		}
		return out[i].Kind < out[j].Kind
	})
	return out
}

// sortedHarnesses is the canonical harness order for every generated view.
func (r RecordKindRegistry) sortedHarnesses() []Harness {
	harnesses := make([]Harness, 0, len(r.Harnesses))
	for harness := range r.Harnesses {
		harnesses = append(harnesses, harness)
	}
	sort.Slice(harnesses, func(i, j int) bool { return string(harnesses[i]) < string(harnesses[j]) })
	return harnesses
}

// statusRowCounts counts the declared rows per status across every harness.
func (r RecordKindRegistry) statusRowCounts() map[RecordKindStatus]int {
	counts := make(map[RecordKindStatus]int, len(recordKindStatusClosedSet))
	for _, harness := range r.sortedHarnesses() {
		for _, kind := range r.Harnesses[harness].Kinds {
			counts[kind.Status]++
		}
	}
	return counts
}

// fallbackStatuses names the dispositions the unseen-valid-kind fallback lowers
// to, in closed-set order. validate() requires one retained-evidence fallback in
// every section, so a valid registry yields exactly one.
func (r RecordKindRegistry) fallbackStatuses() []RecordKindStatus {
	fallbacks := make(map[RecordKindStatus]bool, len(recordKindStatusClosedSet))
	for _, harness := range r.sortedHarnesses() {
		if status := r.Harnesses[harness].Fallback.Status; status != "" {
			fallbacks[status] = true
		}
	}
	var statuses []RecordKindStatus
	for _, status := range recordKindStatusClosedSet {
		if fallbacks[status] {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

// statusProse describes the status closed set from the closed-set table and this
// registry's own rows. What each status means, how many rows carry it, and
// whether it is the open fallback are all read from the registry, so the text
// cannot contradict a future declared status.
func (r RecordKindRegistry) statusProse() string {
	counts := r.statusRowCounts()
	fallbacks := make(map[RecordKindStatus]bool, len(recordKindStatusClosedSet))
	for _, status := range r.fallbackStatuses() {
		fallbacks[status] = true
	}
	entries := make([]string, 0, len(recordKindStatusClosedSet))
	for _, status := range recordKindStatusClosedSet {
		note, ok := recordKindStatusNotes[status]
		if !ok {
			note = "no documented meaning; add one to recordKindStatusNotes"
		}
		entry := fmt.Sprintf("**%s** (%s) — %d rows", status, note, counts[status])
		if counts[status] == 0 {
			entry += ", declared by no kind in this build"
		}
		if fallbacks[status] {
			entry += ", and the unseen-valid-kind fallback of every harness"
		}
		entries = append(entries, entry)
	}
	return strings.Join(entries, "; ") + "."
}

// Markdown renders the registry as the generated per-kind table owned by
// docs/record-kinds.md. Harness sections sort by name; kinds keep file order.
func (r RecordKindRegistry) Markdown() string {
	var out strings.Builder
	for _, harness := range r.sortedHarnesses() {
		section := r.Harnesses[harness]
		fmt.Fprintf(&out, "## %s (adapter %d, indexer %d)\n\n", string(harness), section.AdapterVersion, section.IndexerVersion)
		fmt.Fprintf(&out, "Baseline index format: %d.\n\n", section.IndexVersion)
		if v := section.NativeVersions; v != nil {
			fmt.Fprintf(&out, "Native generation: adapter %d, indexer %d, index format %d.\n\n", v.AdapterVersion, v.IndexerVersion, v.IndexVersion)
		}
		fmt.Fprintf(&out, "Unseen valid kinds: **%s**, preview **%s**. %s Payload: %s. Source: `%s`.\n\n", section.Fallback.Status, section.Fallback.Preview, section.Fallback.Reason, section.Fallback.Payload, section.Fallback.Source)
		out.WriteString("| Context | Namespace | Kind | Match | Status | Preview | Payload | Detail | Source |\n")
		out.WriteString("|---|---|---|---|---|---|---|---|---|\n")
		for _, kind := range section.Kinds {
			fmt.Fprintf(&out, "| %s | %s | `%s` | %s | %s | %s | %s | %s | `%s` |\n",
				kind.Context, kind.Namespace, kind.Kind, kind.Match, kind.Status, kind.Preview, kind.Payload, kind.Reason, kind.Source)
		}
		out.WriteString("\n")
	}
	return out.String()
}
