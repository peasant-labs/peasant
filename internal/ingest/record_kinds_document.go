package ingest

import (
	"fmt"
	"strings"
)

// recordKindDocWidth is the generated document's wrap width, matching the
// hand-written prose around the generated sections.
const recordKindDocWidth = 80

// wrapRecordKindDoc reflows generated prose to the document width. Continuation
// lines carry the caller's indent so a wrapped sentence still reads as one list
// item.
func wrapRecordKindDoc(text, indent string) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return ""
	}
	var out strings.Builder
	line := indent
	for i, word := range words {
		candidate := word
		if i > 0 {
			candidate = " " + word
		}
		if len(line)+len(candidate) > recordKindDocWidth && len(line) > len(indent) {
			out.WriteString(strings.TrimRight(line, " "))
			out.WriteString("\n")
			line = indent + word
			continue
		}
		line += candidate
	}
	out.WriteString(line)
	return out.String()
}

// Document generates the entire human view, including field definitions. There
// is no independently maintained table or hand-written policy beside the YAML.
// Every closed-set statement below is rendered from the registry itself, so the
// prose cannot describe a status the registry does not declare, or deny one it
// does.
func (r RecordKindRegistry) Document() string {
	fallbacks := strings.Join(recordKindStatusNames(r.fallbackStatuses()), ", ")
	var out strings.Builder
	out.WriteString(`# Record-kind registry

Generated from the co-located adapter vocabulary declarations. Do not hand-edit this document.
Regenerate the registry and document with: go generate ./internal/ingest/

## Reading the registry

The registry describes parser behavior; it is not a wire schema or a native-input
allowlist. A previously unseen valid kind needs no named row or release to use
the ` + fallbacks + ` fallback. Adding a specialized handler does require a row.
Invalid JSON, invalid known fields, corrupt managed data and failed retention
remain errors, not successful unknown-kind captures.

- **Context** separates retained format-1 indexing from native-generation parsing.
  Pi uses format 1 even though its parser follows a native active graph.
- **Namespace** separates discriminator domains. Equal record and block names
  are not the same key. **Match** is literal unless explicitly marked prefix.
- **Status** is the closed set below, listed with what this build declares.
`)
	out.WriteString(wrapRecordKindDoc(r.statusProse(), "  "))
	out.WriteString(`
- **Preview** means the mapping can populate a human-readable content preview,
  not that every instance has nonempty text. Tool arguments alone are not preview
  text.
- **Payload** describes local retained shape, not a new public schema. Generic
  unknown payloads are complete JSON with source coordinates; known control limits
  do not license truncating unknown evidence.
- **Rendering is a consumer concern and is intentionally outside this registry.**
  The registry does not classify, track or report whether a kind is rendered.
- **Source** names first-party production code as generated reporting metadata.
  Each adapter vocabulary is compared exactly with its runtime dispatch/census;
  a source pointer is never parsed as Go syntax. Census observations are not an
  accept-list.
- **Versions** are verification targets: baseline and native overrides match
  their actual producer registries. They are not session producer stamps.

## Reporting and verification

`)
	out.WriteString(wrapRecordKindDoc(fmt.Sprintf(
		"Run rows of the %s disposition count **occurrences** separately from affected **sessions** for each harness/namespace/kind after successful writes. Multiple unknown blocks in one session are multiple occurrences and one affected session. The legacy refusal API counts per-session refusal inputs; it does not enumerate all unknown occurrences and must not be used for %s accounting.",
		fallbacks, fallbacks), ""))
	out.WriteString(`

Vocabulary completeness compares declarations and production censuses in both
directions without assuming a Go syntax shape. Behavioral fixtures separately
check classification and previews. These checks are not proof of all source,
storage, export and receiver paths: those require the harness and publication
integration suites. Publication requires validated retained evidence, partial
signaling and the receiver's preservation capability; failed or oversized
transfer must not silently discard evidence. Schema release/tag precedes pins.

` + strings.TrimRight(r.Markdown(), "\n") + "\n")
	return out.String()
}

// recordKindStatusNames renders statuses for prose.
func recordKindStatusNames(statuses []RecordKindStatus) []string {
	names := make([]string, 0, len(statuses))
	for _, status := range statuses {
		names = append(names, string(status))
	}
	return names
}
