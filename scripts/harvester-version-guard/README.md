# Harvester version guard

Compare committed parser behavior with an explicitly reviewed base:

```sh
nix develop -c make check-harvester-versions BASE=<base-commit> CANDIDATE=HEAD
```

Working edits are excluded. The PR test workflow compares its base SHA with the
checked-out candidate. Each parser capture uses `go test -race` with the native
cases in parallel; the fan-out is the guard's own concurrent code. Ordinary builds
do not invoke the guard.

The runner archives both revisions into a temporary directory and runs one shared
probe over both revisions' existing named synthetic corpora. The candidate
corpus pass observes every case it declares; the base pass replays the shared
cases whose bodies changed, so an edit is judged against the old bytes. A case
the probe did not actually process fails the run. Each corpus is fed unchanged
to both implementations. It compares native adapter extraction and
OpenCode SQLite materialization separately from completion-checked index
results (the data accepted for persistence, including authoritative full content
where supported). It never opens the user's native
history or analytics database.

Every changed harness must increase its affected `AdapterVersion` or
`IndexerVersion` in `internal/ingest/harvester_versions.go`. A harness with a
`GenerationRepairTargets` entry is also built through its indexer's
`BuildGeneration`, the managed-generation lane a format-2 store indexes with,
and that output is compared against the entry's own `IndexerVersion` under the
`<harness>@generation` name. The builder gets the captured bytes, the extracted
metadata, no prior evidence, a fixed identity and a sequential allocator. The
OpenCode lane reads a SQLite snapshot through the pipeline and is not built by
the probe. Shared helpers can
require several harness bumps. Failure becoming success, or success becoming
failure, is a behavior change. A new fixture alone causes no bump when both
parsers produce the same result. Old fixture inputs remain covered when the
candidate edits them; a removal is caught by the `required_cases` manifest.
There are no editable expected digests.

Inputs come from the native Claude/Codex/Cursor E2E manifests, Strike protocol
files, Pi capture and native-v3 structure cases, and the named index-completion
and OpenCode native fixture families.
Existing required-name manifests remain enforced. The OpenCode SQLite builder
is copied identically into both temporary trees so builder changes cannot be
mistaken for parser changes. Each case directory is cleared before a capture, so
a `-wal`/`-shm` sidecar left by the first capture of a pass cannot be read with
the second capture's main file.

The base corpus is read with the current strict loader: any base whose corpus
uses only today's case fields loads, but removing a field -- or adding a required
one -- makes older bases fail the guard rather than the comparison, so that
change needs a guard-side migration. A newly registered harness without observations
fails; add it to the probe using an existing synthetic corpus.
Historical revisions without Pi do not simulate a Pi parser. When a corpus
predates Pi, registered Pi parsers use the candidate's first native corpus;
existing Pi corpus files and their required-name manifest remain authoritative.

The probe fixes filesystem/Git context and fallback source clocks, removes only
metadata serialization/producer bookkeeping, capture-time `ingested`, and their
derived hashes, and normalizes synthetic SQLite temporary paths. Extracted
timestamps, model, context, statistics, transcript materialization and entry
fields remain compared. Diagnostic error wording alone is not a behavior bump.

Compatibility bridges support the former global `CurrentIndexVersion` and
adapter baseline 1, and the materializer's old tuple versus captured-struct
return envelope. Every bridge invokes the actual parser at that revision.
Indexer selection follows ingestion: authoritative full capture first,
versioned result second, and the former `IndexTranscript` API last.
Other incompatible APIs or fixture schemas fail with a prerequisite
diagnostic; update the probe deliberately before claiming a comparison passed.

This is a corpus guard, not proof over every possible native input. Add newly
supported shapes to the existing named corpus so the old implementation is
actually exercised on those same bytes.
