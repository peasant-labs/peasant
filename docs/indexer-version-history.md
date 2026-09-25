# Historical indexer revisions

Before per-harness registration, all harnesses shared one indexer revision. The
existing SQLite `sessions.index_version` and `index_log.index_version` columns
retain those integers unchanged. They describe the parser, not the stored index
format. New parser targets live in `ingest.HarvesterVersionRegistry`.

## Per-harness retention revisions

The baseline below is the relational format-1 producer. These revisions describe
code eligibility, not a claim that any stored session was produced by that code.
Actual per-capture producer stamps are separate evidence.

| Harness | Adapter change | Indexer change | Reason |
|---|---|---|---|
| Claude Code | unchanged at 1 | 17 → 18 | Retain unknown records, system subtypes and nested blocks with redacted payloads and positions instead of settling on unknown-kind refusal. |
| Cursor | unchanged at 1 | 16 → 17 | Retain unknown roles/records/blocks while validating known content and preserving siblings. |
| Strike | unchanged at 1 | 16 → 17 | Retain unknown events and nested blocks through authoritative and retained indexing while keeping tool/process validation. |
| Pi | 1 → 2 | 16 → 17 | Native admission and metadata extraction now tolerate additive fields and unknown graph nodes; active-path indexing retains redacted unknown entry/role/block evidence. Format remains 1. |
| Codex | unchanged at 1 | 16 → 17 | Retained rollout indexing preserves unknown envelope/event/response/block evidence with actual traversal positions; raw acquisition is unchanged. |
| OpenCode | 1 → 2 | 16 → 17 | Legacy acquisition now retains opaque orphan parts instead of omitting them; retained indexing preserves unknown parts, inline blocks and tool-output children. |

Claude Code, Cursor and Strike changes are in capture/indexing, not adapter
metadata extraction, so their adapter revision does not change. Pi extraction
and indexing share the changed native document decoder, so both revisions change.
Managed-generation overrides are declared independently in
`NativeGenerationRepairTargets`; never replace them with the format-1 baseline.

| Native-generation harness | Adapter change | Indexer change | Index format | Reason |
|---|---|---|---|---|
| Codex | unchanged at 2 | 17 → 18 | 2 (unchanged) | Native history replay retains unknown source evidence, canonical item bodies and nested blocks with ownership/traversal coordinates; raw acquisition is unchanged. |
| OpenCode | 2 → 3 | 17 → 18 | 2 (unchanged) | Current row normalization retains unknown rows and blocks before projection; native indexing carries them through selected main/earlier evidence. |

OpenCode's private managed-projection version 3 and prior-evidence version 2 are
not indexformat versions and do not change the representation targets above.

## Former global revisions

| Revision | Behavior |
|---|---|
| 1 | Initial indexing support with index-log tracking. |
| 2 | Full-depth content block decomposition in production. |
| 3 | Populate tool kind and stop reason. |
| 4 | Project walk-up and provider role reclassification. |
| 5 | Skill body detection, depth-1 role propagation, direct/progress/queue-operation types. |
| 6 | Suppress duplicate OpenCode user text echoes; inherit parent roles for non-echo parts. |
| 7 | Skip empty text/thinking/default parts in OpenCode and Claude; remove read-time filters. |
| 8 | Restore empty-part storage, read-time empty suppression and consecutive deduplication; add part type. |
| 9 | Classify Claude compaction/context continuation as system messages. |
| 10 | Canonical Claude tool-result roles; AskUserQuestion results stay user; move previews from tool-result wrappers to children. |
| 11 | Classify empty progress/direct/queue-operation and tool-loaded wrappers as system messages. |
| 12 | Preserve exact Claude assistant model observations and their boundaries through suppression/deduplication. |
| 13 | Preserve OpenCode message graphs via root ParentEntryID; render orphan parts as system roots. |
| 14 | Honor Claude isMeta, classifying harness-injected user-role entries as system. |
| 15 | Honor OpenCode synthetic parts, classifying wholly harness-authored messages as system. |
