package ingest

import (
	"fmt"
	"strings"
)

// GenerateRecordKindRegistryYAML builds the checked-in reporting artifact
// from adapter vocabularies and the central interpretation lowering.
func GenerateRecordKindRegistryYAML() ([]byte, error) {
	registry, err := LoadRecordKindRegistry()
	if err != nil {
		return nil, fmt.Errorf("generate record-kind registry: build declarations: %w", err)
	}
	raw, err := marshalRecordKindRegistry(registry)
	if err != nil {
		return nil, fmt.Errorf("generate record-kind registry: encode YAML: %w", err)
	}
	return raw, nil
}

type recordKindProfileKey struct {
	Harness   Harness
	Context   RecordKindContext
	Namespace string
	Kind      string
	Match     RecordKindMatch
}

func marshalRecordKindRegistry(registry RecordKindRegistry) ([]byte, error) {
	profiles, err := recordKindDeclaredProfiles()
	if err != nil {
		return nil, err
	}
	var out strings.Builder
	out.WriteString("# Parser behavior, not a native-input allowlist. One declaration per line.\n")
	out.WriteString("# Inventories point at production admission/dispatch syntax. Unknown valid names\n")
	out.WriteString("# use fallback without a new declaration. Invalid known fields remain errors.\n")
	fmt.Fprintf(&out, "version: %d\n", registry.Version)
	out.WriteString("harnesses:\n")
	for _, vocabulary := range allRecordKindVocabularies() {
		section := registry.Harnesses[vocabulary.Harness]
		// Anchors are reset per harness: a YAML anchor is document-scoped, so a
		// shared map would make every harness after the first merge rows whose
		// meaning it never declared.
		anchors := make(map[string]bool)
		fmt.Fprintf(&out, "  %s:\n", vocabulary.Harness)
		fmt.Fprintf(&out, "    adapter_version: %d\n", section.AdapterVersion)
		fmt.Fprintf(&out, "    indexer_version: %d\n", section.IndexerVersion)
		fmt.Fprintf(&out, "    index_version: %d\n", section.IndexVersion)
		if section.NativeVersions != nil {
			fmt.Fprintf(&out, "    native_versions: {adapter_version: %d, indexer_version: %d, index_version: %d}\n", section.NativeVersions.AdapterVersion, section.NativeVersions.IndexerVersion, section.NativeVersions.IndexVersion)
		}
		writeRecordKindFallback(&out, section.Fallback, "    ")
		out.WriteString("    inventories:\n")
		for _, inventory := range section.Inventories {
			fmt.Fprintf(&out, "      - context: %s\n", inventory.Context)
			fmt.Fprintf(&out, "        namespace: %s\n", inventory.Namespace)
			out.WriteString("        sources:\n")
			for _, source := range inventory.Sources {
				fmt.Fprintf(&out, "          - %s\n", recordKindSourceYAML(source))
			}
			out.WriteString("        kinds:\n")
			for _, kind := range inventory.Kinds {
				key := recordKindProfileKey{Harness: vocabulary.Harness, Context: inventory.Context, Namespace: inventory.Namespace, Kind: kind.Kind, Match: kind.Match}
				profile, ok := profiles[key]
				if !ok {
					return nil, fmt.Errorf("record-kind YAML generation: %s has no central lowering profile", key)
				}
				writeRecordKindYAMLRow(&out, kind, profile, anchors)
			}
		}
	}
	return []byte(out.String()), nil
}

func recordKindDeclaredProfiles() (map[recordKindProfileKey]recordKindProfile, error) {
	profiles := make(map[recordKindProfileKey]recordKindProfile)
	for _, vocabulary := range allRecordKindVocabularies() {
		for _, inventory := range vocabulary.Inventories {
			for _, rule := range inventory.Rules {
				rule.Context = inventory.Context
				rule.Namespace = inventory.Namespace
				key := recordKindProfileKey{Harness: vocabulary.Harness, Context: rule.Context, Namespace: rule.Namespace, Kind: rule.Kind, Match: rule.Match}
				if _, exists := profiles[key]; exists {
					return nil, fmt.Errorf("record-kind YAML generation: duplicate declaration %s", key)
				}
				profiles[key] = recordKindProfileFor(rule)
			}
		}
	}
	return profiles, nil
}

