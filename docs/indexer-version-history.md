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

Claude Code, Cursor and Strike changes are in capture/indexing, not adapter
metadata extraction, so their adapter revision does not change. Pi extraction
and indexing share the changed native document decoder, so both revisions change.
Managed-generation overrides are declared independently in
`NativeGenerationRepairTargets`; never replace them with the format-1 baseline.

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
