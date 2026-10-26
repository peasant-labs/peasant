# Frozen content parity capture

The golden corpus in `internal/store/testdata/content_model_parity_goldens/`
was captured from production packages at
`ed9984369d8bafb4b5552123b9b4f56f93600569`, not from the harmonized writer.
The source YAML is the input; goldens are immutable observed outputs. Never
regenerate expectations from the current checkout to make a failure pass.

## Reproduce

From the feature checkout, with a **clean detached sandbox worktree** at that
revision and two nonexistent output directories:

```sh
export TMPDIR=/tmp/opencode
bash scripts/parity-oracle/capture.sh "$BASE_WORKTREE" "$TMPDIR/content-capture-one"
bash scripts/parity-oracle/capture.sh "$BASE_WORKTREE" "$TMPDIR/content-capture-two"
diff -r "$TMPDIR/content-capture-one" "$TMPDIR/content-capture-two"
diff -r internal/store/testdata/content_model_parity_goldens "$TMPDIR/content-capture-one"
nix develop -c go test ./internal/store/ -run '^TestContentModelParity$' -count=1 -v
nix develop -c go test ./internal/store/ -run '^TestContentModelParity(ByteMutation|FixtureManifest)$' -count=1 -v
```

The capture script checks the old revision and clean worktree, copies only the
scenario driver and capture command into it, builds the old CLI, and runs a Go
capture executable linked to **the old production libraries**. The CLI is a
build-provenance check; its commands are not used to approximate internal
surfaces. The capture executable calls the actual writers, readers, detail,
export, and publication builders. No production file in the base is patched.
The copied driver, command, and binary are removed on ordinary exits/signals;
after SIGKILL remove only those two copied directories before retrying. Never
commit in the base worktree. The script refuses an existing output directory;
a partial capture remains inspectable but is not a complete corpus.

## Inputs and surfaces

All identifiers, metadata, times, source paths, tool bytes, and ref ordering are
pinned synthetic fixture inputs. Each case has a `why` explaining its boundary.
Native Codex, Pi, and simple OpenCode sources run the production parsers;
targeted entry projections model storage-boundary cases the normal parsers do
not emit. These projections are declared in YAML, not inferred from goldens.
The tool case explicitly seeds a call/result split with unique refs. The
file-backed case alone seeds old catalog rows and immutable blobs to exercise
the retained reader; it does not substitute a test implementation of the new
writer. All other cases use production activation or Pi's unchanged V1 writer.

Each `bytes` field is base64 encoding of the **exact surface bytes**. Detail,
export, publish transcript content and preview use the production serialized
bytes directly. `publishEntries` is the raw `entries` JSON member emitted by
`push.MapMetadata` (the `PublishRequest` builder). Routing entries, range,
max index, first entry, first user/bulk/leading user, metric input/hash and
generation partitions use `json.Marshal` on production return values.
`sessionEntriesHash` and `fullCaptureHash` are read from SQLite, retaining SQL
NULL separately from empty string. The loader requires every surface key;
no missing key silently decodes as absence. Comparison never normalizes payload
JSON, strips refs, replaces tool previews, or adjusts hashes.

The preview-only case freezes full-detail and export `ErrSnapshotIncomplete`,
publication `ErrMetadataMissing`, their exact messages, and SQL NULL for the
full-capture hash. Preview and routing remain observable; no successful full
payload is invented. The omitted-placeholder and retained-unknown scenarios
also freeze the base's observed publication refusal rather than override its
readiness policy. Pi has no managed generation: generation-read refusal is
frozen, while detail uses the production API provider's V1 projection path.
Every refusal is recorded with `errors.Is` sentinel identity where provided
and the complete, fixture-deterministic message. No volatile content is scrubbed.

## Test scope and lifetime

This integration fixture protects serialization and hash compatibility across
a writer replacement. A unit test cannot independently observe persisted
authority, metadata seeds, blob reads and the publication boundary together.
The driver does not copy reader/writer logic; only old persistence seeding for
the dual-read scenario is explicit. Each scenario owns a new temporary DB and
artifact directory, closes resources, and removes them on normal exits. Parallel
processes never share databases. SIGKILL may leave an isolated temp directory,
never a live store. Tests spawn no subprocesses; only manual capture tooling
compiles the pinned old build. The comparator runs in about two seconds; the
full store suite takes roughly a minute on the capture host. A clean checkout
needs no old worktree to run the comparator, only for deliberate reproduction.

The one-byte mutation test changes a frozen detail byte and verifies rejection.
Independent repeat capture proves determinism, not parity with the new writer.
Retain the oracle through rollout; simplify it only when the old representation
is deliberately retired and the retained compatibility contract is reviewed.

## Capture evidence

Captured 2026-10-08 using the pinned Nix shell (Go 1.26.5). Two complete capture
runs were byte-identical. Mutation and required-name checks passed. The current
writer's comparator remains red: it detects publication refs, metric seed/hash
changes, routing extra serialization, long-preview hash differences, and empty
non-emitted blob rejection. These are production findings, not reasons to update
the frozen corpus. Pi V1 and the explicit file-backed reader cases match.