func writeRecordKindFallback(out *strings.Builder, fallback RecordKind, indent string) {
	// Every harness declares its own fallback anchor so a section never merges a
	// definition another harness wrote.
	fmt.Fprintf(out, "%sfallback: &fallback\n", indent)
	fmt.Fprintf(out, "%s  status: %s\n", indent, fallback.Status)
	fmt.Fprintf(out, "%s  preview: %s\n", indent, fallback.Preview)
	fmt.Fprintf(out, "%s  payload: %s\n", indent, fallback.Payload)
	fmt.Fprintf(out, "%s  reason: %s\n", indent, fallback.Reason)
	fmt.Fprintf(out, "%s  source: %s\n", indent, fallback.Source)
}

func recordKindSourceYAML(source RecordKindSource) string {
	parts := []string{"file: " + source.File, "symbol: " + source.Symbol}
	if source.Switch != "" {
		parts = append(parts, "switch: "+source.Switch)
	}
	if source.PrefixArgument != "" {
		parts = append(parts, "prefix_argument: "+source.PrefixArgument)
	}
	if source.EqualOperand != "" {
		parts = append(parts, "equal_operand: "+source.EqualOperand)
	}
	if source.Complete {
		parts = append(parts, "complete: true")
	}
	if source.ListsOnly {
		parts = append(parts, "lists_only: true")
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// writeRecordKindYAMLRow writes one row, declaring its anchor on the first row
// that carries the anchor's canonical shape. Anchors already declared in this
// harness are merged with the fields that differ from that shape.
func writeRecordKindYAMLRow(out *strings.Builder, kind RecordKind, profile recordKindProfile, anchors map[string]bool) {
	shape, shared := recordKindAnchorShapes[profile.Anchor]
	if !shared {
		writeFullRecordKindYAMLRow(out, kind, "")
		return
	}
	if !anchors[profile.Anchor] {
		if !recordKindShapeMatches(kind, shape) {
			writeFullRecordKindYAMLRow(out, kind, "")
			return
		}
		anchors[profile.Anchor] = true
		writeFullRecordKindYAMLRow(out, kind, profile.Anchor)
		return
	}
	parts := []string{"<<: *" + profile.Anchor, "kind: " + kind.Kind}
	if kind.Match == RecordKindPrefix {
		parts = append(parts, "match: prefix")
	}
	if kind.Status != shape.Status {
		parts = append(parts, "status: "+string(kind.Status))
	}
	if kind.Preview != shape.Preview {
		parts = append(parts, "preview: "+string(kind.Preview))
	}
	if kind.Payload != shape.Payload {
		parts = append(parts, "payload: "+kind.Payload)
	}
	if kind.Reason != shape.Reason {
		parts = append(parts, "reason: "+kind.Reason)
	}
	fmt.Fprintf(out, "          - {%s}\n", strings.Join(parts, ", "))
}

func writeFullRecordKindYAMLRow(out *strings.Builder, kind RecordKind, anchor string) {
	prefix := "          - "
	if anchor != "" {
		prefix += "&" + anchor + " "
	}
	parts := []string{"kind: " + kind.Kind}
	if kind.Match == RecordKindPrefix {
		parts = append(parts, "match: prefix")
	}
	parts = append(parts,
		"status: "+string(kind.Status),
		"preview: "+string(kind.Preview),
		"payload: "+kind.Payload,
	)
	if kind.Reason != "" {
		parts = append(parts, "reason: "+kind.Reason)
	}
	fmt.Fprintf(out, "%s{%s}\n", prefix, strings.Join(parts, ", "))
}
