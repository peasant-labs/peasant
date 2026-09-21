package ingest

import "strings"

// Document generates the entire human view, including field definitions. There
// is no independently maintained table or hand-written policy beside the YAML.
func (r RecordKindRegistry) Document() string {
	return `# Record-kind registry

Generated from internal/ingest/record_kinds.yaml. Do not hand-edit this document.
Regenerate with: go generate ./internal/ingest/

## Reading the registry

The registry describes parser behavior; it is not a wire schema or a native-input
allowlist. A previously unseen valid kind needs no named row or release to use
the retained-unknown fallback. Adding a specialized handler does require a row.
Invalid JSON, invalid known fields, corrupt managed data and failed retention
remain errors, not successful unknown-kind captures.

- **Context** separates retained format-1 indexing from native-generation parsing.
  Pi uses format 1 even though its parser follows a native active graph.
- **Namespace** separates discriminator domains. Equal record and block names
  are not the same key. **Match** is literal unless explicitly marked prefix.
- **Status** is represented (interpreted entries or owning-entry state),
  tracked-only (stored non-conversation evidence), ignored-control (no row),
  retained-unknown (complete redacted evidence without interpretation), or refused
  (a known unsupported shape; never the default for arbitrary valid new names).
- **Preview** means the mapping can populate a human-readable content preview,
  not that every instance has nonempty text. Tool arguments alone are not preview
  text. Renderer coverage is separate from this local storage property.
- **Payload** describes local retained shape, not a new public schema. Generic
  unknown payloads are complete JSON with source coordinates; known control limits
  do not license truncating unknown evidence.
- **Visualized** is rendered (named projection/renderer), hidden, planned or
  not-applicable (entryless or structural). Tool display can depend on a recognized
  tool kind and pairing; storing a generic tool name does not prove a renderer.
  Unknown-record display is deferred; no new transcript renderer is claimed.
- **Source** names first-party production code. Inventories are extracted from
  actual dispatch/admission syntax, including explicit prefix matching, not a
  second test-only vocabulary. Census observations are not an accept-list.
- **Versions** are verification targets: baseline and native overrides match
  their actual producer registries. They are not session producer stamps.

## Reporting and verification

Retained-unknown run rows count **occurrences** separately from affected
**sessions** for each harness/namespace/kind after successful writes. Multiple
unknown blocks in one session are multiple occurrences and one affected session.
The legacy refusal API counts per-session refusal inputs; it does not enumerate
all unknown occurrences and must not be used for retained-unknown accounting.

Tracked-not-visualized is **registry-wide coverage**, not evidence that those
kinds occurred in this run. It includes represented/planned and tracked/hidden
rows, qualified by context and namespace. Unseen names have no static row; their
actual occurrences belong in the run's retained-unknown summary.

Source inventory gates compare both directions. Behavioral fixtures separately
check classification and previews. These checks are not proof of all source,
storage, export and receiver paths: those require the harness and publication
integration suites. Publication requires validated retained evidence, partial
signaling and the receiver's preservation capability; failed or oversized
transfer must not silently discard evidence. Schema release/tag precedes pins.

` + strings.TrimRight(r.Markdown(), "\n") + "\n"
}
