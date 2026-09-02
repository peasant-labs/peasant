# Benchmark Procedures

This page gives local-only procedures for repeatable profiling evidence. Use
copied or synthetic data. Do not run these procedures against the live Peasant
data directory.

## Village Push Profile Evidence

Use `scripts/profile-push-copy.sh` to prepare repeatable `peasant village push`
profile evidence when push profiling is enabled. The harness is only a safe
runner. It does not choose the profile command for you, and it does not compare
runs by a hard maximum duration.

### Goals

- Keep profile outputs under `/tmp/opencode`.
- Keep the work directory under `/tmp/opencode/peasant-push-profile-*`.
- Record whether JSON and JSONL files were created.
- Compare structural metrics: keys, stage names, counter names, outcomes, safe
  subject identifiers, and forbidden-string absence.
- Treat wall time as context, not as a pass or fail threshold.

### Dry Run

The dry run validates paths and writes a safe summary without running a push:

```bash
scripts/profile-push-copy.sh \
  --dry-run \
  --work /tmp/opencode/peasant-push-profile-dry-run \
  --profile-output /tmp/opencode/push-profile.json \
  --trace-output /tmp/opencode/push-profile.jsonl \
  --summary-output /tmp/opencode/push-profile.summary.log
```

Expected output:

```text
push profile dry run: ok
profile summary: /tmp/opencode/push-profile.summary.log
```

### Real Run Shape

When the CLI profiling flags are available, pass the exact command to the
harness after `--`. The harness exports these variables for the command:

| Variable | Meaning |
| --- | --- |
| `PROFILE_WORK` | Local-only work directory. |
| `PROFILE_JSON` | JSON v1 profile output file. |
| `PROFILE_TRACE` | Optional JSONL trace output file. |
| `PROFILE_SUMMARY` | Summary file written by the harness. |

Example shape:

```bash
scripts/profile-push-copy.sh \
  --work /tmp/opencode/peasant-push-profile-candidate \
  --profile-output /tmp/opencode/push-profile.json \
  --trace-output /tmp/opencode/push-profile.jsonl \
  --summary-output /tmp/opencode/push-profile.summary.log \
  -- bash -c 'go run ./cmd/peasant --data-dir "$PROFILE_WORK/data-home" --config-dir "$PROFILE_WORK/config-home" --state-dir "$PROFILE_WORK/state-home" village push --profile-output "$PROFILE_JSON" --profile-trace "$PROFILE_TRACE"'
```

### Public-Safe Push Evidence

When you summarize a run, include only:

- commit under test;
- command shape, without local data paths other than `/tmp/opencode` files;
- profile status;
- whether JSON and JSONL files were created;
- counts for selected, published, failed, and skipped sessions;
- stage totals and distribution keys from injected or collected profile data;
- request, retry, payload, response, and receipt counters;
- redaction counts by category and rule identifier;
- safe error codes and safe recovery text.

Do not include transcript text, matched redaction text, raw local paths, raw git
remotes, branch output, raw logs, annotation values, or private project history.

### Comparison Rules

- Compare two runs only when they use the same command shape, input shape, and
  profile fields.
- Do not mark a run better only because wall time is lower. First confirm it
  published the same sessions and did not fail earlier.
- Prefer structural checks over duration limits: required stage names, required
  counters, stable ordering, safe subject identifiers, and forbidden-string
  absence.
- Keep raw profile files local. Public evidence should quote only aggregate,
  privacy-safe fields.

## Harvest Benchmark Procedure

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
```

Run the checked-in harness. It creates a small control directory and points the
CLI at the copied corpus through an XDG data-home symlink. It does not copy the
corpus.

```bash
STATUS=0
scripts/profile-index-copy.sh --corpus "$CORPUS" --work "$WORK" --summary-output "$SUMMARY" || STATUS=$?
printf '\nscript exit status: %d\nsummary log: %s\n' "$STATUS" "$SUMMARY" >>"$SUMMARY"
```

`STATUS=1` can be expected on copied corpora with known dirty-data warnings. Do
not discard the run for that reason. Instead, record the warning classes and the
number of successfully indexed sessions.

## Extract Public-Safe Evidence

Use the harness summary as the main source. It writes profile lines, warning
counts, corpus path, control directory, wall seconds, and the CLI profile status.

When you create a public PR or issue comment, include:

- commit under test;
- branch name;
- corpus type;
- script status and whether that status is expected;
- outer wall time and CLI wall time;
- session count, entry count, and byte count;
- stage timings and INDEX write-cause details, including target-repair timings
  when repair work runs;
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
queue behind one SQLite writer. The buffered-writer candidate reduces write
pressure with a stage-level buffer that collects prepared annotations from many
sessions and flushes them through larger serial write batches. Flushes are
bounded by batch size and by a `500ms` interval so progress remains visible on
small or slow batches.
