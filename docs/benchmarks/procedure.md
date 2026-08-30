# Harvest Benchmark Procedure

This procedure describes how to run repeatable `peasant harvest index` profiles
against copied data. It is for local performance work only. Do not run it against
the live Peasant data directory.

## Goals

- Measure one branch or commit at a time.
- Keep the input corpus stable across runs.
- Keep logs outside the copied corpus.
- Record enough setup data to compare runs safely.
- Separate expected dirty-corpus warnings from branch-caused failures.

## Safety Rules

- Do not read or mutate `~/.local/share/peasant`.
- Do not create a new live-data copy for each run.
- Do not delete the source corpus.
- Do not store raw profile logs in committed files, issues, or PR comments.
- Do not paste high-cardinality values, local file paths, transcript text, or raw
  annotation values into public evidence.
- Keep raw logs under `/tmp/opencode` or another local-only directory.

## Corpus Types

Use one of these copied corpus shapes. Do not compare results across corpus types
as if they are the same workload.

| Corpus | Purpose | Path |
| --- | --- | --- |
| Warm copied corpus | Reindex an existing copied store, including existing annotations and warm hashes | `/tmp/opencode/peasant-index-profile-live-source` |
| No-annotations copied corpus | Measure annotation creation from a copied store with annotation rows removed | `/tmp/opencode/peasant-index-profile-no-annotations-source` |

The no-annotations corpus must start clean:

```bash
nix shell nixpkgs#sqlite -c sqlite3 "/tmp/opencode/peasant-index-profile-no-annotations-source/peasant.db" "select 'annotations', count(*) from annotations union all select 'annotation_run_state', count(*) from annotation_run_state; pragma integrity_check;"
```

Expected output before a no-annotations run:

```text
annotations|0
annotation_run_state|0
ok
```

## Branch Setup

Run profiles from the worktree for the exact branch or commit you want to test.
Record these values before each run:

```bash
git rev-parse --short HEAD
git branch --show-current
git status -sb
```

The worktree should be clean, or the report must state that the run used
uncommitted changes.

## Run The Profile

Set a corpus and run-specific output names:

```bash
CORPUS="/tmp/opencode/peasant-index-profile-no-annotations-source"
WORK="/tmp/opencode/peasant-index-profile-control-no-annotations-candidate"
SUMMARY="/tmp/opencode/peasant-no-annotations-candidate.summary.log"
RAW="/tmp/opencode/peasant-no-annotations-candidate.raw.log"
```

Run the checked-in harness. It creates a small control directory and points the
CLI at the copied corpus through an XDG data-home symlink. It does not copy the
corpus.

```bash
STATUS=0
scripts/profile-index-copy.sh --corpus "$CORPUS" --work "$WORK" >"$SUMMARY" 2>&1 || STATUS=$?
if [ -f "$WORK/profile.log" ]; then
  mv "$WORK/profile.log" "$RAW"
fi
printf '\nscript exit status: %d\nraw profile log: %s\nsummary log: %s\n' "$STATUS" "$RAW" "$SUMMARY" >>"$SUMMARY"
```

`STATUS=1` can be expected on copied corpora with known dirty-data warnings. Do
not discard the run for that reason. Instead, record the warning classes and the
number of successfully indexed sessions.

## Extract Public-Safe Evidence

Use the harness summary as the main source. It prints profile lines, warning
counts, corpus path, control directory, wall seconds, and the CLI profile status.

When you create a public PR or issue comment, include:

- commit under test;
- branch name;
- corpus type;
- script status and whether that status is expected;
- outer wall time and CLI wall time;
- session count, entry count, and byte count;
- stage timings;
- warning counts;
- known cleanup state for mutable copied corpora;
- interpretation of the next bottleneck.

Do not include:

- raw profile log content;
- local source paths;
- raw annotation values;
- transcript text;
- full redacted-value inventories.

## No-Annotations Cleanup

The no-annotations corpus is reusable only if it is scrubbed after every run. Run
this cleanup only when the corpus started with zero annotations before the
profile. It deletes the classifier annotations produced by the run and clears the
matching run-state rows.

```bash
nix shell nixpkgs#sqlite -c sqlite3 "/tmp/opencode/peasant-index-profile-no-annotations-source/peasant.db" "PRAGMA foreign_keys=OFF; BEGIN; DELETE FROM annotation_target_entries; DELETE FROM annotation_target_sessions; DELETE FROM annotation_target_associations; DELETE FROM annotation_target_annotations; DELETE FROM annotation_target_projects; DELETE FROM annotations WHERE annotator_id IN (SELECT id FROM annotators WHERE name IN ('outcome-classifier','frustration-classifier','scope-classifier','frustration-signal-classifier','resolution-evidence-classifier')); DELETE FROM annotation_run_state; COMMIT; PRAGMA foreign_keys=ON; select 'annotations', count(*) from annotations union all select 'annotation_run_state', count(*) from annotation_run_state union all select 'annotation_target_sessions', count(*) from annotation_target_sessions union all select 'annotation_target_entries', count(*) from annotation_target_entries; pragma foreign_key_check; pragma integrity_check;"
```

Expected output after cleanup:

```text
annotations|0
annotation_run_state|0
annotation_target_sessions|0
annotation_target_entries|0
ok
```

If cleanup times out, check the row counts before retrying. SQLite should roll
back an incomplete transaction. Do not delete or recreate the corpus unless the
integrity check fails or generated files have made the corpus invalid for the
next run.

## Comparison Rules

- Compare warm-corpus runs only to other warm-corpus runs.
- Compare no-annotations runs only to other no-annotations runs.
- Compare branch A and branch B only when they use the same corpus, same harness,
  same command, and similar warning classes.
- Do not treat shorter wall time as a win if the candidate indexed fewer sessions
  or failed earlier.
- Remember that stage timings can overlap and do not sum to wall time.
- Remember that aggregate worker timers can be much larger than wall time.
  Aggregate mutex wait is a contention signal, not elapsed runtime.

## Result Template

Use this structure for durable local summaries and public-safe PR comments:

```markdown
## Profile Evidence

- Commit: `<short sha>`.
- Branch: `<branch>`.
- Corpus: `<warm copied corpus | no-annotations copied corpus>`.
- Script status: `<status>` (`expected` or `unexpected`).
- Outer wall time: `<duration>`.
- CLI wall time: `<duration>`.
- Sessions: `<count>`.
- Entries: `<count>`.
- Bytes: `<count>`.

## Stage Timings

- `DB INSERT`: `<duration>`.
- `INDEX`: `<duration>`.
- `COMPUTE`: `<duration>`.
- `ANNOTATE`: `<duration>`.

## Hot Detail

- `<metric>`: `<duration/count>`.
- `<metric>`: `<duration/count>`.

## Interpretation

- `<what the profile proves>`.
- `<what it does not prove>`.
- `<next bottleneck or next experiment>`.

## Cleanup

- `<cleanup status>`.
```

## Current Interpretation Guide

On the current no-annotations profile, `ANNOTATE` is dominated by annotation
persistence. Classifier compute is small compared with write contention:

- classifier run: about `15s` aggregate;
- batch persistence: about `1h25m` aggregate;
- batch mutex wait: about `1h20m` aggregate.

This means session annotation workers are already running concurrently, but they
queue behind one SQLite writer. The next experiment should reduce write pressure
with a stage-level buffer that collects prepared annotations from many sessions
and flushes them through larger serial write batches.
