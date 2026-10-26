# Native OpenCode profiling

Manual developer measurement, not a CI benchmark or parser test. Use a completed,
consistent SQLite **sandbox backup**, never a live source. The driver requires the
backup to lie under `-sandbox-root` and refuses a database that is itself the
recorded metadata source. The production source opens it restrictively read-only.
No store, redaction, publication, or source write is performed.

The driver confines discovery's filesystem probes and every SQLite source open
to the exact resolved backup path. Conventional sibling databases, aliases and
legacy directory trees are denied before source I/O, even when a sibling symlink
points outside the sandbox. The discovery filesystem also permits the approved
backup's optional regular WAL sidecar metadata for its freshness clock, but
denies sidecar symlinks and content reads. Every selected session must name
that exact approved source, not merely a matching SQLite layout. Keep the backup
and its parent directory stable for the run; this path check is not an OS-level
sandbox against concurrent filesystem replacement.

The manifest is the retained profiler's JSON array of `{"metadata":"..."}`
selections. Only metadata is loaded in advance; neither exported rows nor fixed
snapshots are accepted. Production discovery runs against only the backup before
measurement, and its selected SQLite authority must match each requested mode.
Merely having both tables does not establish that every session has current rows.

- The default `-mode=production` dispatches the selected session to native
  generation for current SQLite authority or materialization plus strict capture
  for legacy SQLite authority, matching the pipeline's format target boundary.
  The report names both layout counts. Discovery itself is outside timing.
- `-mode=fresh` measures the current native producer stages of a fresh harvest:
  `MaterializeTranscriptWithCursor` acquires actual metadata and transcript from
  the backup, then `BuildGeneration` consumes that metadata and acquires its real
  native snapshot. Both reads, normalization, fingerprinting, snapshot hashing,
  classification, generation content and prior evidence are timed. No strict
  retained parser is substituted for native generation. This is producer-stage
  evidence, not full harvest evidence: redaction, diff, pair installation, store
  activation, and Git lookups remain excluded.
- `-mode=generation` executes `OpenCodeIndexer.BuildGeneration`, with source access
  calling the real adapter's `SnapshotOpenCodeProvenance` on the backup **inside**
  the timed call. This includes source open, session evidence, SQL message pages,
  native decoding, snapshot hashing, close, classification, generation content,
  and prior evidence. The snapshot options are empty, as in ordinary pipeline
  wiring: the driver fabricates no fork, revert, or delivery proof. This mode is
  for current native sessions only, matching the production format-2 gate.
- `-mode=current` and `-mode=legacy` execute the adapter's actual
  `MaterializeTranscriptWithCursor` followed by strict retained capture. They
  include native SQL acquisition, normalization, metadata preparation, managed
  encoding, fingerprinting, and strict indexing. Git lookups are denied rather
  than traversing working directories from the copied source. These modes
  exclude redaction and store activation; neither is a full harvest benchmark.
  With `-mode=legacy` and no manifest, select every legacy SQLite authority from
  real discovery instead. No retained metadata is needed: actual materialization
  acquires it from the backup inside the measurement. This avoids forcing a
  migrated session back onto an obsolete legacy representation.

`buildSeconds` includes acquisition. `acquisitionSeconds` is its nested snapshot
or materialization portion, not additional time. Their difference measures the
remaining generation/capture work. Timing excludes metadata loading, output
hashing/encoding, and memory-stat sampling. Allocation deltas bracket only the
production calls. One serial worker processes one selected session at a time;
there is no row-size cap. Avoid giant sessions when available memory is limited.

Build identical driver source against both revisions, then run serialized:

```sh
go build -o /tmp/opencode/native-candidate ./scripts/profile-opencode-native
env GOMAXPROCS=1 GOGC=100 GOMEMLIMIT=off /tmp/opencode/native-candidate \
  -db "$BACKUP" -sandbox-root "$SANDBOX" -manifest "$MANIFEST" \
  -mode=production -phase=parity > /tmp/opencode/native-parity.json
# Repeat separately with -phase=timing, or:
# -phase=profile -cpuprofile /tmp/opencode/native.cpu
```

Parity hashes the entire result and exact typed error text without printing
private content or dependency errors. Deterministic refs make generation output
comparable. Compare every case's input/output hashes, and require honest success
and refusal counts. Matching refusals are not indexing success; any refusal makes
timing/profile exit unsuccessfully. Record the backup identity, chosen cohort,
source/driver revisions, Go version, exact commands, and three alternating timing
pairs. Use a separate CPU profile to distinguish SQL, decoding, normalization,
hashing, and generation costs. RSS from an external process observer includes
runtime and SQLite overhead; it is not the parser's allocated-byte counter.
