# Profiling: procedures and results

This file condenses the profiling practice for this repository. The full pages are in
`docs/benchmarks/`: `procedure.md` (the repeatable procedures), `harvest-optimizations.md` (the
INDEX handoff), and `push-profile-json-v1.md` (the push profile shape).

## Hard rules

- Use copied or synthetic data only. Do not profile against `~/.local/share/peasant`.
- Keep the corpus stable across runs. Do not compare results across corpus types.
- Keep logs and outputs in a local scratch directory outside the repository (for example `$TMPDIR`). Do not commit them.
- Record the revision, the branch, and the exact command for every run.
- Keep raw profile files local. Public evidence quotes only aggregate, privacy-safe fields.
- Do not paste transcript text, raw paths, git remotes, annotation values, or redacted values into public evidence.

## Procedure (condensed)

1. Record the state of the worktree:

   ```bash
   git rev-parse --short HEAD
   git branch --show-current
   git status -sb
   ```

2. Pick one corpus:
   - Warm copied corpus: reindex a copied store with annotations and warm hashes.
   - No-annotations copied corpus: measure annotation creation from a scrubbed copy.

3. Run the checked-in harness for the surface:
   - Harvest INDEX: `scripts/profile-index-copy.sh --corpus "$CORPUS" --work "$WORK" --summary-output "$SUMMARY"`.
   - Village push: `scripts/profile-push-copy.sh` with the exported `PROFILE_*` destinations.

4. Read the summary: profile lines, warning counts, wall seconds, and the CLI profile status.
5. Clean up a mutable corpus after the run. The no-annotations scrub is in `docs/benchmarks/procedure.md`.
6. Compare two runs only with the same corpus, harness, command, and warning classes.

## Comparison rules

- Do not mark a run better only because the wall time is lower. First confirm it did the same work.
- Prefer structural checks: required stage names, required counters, and stable ordering.
- Stage timings can overlap. They do not sum to the wall time.
- Aggregate worker timers can be larger than the wall time. Aggregate mutex wait is a contention signal, not elapsed runtime.
- A known dirty-corpus warning is not a branch failure. Record the warning classes.

## Public-safe result template

```markdown
## Profile Evidence

- Commit: `<short sha>` · Branch: `<branch>` · Corpus: `<warm | no-annotations>`
- Script status: `<status>` (`expected` or `unexpected`)
- Outer wall time: `<duration>` · CLI wall time: `<duration>`
- Sessions: `<count>` · Entries: `<count>` · Bytes: `<count>`

## Stage Timings

- `DB INSERT`: `<duration>` · `INDEX`: `<duration>` · `COMPUTE`: `<duration>` · `ANNOTATE`: `<duration>`

## Hot Detail

- `<metric>`: `<duration or count>`

## Interpretation

- What the profile proves.
- What it does not prove.
- The next bottleneck or the next experiment.

## Cleanup

- `<cleanup status>`
```

## Results so far

Measured 2026-08 to 2026-10. Re-measure before you rely on a number.

- The INDEX lane: a warm harvest of about 9,600 sessions took about 40 minutes. Four stalls of 20 to 26 seconds held about 80% of the wall time. The single serialized write lane commits one giant session at a time.
- After the INDEX fixes, ANNOTATE dominated the no-annotations profile: classifier run about 15 s aggregate; batch persistence about 1 h 25 m aggregate; batch mutex wait about 1 h 20 m aggregate. The workers ran concurrently and queued behind one SQLite writer.
- The wins: no per-blob fsyncs in staging (#554); prepared statements for durable full-content inserts (#556: −18% on the write lane, allocations halved); prepared statements for the V2 install (#558: −12 to −14% wall time, about 50% fewer allocations); reclaim (#559).
- The store after reclaim: database 42.9 GB; owned tree 52.2 GB on disk; 7,506 generations; 2,044,440 content blobs; 7,491 native sessions.

## Interpretation guide

- State what a profile proves and what it does not prove. The INDEX profile bounded the stall to entry writes. It did not split statement preparation, row inserts, FTS tokenization, and WAL growth.
- The next bottleneck is the interesting number after a fix. Name it.
