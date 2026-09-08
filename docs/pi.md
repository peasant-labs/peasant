# Pi Coding Agent recordings

## Delivery status

Native ingestion, source preview, backend detail/export, and raw WebSocket validation
are implemented. The published transcript UI dependency currently pinned by Peasant
does not include Pi presentation. A Pi-capable upstream UI package must be published
and pinned before the full web build and browser transcript experience can be verified.
Do not treat a Go-only CLI build as proof of browser support.

## Reading recordings

Peasant reads normal Pi JSONL version 3 recordings. The default source is
`~/.pi/agent/sessions`. It scans first-level project directories, including project
directory symlinks. A configured source can also be a project directory or one JSONL file.

```yaml
sources:
  pi:
    enabled: true
    paths:
      - ~/.pi/agent/sessions
```

To select a different source for one harvest:

```console
peasant harvest --source-provider pi --source-path /recordings/project
```

`peasant kickstart` includes Pi in the ordinary project/session selection and previews
the source without importing it. The session header supplies the session identity and
working directory. The latest recorded session name supplies the display name; a blank
name clears an earlier name.

The transcript follows the last serialized entry back through its parents. It includes
pre-compaction history, thinking, tool calls and results, context messages (including
Pi messages with `display: false`), and compaction and branch summaries. Extension state
is separate metadata, not conversation text. Images appear as `[image omitted]`; image
bytes do not become public transcript content. Recorded usage keeps missing values
distinct from zero, and costs remain recorded harness estimates rather than billing.

The session model used by publication comes from the first recorded assistant model
observation, preferring its response model when present. Model-change state alone does
not supply an observation. Sessions with no recorded assistant model remain subject to
the ordinary publication no-model gate.

Original recordings are read-only. Harvest writes ordinary managed copies. `harvest logs`
copies files without creating an analytics database. `--dry-run` writes nothing. Reindexing
uses the original when present and falls back to the managed transcript when it is missing.
Local redaction changes the managed copy, not the original. A later source-present forced
harvest can read the original again, as with other harnesses.

An incomplete final JSONL line can be ignored while Pi is writing. Interior corruption,
duplicate keys or identifiers, unsupported versions, and inconsistent active paths reject
that recording with a diagnostic. Other recordings continue. Parser limits are 64 MiB per
file, 8 MiB per physical line, and 200,000 physical lines.

Selected extension metadata has separate shared-contract safety limits, including
64 KiB per string and 64 KiB per metadata record. A valid Pi recording can exceed
these limits. Peasant currently rejects that recording rather than silently removing
its extension state; the diagnostic names the exceeded limit. Ordinary transcript
text is not subject to the metadata string limit.

## Publishing and preservation

Publishing is an explicit user action through the ordinary share/push workflow. Detailed
usage and extension metadata require the receiving Village server to advertise their
content capabilities; an older server cannot silently accept a reduced transcript.
Push dry-run performs local validation without capability negotiation or upload.

The backend integration test starts from the native ingestion fixture, not a manually
constructed public transcript. It checks SQLite reopen, the common detail/export path,
CLI publication, encrypted Village storage/read/pull, and canonical rewriting of a
bare compatibility payload derived from the actual producer output. The fixture covers
complete and partial token accounting, exact zero, cost-only unknown usage, absent tool
and summary usage, context and summary turns, image placeholders, and non-conversational
metadata. This synthetic fixture does not establish that every real recording fits the
metadata limits or that the browser UI is ready. See [the E2E guide](e2e.md).

Pi v1/v2 compatibility, first-class images, extension execution, and fork navigation are
not supported. A separate native tool namespace remains distinct from the tool name through
local storage, detail, export, redaction, and publication. An explicitly recorded empty
namespace remains distinct from an omitted namespace. Publication requires the receiving
Village server to advertise `tool_namespace_v1`; Peasant refuses before upload otherwise.
