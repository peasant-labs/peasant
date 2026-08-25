# Session-origin real-store audit

This manually invoked, read-only harness checks the real-store measurement question: does
`sessionorigin.Classify` -- the one production classification rule -- produce the signal
distribution the implementation plan predicted, when it is run over a real person's own harness
history?

It walks the operator's own Claude Code projects directory through the same production discovery
path Peasant's ingest pipeline uses (`ingest.NewClaudeAdapter(...).Discover`), and tallies how many
sessions landed on each deciding signal (`sessionorigin.Signal`) and origin (`sessionorigin.Origin`).
It contains no second copy of the classification rule or the transcript parser -- both are read from
`internal/sessionorigin` and `internal/ingest` as-is.

The harness is guarded by the `origin_audit` build tag. It is not compiled or run by
`go test ./...`, `make check`, or a production Peasant build, on the same footing as
`cmd/peasant-guided-screenshots`.

## Read-only, by construction

This command never attaches an evidence cache to the adapter it builds (`ClaudeAdapter.evidence`
stays `nil`), so `Discover` takes the no-cache branch of its own logic and never calls
`ClaudeEvidenceCache.SaveClaudeEvidence` -- see `ClaudeAdapter.saveMinedEvidence` in
`internal/ingest/claude.go`. It never calls `ExtractMetadata`. It writes no file, no cache record,
and no database row anywhere. It reads only the transcripts under `--source-path`.

## Run

```bash
make origin-audit
```

which runs `go run -tags=origin_audit ./cmd/peasant-origin-audit` against the operator's own
`~/.claude/projects`. Pass `--source-path` to point it at another directory:

```bash
go run -tags=origin_audit ./cmd/peasant-origin-audit --source-path ~/some/other/claude/projects
```

Run the baseline test with:

```bash
make origin-audit-test
```

## Output

Counts per deciding signal, counts per origin, the total number of `.jsonl` files examined, and two
kinds of gap made explicit rather than silently dropped: transcripts this pass could not read (it
still counts them, per `Discover`'s own fail-open behavior, as `unknown`/`no-evidence` -- this list is
how a read failure never gets mistaken for a genuine absence of evidence), and transcripts that were
examined but produced no session at all (an unrecognised filename, or a transcript with no
conversation record).

The reference tier counts this gate compares against are recorded in the Beads acceptance record for
this epoch, not in this repository: they describe one person's machine, and are private-history-
derived.

## Baseline

`testdata/baseline/` is a small, hand-counted fixture tree with one shape per deciding signal --
a structured-identity root, a programmatic-launch root, a command-wrapper root, a
caveat-prefixed command-wrapper root (the shape a locally run slash command takes on disk, where
the harness writes a caveat record in front of the command), a bootstrap-text
root, a plain-prose root with no evidence, a root/subagent pair (proving `parent-linked`), and a root
made unreadable by the test itself -- plus `manifest.yaml` (decoded through the shared
`internal/testutil.SemanticManifest` helper) as the deletion guard over the case names. `TestRunAuditBaseline`
asserts every count in the report against this hand-counted fixture EXACTLY, not merely non-zero, so
a harness that silently reported all zeros -- or any other wrong distribution -- fails it.
