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

## Push Measurement Boundaries

- The CLI owns one `push.run` span across transcript and annotation work. Session
  stages are nested under `push.session`; early failures end their active stage
  as failed. A failed-open negotiation is skipped, not a successful handshake.
- Session subjects are stable SHA-256 tokens, not raw session identifiers.
  Transport request, byte, response and retry-decision events carry the same
  subject, so counters can be grouped per session as well as per operation.
- `push.db.reads` counts each invoked store read, including failed reads,
  preflight re-reads, per-provider queries, and annotation queries. It does not
  estimate SQL statements inside a store method. Writes are not reads.
- `push.http.requests` counts calls at the HTTP `RoundTripper` boundary, including
  redirects. `push.http.responses` counts returned responses by status class.
  These do not measure TCP packets or replay attempts internal to a transport.
- Payload bytes count request-body bytes consumed by the transport, including
  multipart framing. Response bytes count bytes consumed by the client decoder,
  subject to its existing read limits. Neither counter copies body content.
- The client has no application retry loop. `push.http.retries` is explicitly
  zero for observed requests. On remote failure, `push.retry` records a skipped
  retry decision; safe error retryability describes eligibility, not an attempt.
- Annotation requests include manifest GETs and batch POSTs, distinguished by
  the `operation` attribute. Visibility requests count owner visibility PATCHes.
- Published totals count only terminal successes; a dry-run forecast is not a
  publication. Selected sessions not reached after preflight or connection abort
  count as skipped, without changing the ordinary command result.
- Concurrency high water is the observed overlap of real session work. It can
  vary with scheduling. Tests use barriers, not time thresholds, to prove it.
- Payload construction includes whole-document redaction performed inside the
  existing builders. Coarse redaction spans isolate metadata and entry redaction;
  decorator metrics describe combined application calls separately. Inclusive
  parent and child durations must not be added as independent elapsed work.

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
