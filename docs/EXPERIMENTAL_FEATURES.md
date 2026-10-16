# Experimental features

This register describes incomplete experiments and the work still needed before
they can be offered as product features. Experimental or planned does **not** mean
shipped, supported end-to-end, or already protected by a feature flag.

This register starts with file familiarity only; it is not an inventory or
reclassification of Peasant's compatibility tables. For existing experimental
switches for other surfaces, see [EXPERIMENTAL.md](../EXPERIMENTAL.md).

## File familiarity / understanding coverage

**Status: incomplete experiment; completion and feature-flag implementation are
deferred.** There is no end-to-end file-familiarity feature in the app today.

The intended purpose is to summarize recorded file activity and its recency, not
to prove human understanding. Existing helpers use tool-input paths and textual
file mentions as rough signals. Labels such as “discussed”, “questioned”, depth,
and freshness are heuristics: they do not reliably identify actual questions,
learning, comprehension, or forgetting. Missing activity is not evidence that a
person does not understand a file.

### Current surfaces

| Surface | Current status |
|---------|----------------|
| Recorded evidence | Transcripts can contain tool file paths and user mentions. Availability depends on what was captured; this is not a complete record of file activity. |
| Extraction helpers | Path extraction, interaction/depth heuristics, recency, trail, and suggestion helpers exist in [the API package](../internal/api/familiarity.go), but extraction is not wired into production ingestion. |
| Per-session storage | [Store write methods](../internal/store/familiarity.go) exist for `session_files`; no production producer calls them. Test seeding is not real capture. |
| Persistent aggregate storage | `file_familiarity` and its refresh/read methods exist, but refresh is not wired into production. |
| Backend read surface | [ProjectFamiliarity](../internal/api/store_adapter.go) reads visible `session_files` rows and aggregates them in memory, rather than reading `file_familiarity`. The [WebSocket topic](../internal/api/websocket.go) `project_familiarity` exists. In an ordinary unseeded deployment its successful response has empty files, trails, and suggestions, with zero familiarity and unexplored counts. Manually populated or preseeded stores may contain data. |
| Web consumer | Typed subscription plumbing exists, but no active app component subscribes or renders the result. The [inbound message handler](../web/src/contexts/WebSocketContext.tsx) does not handle successful `project_familiarity` payloads. |
| Validation | Helper tests and seeded visibility tests exercise parts of the code, not a working capture-to-app journey. |

An empty response is therefore not a measurement of project understanding or an
inventory of unexplored source files. Existing code and stored data remain in
place; this documentation removes nothing and changes no runtime behavior.

### Surfaces still needed

- A producer on the real generation/indexing path, with an explicit refresh,
  backfill, re-index, and invalidation lifecycle so derived activity stays current.
- Validated project/file aggregation, path identity, interaction semantics, and
  coverage bounds, including what any percentage or “unexplored” count measures.
- A real app consumer, including subscription, successful message handling,
  rendering, and loading, empty, and error states.
- Explicit source bounds and application of existing canonical redaction,
  selection, and visibility policies; no separate local policy derivation.
- Validation of the real producer-to-API-to-mounted-app path, not just helpers or
  seeded data.

These are outstanding surfaces, not a commitment to a delivery scope or date.

### Planned opt-in gate — not implemented

The plan is one explicit opt-in gate applied consistently to collection and
storage writes, API advertisement/availability, and the UI consumer. The flag
name, configuration mechanism, and defaults are undecided. **No familiarity flag
is implemented by this document, and current familiarity surfaces are not claimed
to be gated.** The existing `--experimental` switch is not an all-experiments
switch and does not enable this feature.

Feature completion and gate design/implementation are deferred future work and
need separate approval. Any resulting shared-wire change must land and be tagged
in the canonical schema repository before Peasant re-pins it, following the
[contract procedure](contract/versioning-procedure.md); a local wire fork is not
a substitute.
