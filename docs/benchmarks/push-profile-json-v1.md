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
