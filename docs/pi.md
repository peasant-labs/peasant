# Pi Coding Agent recordings

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
16 KiB per string and 64 KiB per metadata record. A valid Pi recording can exceed
these limits. Peasant currently rejects that recording rather than silently removing
its extension state; the diagnostic names the exceeded limit. Ordinary transcript
text is not subject to the metadata string limit.

Pi v1/v2 compatibility, first-class images, extension execution, and fork navigation are
not supported. A separate native tool namespace is retained in local evidence, but public
detail/export currently refuses it because the released shared contract cannot represent
that field. Peasant does not silently concatenate it into the tool name or discard it.
