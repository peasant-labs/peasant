# Retained parser profiling

Internal developer command; no store, discovery, redaction, or native acquisition.
Use copied sandbox inputs only. All indexer filesystem operations are denied,
including fallback reads of paths in metadata. One serial worker loads one case
at a time outside each timing region. Profile runs preload the explicit corpus
before sampling, bounded by `-profile-max-bytes` (512 MiB of raw inputs by default).
This is an operator memory guard, not a performance target; decoded working memory
is additional. Use a smaller sample if it cannot fit comfortably in available RAM.

Manifest: a JSON array of `{"metadata":"/sandbox/metadata.json",
"transcript":"/sandbox/transcript.jsonl"}`. OpenCode additionally requires
`"origin":1` (legacy SQLite projection) or `2` (current SQLite projection), taken
from the real capture. Legacy directory sources are not supported.
The optional `"contentOmitted":true` carries the actual production composition
flag and participates in corpus identity. The production metadata-to-flag helper
is unexported; this driver does not duplicate its warning-code policy. When the
flag applies, set it explicitly in the sandbox manifest; do not infer completeness
from a successful parser result with the flag omitted.

Build **the identical driver source** against each revision:

```sh
go build -o /tmp/opencode/parser-baseline ./scripts/profile-index-parser
# Repeat against the candidate revision, producing parser-candidate.
/tmp/opencode/parser-baseline -manifest "$MANIFEST" -mode=generation -phase=parity > /tmp/opencode/baseline.json
/tmp/opencode/parser-candidate -manifest "$MANIFEST" -mode=generation -phase=parity -compare /tmp/opencode/baseline.json > /tmp/opencode/candidate.json
```

`generation` calls Claude's captured-file `GenerationBuilder` exit with deterministic
entry/submission refs. For OpenCode it requires an additional `"snapshot"` path
to a JSON-encoded, actually captured `ingest.OpenCodeHistorySnapshot`; that evidence
is injected through `OpenCodeSourceAccess`, never constructed from retained text.
The complete candidate digest includes `PriorEvidence`. The provided transcript
is still part of corpus identity but is not the source used by this builder.
The injected snapshot already contains decoded rows: this mode does not measure
`DecodeOpenCodeProvenanceRow`. Use `scripts/profile-opencode-rows` for that seam.

`capture` calls `IndexTranscriptBytesForCapture` and compares the entire
`TranscriptCaptureResult` (entries, ignored records, diagnostics, retained unknowns).
Use this for Cursor, Strike, and Pi strict capture measurements. Codex capture can
measure retained unknown preparation, but **not** native managed record replay.
OpenCode capture measures the retained projection decoder, semantic retention,
and nested tool-content preparation. It does **not** call `prepareOpenCodeCurrent`
or `DecodeOpenCodeProvenanceRow`, so it is not evidence for the native row wrapper
or native-generation optimization. Origin 2 must name an actual current SQLite
projection, not a legacy native JSON document relabeled as a projection.

`entries` calls the retained compatibility `IndexTranscriptBytes` exit
with full content/depth for Claude, Codex, Cursor, Strike, Pi, and OpenCode. These
are different workloads: retained capture/entries results do **not** prove native-generation
coverage. Codex native generation is unsupported: its authority resolution can
scan sibling rollouts, open native pointer storage, and resolve history dependencies.
Binding a retained artifact to one native pathname would not establish that source
authority; the supplied corpora do not include captured original source/dependency
evidence. Those paths need a separate actual-source fixture or sandbox measurement.
OpenCode generation requires the
explicit real snapshot; retained projection inputs alone do not cover native row
decoding. This command does not fabricate or acquire native evidence. No prior generation is
supplied; this measures a fresh build, not identity reuse on refresh.

Parity hashes every complete returned value (including raw blobs losslessly and
partial values on refusal), plus the exact input bytes. JSON map encoding is
canonical; array order, nil/empty differences, and blob bytes remain significant.
Errors are reduced to refusal status, never printed, because they can contain
transcript data. A matching refusal is not successful indexing: inspect per-harness
success/refusal and successful-byte counts before drawing conclusions.

Run timing separately, with no competing benchmarks. It excludes loading, hashing,
encoding, and memory-stat sampling; allocation deltas bracket only the build.
The counters are process-wide; use the serial standalone process as intended.

```sh
/tmp/opencode/parser-baseline -manifest "$MANIFEST" -mode=capture -phase=timing > /tmp/opencode/baseline-timing.json
/tmp/opencode/parser-candidate -manifest "$MANIFEST" -mode=capture -phase=timing > /tmp/opencode/candidate-timing.json
/tmp/opencode/parser-candidate -manifest "$MANIFEST" -mode=capture -phase=profile -cpuprofile /tmp/opencode/parser.cpu > /tmp/opencode/profile-summary.json
go tool pprof -top /tmp/opencode/parser-candidate /tmp/opencode/parser.cpu
```

Profile runs do not report timing/allocation measurements; profiling overhead is
not benchmark evidence. Inputs are preloaded before CPU sampling, and parity
hashing is disabled. Very small corpora may not produce enough CPU samples.
Keep profiles and manifests private, outside the repository. Record date, commit,
Go version, exact invocation, corpus digest, and repeated timings alongside results.
This is a manual measurement tool, not a CI performance threshold or parser test.
