# Harness layouts

Peasant ingests only the harnesses in `schema.AllHarnesses`. For other coding tools,
`internal/harnesslayout` records where the tool keeps its session metadata and transcripts,
and a probe captures the structure of those files from a real machine. A captured layout is
the evidence a maintainer needs to write a full ingest adapter. It is not ingestion: nothing
here writes to the Peasant database, the managed transcript store, or any wire format.

A tool with a layout is not a `schema.Harness`. Promoting one to an ingested harness follows
the bestiary and schema contract procedure in [`../bestiary.md`](../bestiary.md) and
[`../contract/versioning-procedure.md`](../contract/versioning-procedure.md), and the local
harness migration that the store needs for a wider harness set.

## Capturing a layout

```console
peasant layout list
peasant layout capture <tool>
peasant layout capture <tool> --path /other/root --limit 0
```

`list` prints every declared tool, its default roots on this machine, and whether each root
exists. `capture` reads the roots read-only and prints a JSON report. For each session it
gives the recovered metadata counts and model identifiers. For each artifact it gives the
record count, a record-kind census, and every field path with the JSON types seen there.

The report never contains transcript text. By default it also removes root paths, session
identifiers, titles, project paths, timestamps, and error text, keeping only a coarse error
class for each failure, so it can be attached to a public issue.
`--include-metadata` keeps those values for local inspection. Do not post such a report.

`list` marks each root `present`, `absent` when the path does not exist, or an error class
(`permission`, `canceled`, `other`) when stating the path fails for another reason.

`--limit` is the maximum number of successful captures. A capture that fails does not consume
it, so a later session is still captured. `0` captures every session. A negative value is
rejected. When the report has no captures, stderr says that no sessions were found, or that
sessions were found and every capture failed.

### Report fields

A saved report is JSON. These fields are the contract:

- `records` counts decoded records. A record that does not decode is not one of them.
- `malformed` counts records that did not decode. A trailing `}`, `]`, or second value is
  malformed. Whitespace after a single value is not.
- `fields[].count` is how many values were seen at that path. It can be lower than `records`
  when a record omits the field, and higher when an array repeats it.
- `fields[].types` is the sorted set of JSON types seen at the path.
- `truncated` means the artifact had more than 1024 distinct paths. Paths are kept in a fixed
  order — the record, then object keys in lexicographic order, depth-first — so the same file
  always keeps the same subset.
- The shape-only report omits root paths, session identifiers, titles, project paths,
  timestamps, artifact paths, and error text. It keeps error classes, model identifiers,
  counts, record kinds, and field paths, including object keys.

Field paths use this grammar:

```text
$                  the record
.identifier        an object key matching [A-Za-z_][A-Za-z0-9_]*
["json-string"]    any other object key, as a JSON string
[]                 one array element
```

A key `a.b` is `$["a.b"]`. A nested object `a` then `b` is `$.a.b`. A key `x[]` is `$["x[]"]`.
An array element under `x` is `$.x[]`. An empty key is `$[""]`.

Adding an optional JSON field is compatible with reports already saved. Changing the path
grammar, the meaning of a count, or which values the shape-only report removes is a breaking
change for those reports.

## Declaring a layout

One Go file per tool, `internal/harnesslayout/<tool>.go`, declares:

- the `Tool` constant;
- the default roots for each operating system, using `{home}` and `{config}`
  (`$XDG_CONFIG_HOME` or `~/.config` on Linux, `~/Library/Application Support` on macOS,
  `%APPDATA%` on Windows);
- every artifact with its glob pattern, format, and role (transcript, metadata, index, or
  auxiliary);
- the public references the layout was derived from;
- a `Probe` that discovers sessions and captures them, and registers the layout from `init`.

Tests use synthetic fixtures under `internal/harnesslayout/testdata/<tool>/` that follow the
declared layout. Each tool has one page in this directory that describes its layout.
