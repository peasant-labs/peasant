package ingest

import (
	"bytes"
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed record_kinds.yaml
var recordKindsYAML []byte

// RecordKindStatus is the closed set of dispositions for one harness record
// or part kind. It answers what the parser does with the kind.
type RecordKindStatus string

const (
	// RecordKindRepresented stores the kind as session entries that read as
	// transcript content.
	RecordKindRepresented RecordKindStatus = "represented"
	// RecordKindTrackedOnly stores the kind for tracking and search without
	// showing it in the transcript.
	RecordKindTrackedOnly RecordKindStatus = "tracked-only"
	// RecordKindIgnoredControl writes no entry; the capture accounts for the
	// kind with a recorded reason and can still certify complete.
	RecordKindIgnoredControl RecordKindStatus = "ignored-control"
	// RecordKindRefused writes no entry; the typed refusal names the kind and
	// the capture stays incomplete until a build represents it.
	RecordKindRefused RecordKindStatus = "refused"
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

// RecordKindVisualized is the closed set for how a stored kind reaches a
// reader.
type RecordKindVisualized string

const (
	// RecordKindRendered reads as a transcript element through a named renderer.
	RecordKindRendered RecordKindVisualized = "rendered"
	// RecordKindHidden is stored but deliberately never shown.
	RecordKindHidden RecordKindVisualized = "hidden"
	// RecordKindPlanned is stored with rendering intended but not yet built.
	RecordKindPlanned RecordKindVisualized = "planned"
	// RecordKindNotApplicable writes no entry, so there is nothing to show.
	RecordKindNotApplicable RecordKindVisualized = "not-applicable"
)

// RecordKind is one row of the record-kind registry: the parser disposition,
// the stored shape, and the reader treatment of one harness kind.
type RecordKind struct {
	Kind       string               `yaml:"kind"`
	Status     RecordKindStatus     `yaml:"status"`
	Preview    RecordKindPreview    `yaml:"preview"`
	Payload    string               `yaml:"payload"`
	Visualized RecordKindVisualized `yaml:"visualized"`
	Renderer   string               `yaml:"renderer"`
	Reason     string               `yaml:"reason"`
	Source     string               `yaml:"source"`
}

// RecordKindHarness is the registry section for one harness: the harvester
// versions the mapping was written against, plus every mapped kind.
type RecordKindHarness struct {
	AdapterVersion int          `yaml:"adapter_version"`
	IndexerVersion int          `yaml:"indexer_version"`
	Kinds          []RecordKind `yaml:"kinds"`
}

// RecordKindRegistry is the parsed record_kinds.yaml: the per-harness mapping
// deliverable of peasant-labs/peasant#397.
//
//go:generate go run ../../scripts/record-kinds-docgen ../../docs/record-kinds.md
type RecordKindRegistry struct {
	Version   int                           `yaml:"version"`
	Harnesses map[Harness]RecordKindHarness `yaml:"harnesses"`
}

// recordKindsFormatVersion is the registry schema this build reads.
const recordKindsFormatVersion = 1

// LoadRecordKindRegistry parses and validates the embedded registry. A
// registry that breaks a closed set, repeats a kind, or leaves a required
// field empty is a programmer error the drift test reports.
func LoadRecordKindRegistry() (RecordKindRegistry, error) {
	var registry RecordKindRegistry
	decoder := yaml.NewDecoder(bytes.NewReader(recordKindsYAML))
	decoder.KnownFields(true)
	if err := decoder.Decode(&registry); err != nil {
		return RecordKindRegistry{}, fmt.Errorf("record-kind registry: decode: %w", err)
	}
	if err := registry.validate(); err != nil {
		return RecordKindRegistry{}, err
	}
	return registry, nil
}

var loadRecordKindsOnce = sync.OnceValues(LoadRecordKindRegistry)

// cachedRecordKindRegistry serves the production report path. Reporting stays
// best-effort: a caller that cannot load the registry omits the
// tracked-not-visualized list instead of failing the run.
func cachedRecordKindRegistry() (RecordKindRegistry, error) {
	return loadRecordKindsOnce()
}

func (r RecordKindRegistry) validate() error {
	if r.Version != recordKindsFormatVersion {
		return fmt.Errorf("record-kind registry: version %d is not the supported version %d", r.Version, recordKindsFormatVersion)
	}
	if len(r.Harnesses) == 0 {
		return fmt.Errorf("record-kind registry: no harness section")
	}
	for harness, section := range r.Harnesses {
		if !harness.IsKnown() {
			return fmt.Errorf("record-kind registry: harness %q is not a known harness", string(harness))
		}
		if section.AdapterVersion < 1 || section.IndexerVersion < 1 {
			return fmt.Errorf("record-kind registry: harness %q declares nonpositive versions", string(harness))
		}
		seen := make(map[string]bool, len(section.Kinds))
		for i, kind := range section.Kinds {
			where := fmt.Sprintf("record-kind registry: harness %q kind %d", string(harness), i)
			if kind.Kind == "" {
				return fmt.Errorf("%s: empty kind name", where)
			}
			if seen[kind.Kind] {
				return fmt.Errorf("%s: duplicate kind %q", where, kind.Kind)
			}
			seen[kind.Kind] = true
			switch kind.Status {
			case RecordKindRepresented, RecordKindTrackedOnly, RecordKindIgnoredControl, RecordKindRefused:
			default:
				return fmt.Errorf("%s %q: unknown status %q", where, kind.Kind, string(kind.Status))
			}
			switch kind.Preview {
			case RecordKindPreviewYes, RecordKindPreviewNo:
			default:
				return fmt.Errorf("%s %q: preview must be yes or no, got %q", where, kind.Kind, string(kind.Preview))
			}
			switch kind.Visualized {
			case RecordKindRendered, RecordKindHidden, RecordKindPlanned, RecordKindNotApplicable:
			default:
				return fmt.Errorf("%s %q: unknown visualized %q", where, kind.Kind, string(kind.Visualized))
			}
			if kind.Payload == "" {
				return fmt.Errorf("%s %q: empty payload; state the retained extra shape or none", where, kind.Kind)
			}
			if kind.Source == "" {
				return fmt.Errorf("%s %q: empty source; name the code reference or census", where, kind.Kind)
			}
			needsReason := kind.Status == RecordKindRefused || kind.Status == RecordKindIgnoredControl
			if needsReason && kind.Reason == "" {
				return fmt.Errorf("%s %q: status %q requires a reason", where, kind.Kind, string(kind.Status))
			}
			if !needsReason && kind.Reason != "" {
				return fmt.Errorf("%s %q: status %q carries no reason, got %q", where, kind.Kind, string(kind.Status), kind.Reason)
			}
			if kind.Visualized == RecordKindRendered && kind.Renderer == "" {
				return fmt.Errorf("%s %q: visualized rendered requires a renderer", where, kind.Kind)
			}
			if kind.Visualized != RecordKindRendered && kind.Renderer != "" {
				return fmt.Errorf("%s %q: visualized %q names no renderer, got %q", where, kind.Kind, string(kind.Visualized), kind.Renderer)
			}
		}
	}
	return nil
}

// KindsByName indexes one harness section by kind name.
func (h RecordKindHarness) KindsByName() map[string]RecordKind {
	byName := make(map[string]RecordKind, len(h.Kinds))
	for _, kind := range h.Kinds {
		byName[kind.Kind] = kind
	}
	return byName
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

// RecordKindTracked is one stored-but-not-visualized registry kind, named for
// the run report.
type RecordKindTracked struct {
	Harness Harness `json:"harness"`
	Kind    string  `json:"kind"`
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

// TrackedNotVisualized lists the kinds one harness stores without showing:
// stored rows no renderer shows yet. Ignored and refused kinds write no
// entry, and structural entries write none of their own, so only stored
// content with a hidden or planned treatment qualifies.
func (h RecordKindHarness) TrackedNotVisualized() []RecordKind {
	var hidden []RecordKind
	for _, kind := range h.Kinds {
		stored := kind.Status == RecordKindRepresented || kind.Status == RecordKindTrackedOnly
		unshown := kind.Visualized == RecordKindHidden || kind.Visualized == RecordKindPlanned
		if stored && unshown {
			hidden = append(hidden, kind)
		}
	}
	return hidden
}

// TrackedNotVisualized lists every kind the registry stores without showing,
// ordered by harness then file order. A stored-but-invisible kind is a visible
// decision in the run report, not a silent drop.
func (r RecordKindRegistry) TrackedNotVisualized() []RecordKindTracked {
	var out []RecordKindTracked
	harnesses := make([]Harness, 0, len(r.Harnesses))
	for harness := range r.Harnesses {
		harnesses = append(harnesses, harness)
	}
	sort.Slice(harnesses, func(i, j int) bool { return string(harnesses[i]) < string(harnesses[j]) })
	for _, harness := range harnesses {
		for _, kind := range r.Harnesses[harness].TrackedNotVisualized() {
			out = append(out, RecordKindTracked{Harness: harness, Kind: kind.Kind})
		}
	}
	return out
}

// Markdown renders the registry as the generated per-kind table owned by
// docs/record-kinds.md. Harness sections sort by name; kinds keep file order.
func (r RecordKindRegistry) Markdown() string {
	var out strings.Builder
	harnesses := make([]Harness, 0, len(r.Harnesses))
	for harness := range r.Harnesses {
		harnesses = append(harnesses, harness)
	}
	sort.Slice(harnesses, func(i, j int) bool { return string(harnesses[i]) < string(harnesses[j]) })
	for _, harness := range harnesses {
		section := r.Harnesses[harness]
		fmt.Fprintf(&out, "## %s (adapter %d, indexer %d)\n\n", string(harness), section.AdapterVersion, section.IndexerVersion)
		out.WriteString("| Kind | Status | Preview | Payload | Visualized | Detail |\n")
		out.WriteString("|---|---|---|---|---|---|\n")
		for _, kind := range section.Kinds {
			detail := kind.Reason
			if kind.Visualized == RecordKindRendered {
				detail = kind.Renderer
			}
			fmt.Fprintf(&out, "| `%s` | %s | %s | %s | %s | %s |\n",
				kind.Kind, string(kind.Status), string(kind.Preview), kind.Payload, string(kind.Visualized), detail)
		}
		out.WriteString("\n")
	}
	return out.String()
}
