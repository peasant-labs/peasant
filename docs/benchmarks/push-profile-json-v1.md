# Local Push Profile JSON v1

Peasant push profiling writes a local diagnostic JSON document. It is versioned
for repeatable tests and local evidence. It is not a schema-module wire
contract.

## Shape

Top-level keys:

| Key | Required | Content |
| --- | --- | --- |
| `formatVersion` | yes | Integer format version. The first version is `1`. |
| `producer` | yes | App, command, app version, and profile API version. |
| `run` | yes | Safe run metadata, timestamps, duration, subject, selection mode, session count, and concurrency limit. |
| `summaries` | yes | Stage distributions and top bottleneck hints. |
| `spans` | yes | Per-run and per-session spans with safe identifiers. |
| `counters` | yes | Structural counters with typed units and safe attributes. |
| `resources` | yes | DB read count, HTTP request count, retry count, byte counts, optional allocation bytes, and concurrency high water. |
| `redaction` | yes | Entries scanned, bytes scanned, finding counts by category, rule counts, and failure count. |
| `errors` | yes | Privacy-safe errors with code, stage, safe message, and retryability. |
| `traceFile` | no | Relative or file-name reference to an optional JSONL trace. |

Unavailable optional resource values use `null`. Do not write a misleading zero
when the measurement was not collected.

## Stage Summary Rules

Each stage summary includes:

- `stage`;
- `count`;
- `totalMs`;
- `minMs`;
- `maxMs`;
- `p50Ms`;
- `p95Ms`;
- `p99Ms`;
- `errors`;
- `outcomes`.

Tests should compare these values with an injected clock or fixed fixture data.
They must not require a hard wall-clock threshold.

## Redaction Measurement Boundary

Push attaches a run-scoped decorator to its existing `ingest.TextRedactor`.
It calls the same engine methods and retains the existing fail-closed document
validation. Disabled profiling does not wrap the engine or read its report.

| Measurement | Meaning and limits |
| --- | --- |
| `redaction.apply` | Combined engine-call duration. The `operation` is `metadata_scan_apply` or `json_scan_apply`. It includes detection and application, not replacement-only time. An `ok` outcome means the call returned; subsequent document validation can still refuse publication. |
| `entriesScanned` | Stored entries submitted to the entry-redaction boundary, once per session. It is not the count of strings or repeated copies in assembled documents. |
| `bytesScanned` | UTF-8 bytes in decoded JSON **string values** submitted to `RedactJSON`, across all passes. Keys, JSON syntax, numeric scalars and metadata-specific field normalization are excluded. Repeated passes count again. Failed document validation does not undo the submitted-byte count. |
| `rulesMatched` | Run delta of the engine's cumulative built-in rule match counts. These are not exact replacements: overlapping matches and matches that rewrite an existing placeholder can be counted. |
| `findingsByCategory` | The same match deltas grouped by the engine's validated rule categories. Labels come from `Category.String()`: `CREDENTIAL`, `PII`, `PATH`, `INTERNAL`. Existing raw-category tokens remain readable by the recorder for compatibility. |
| `failures` | Document-boundary and aggregate-report validation failures. Counter `operation` attributes distinguish `entries_validation`, `metadata_validation`, `transcript_validation`, and `report_validation`. A report validation failure withholds profile counts; it does not change the push result. A document validation failure still stops publication. |

The current seam does **not** expose separate scan-only, per-rule evaluation, or
replacement-only durations. Therefore push does not emit `redaction.scan`,
`redaction.rule.evaluate`, or `redaction.replacements`. It also cannot count
contextual metadata replacements or recover engine-internal fallback failures.
Those metrics need an upstream engine contract, not a second scan or a guess.

Rule/category counts use validated reports taken before the run and after its
concurrent sessions join. Earlier use of the engine is subtracted. Counts are
run-level, not attributed to individual sessions. Do not share the same engine
with unrelated concurrent runs: the cumulative report cannot separate them.

Only rule IDs from the engine's built-in catalogue can enter a profile. A custom
rule name can contain private content even when it looks like a safe token.
Unknown IDs (including custom and generated XDG rule IDs), unknown/inconsistent
categories, negative counts, inconsistent totals, or decreasing reports withhold
the whole run's finding/rule counts and produce a safe `report_validation`
failure. The engine still runs unchanged. Implementations without `Report()`
retain timing and volume metrics but emit a `report_unavailable` error and no
finding/rule counts. Empty maps in either case mean **unavailable**, not proof of
no findings; consult the errors and operation attributes.

Report match text and residue warnings are never forwarded, even through the
general error sanitizer. Safe errors use fixed local descriptions.

## Sorting Rules

Profile output must be deterministic:

- stages sort by stage identifier;
- spans sort by stage, safe subject identifier, then recorded order where needed;
- counters sort by counter name and safe attributes;
- error lists sort by stage, code, and safe subject where present.

## Safe Evidence Rules

Allowed data:

- closed stage identifiers;
- counter names;
- outcome tokens;
- units;
- safe run identifiers;
- safe session identifiers;
- byte counts;
- request counts;
- retry counts;
- category counts;
- rule identifiers;
- privacy-safe error codes and recovery text.

Forbidden data:

- transcript text;
- matched redaction text;
- raw local filesystem paths;
- raw git remotes;
- branch output;
- raw logs;
- annotation values;
- private project history;
- full inventories of redacted values.

If a field could contain unsafe data, the profile writer must drop it, replace it
with a safe code, or fail closed before writing the profile.

## Future Village Stage Names

Peasant reserves this naming shape for later Village-side profiling. These names
are not emitted by Peasant push profiling until Village adopts the same local
diagnostic shape.

| Reserved stage | Meaning |
| --- | --- |
| `village.publish.multipart_read` | Read the incoming publish body. |
| `village.publish.schema_validate` | Validate the published transcript shape. |
| `village.publish.preservation_scan` | Check preservation and safety rules. |
| `village.publish.hash` | Compute content hashes. |
| `village.publish.encrypt` | Encrypt content before storage. |
| `village.publish.object_upload` | Upload content to object storage. |
| `village.publish.db_transaction` | Persist registry state. |
| `village.publish.advisory_lock_wait` | Wait for per-transcript lock ownership. |
| `village.publish.response_build` | Build the HTTP response. |

## Fixture Families

Profile tests should use the shared fixture loader families in
`internal/testutil`:

- profile contract cases;
- village push cases;
- redaction cases;
- CLI cases.

Each family has a name-only manifest. Adding, removing, or renaming a case must
update the manifest by case name. Do not protect cases with a bare integer count.
