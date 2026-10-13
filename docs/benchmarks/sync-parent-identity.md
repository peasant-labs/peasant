# Sync parent identity read measurement

Measured on 2026-10-12 at revision
`8b2727a694b2855ddc3ed1fe673bfbfa6b4bf961`, using the pinned Nix development
shell (Go 1.26.5). The chosen production shape is the **channel fallback**:
an indexed snapshot-parent scan, sparse evidence reads in batches of 256 IDs,
and a raw-row merge consuming the sync list's existing pushable rows. Push
readers and their row type are unchanged.

## Corpus and command

Read-only sandbox: `/tmp/opencode/peasant-sandbox/peasant.db`.

- Size: 43,106,304,000 bytes.
- Modification time: `2026-10-10T22:29:15.3630256Z`.
- Sessions and pushable rows: 19,763 each.
- Relationship evidence rows, including retained generations: 952.
- Publication metadata rows: 19,425; snapshot parents: 16,640.

```sh
PEASANT_SANDBOX_DB=/tmp/opencode/peasant-sandbox/peasant.db \
  nix develop --command go test -race ./internal/store \
  -run '^TestSandboxSyncParentIdentities$' -count=1 -v -timeout=0
```

The sandbox schema is 62, whereas the normal store opener requires 65. The
measurement opens a single-connection `sqlite.OpenReadOnly` pool without
migrations, then executes the actual production base and channel reader methods.
Both readers use columns already present in this corpus. This is a read-query
measurement, not an open/migration test. No database, schema, artifact, or live
store was changed. The test resolves symlinks and refuses paths outside
`/tmp/opencode`; it skips unless the sandbox environment variable is supplied.

## Results

Thirty warm samples per shape, with fetch and read-side materialization included.
Comparison and sorting are outside the timed regions. The channel time includes
snapshot scanning, all evidence batches, and the raw-row merge, but not the
already-required base read. The API's subsequent identity-map construction is
common to both shapes and is outside this read comparison. All raw identities
and classifications agree between the channel and one-shot shapes on this corpus.

| Read shape | p50 | p95 |
| --- | ---: | ---: |
| Existing base pushable read | 2.942369477 s | 3.262858649 s |
| Added one-shot, two evidence joins plus snapshot PK join | 3.301430567 s | 3.646778407 s |
| Added production channel fallback | 1.245232816 s | 1.412565981 s |

Budget: added p50 ≤ 0.75 × base p50, and added p95 ≤ 2 × added p50.
The channel shape passes: p50 is 0.4232 × base p50 (limit 2.206777108 s),
and p95 is 1.1344 × added p50 (limit 2.490465632 s). The one-shot shape
misses the p50 budget (1.1220 × base p50), so it is retained only as the
local measurement comparator, not as another production path.

The budget derives from the earlier same-corpus shape study: channels were
0.46 × base and one-shot 0.65 × base; the 0.75 bound admits those shapes
with cross-driver variation but rejects a second full-materialization cost.
These race-instrumented Go numbers are not comparable to that Python study's
absolute wall times. Race instrumentation substantially amplifies the Go SQLite
VM cost; another full test run was active during the final measurement, so these
numbers are local shape-decision context, not isolated release-latency claims
or a timing CI gate. A prior warm race run also rejected the one-shot shape.

The channel reader preserves raw evidence-query results, not a per-session
evidence map. The inactive-generation fixture therefore still detects fan-out:
removing the active-generation bound from the started-by join produces two raw
rows and fails the single-row assertion before the API builds its map. Snapshot
and cache values are consulted only when both active evidence arms are silent;
snapshot precedes cache. The fixture with differing snapshot and cache parents
protects this order independently of the measured corpus.
