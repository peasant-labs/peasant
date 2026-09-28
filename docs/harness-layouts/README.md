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
