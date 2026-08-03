# Agent memory (`peasant memory`)

> **Status: EXPERIMENTAL — excluded from the default build.**
> The `peasant memory` command group is gated behind the `experimental` Go build
> tag. The binary produced by `make build` (and the published binaries) does
> **not** include the memory implementation: the `memory` command is hidden from
> `peasant --help`, and invoking it returns an actionable error:
>
> ```
> $ peasant memory
> Error: peasant memory is experimental; rebuild with -tags=experimental
> ```
>
> To use it, rebuild with the tag:
>
> ```bash
> go build -tags=experimental -o bin/peasant ./cmd/peasant
> ```
>
> See [Build-tag gating](#build-tag-gating) below for why and how this works.

The agent memory system extracts generalized **lessons** from annotated friction
episodes and retrieves them at coding-agent prompt time, so an agent is reminded
of past failures before it repeats them. It is the downstream consumer of the
annotation data managed by the commands in the [CLI reference](cli/peasant_annotate.md).

## Pipeline

```
Annotated friction episodes
        │  (LLM lesson-extraction pass → JSONL)
        ▼
peasant memory build --from-file lessons.jsonl
        │  import into the `lessons` table (SQLite, schema v28)
        ▼
OpenAI text-embedding-3-small  ──►  situation_embedding (BLOB per lesson)
        │
        ▼
User prompt ──► cosine-similarity retrieval (top-K above threshold)
        │
        ▼
Top lessons prepended to the agent's context (UserPromptSubmit hook)
```

A lesson follows the ExpeL shape — `[topic] rule + failure_mode`:

```jsonl
{"annotation_id":"<uuid>","session_id":"<uuid>","topic":"dependencies/compatibility","rule":"When adding deps, check Python version compatibility.","failure_mode":"torch had no 3.13 wheel."}
```

Lessons are typically produced by an LLM pass over imported annotations (see the
`orchestrate-lesson-extraction` skill). `memory build` consumes that JSONL.

## Command tree

| Command | Purpose |
|---------|---------|
| `memory build` | Import lessons from a JSONL file **and** compute embeddings |
| `memory embed` | Retry embedding for lessons whose `situation_embedding` is NULL |
| `memory inject on` | Install the `UserPromptSubmit` retrieval hook in a project |
| `memory inject off` | Remove the retrieval hook from a project |
| `memory retrieve` | Find relevant lessons for a prompt (used by the hook) |
| `memory augment` | Emit `lessons + prompt` on stdout (for benchmark piping) |
| `memory eval` | Compare friction rates between baseline and treatment periods |
| `memory list` | List stored lessons |

### `memory build`

```bash
peasant memory build --from-file ./annotation-batch/lessons.jsonl
```

Imports each JSONL line into the `lessons` table and computes an embedding via
OpenAI `text-embedding-3-small`. Requires `OPENAI_API_KEY`.

- `--from-file <path>` — JSONL file with extracted lessons
- `--model <id>` — OpenAI embedding model (default `text-embedding-3-small`)

### `memory embed`

```bash
peasant memory embed          # prompts for confirmation
peasant memory embed -y       # skip confirmation
```

Recovery command: re-embeds lessons left with a NULL embedding when a prior
`memory build` failed partway (missing API key, network error, quota). Requires
`OPENAI_API_KEY`.

- `-y, --yes` — skip the interactive confirmation
- `--model <id>` — OpenAI embedding model (default `text-embedding-3-small`)

### `memory inject on` / `memory inject off`

```bash
peasant memory inject on  --dir /path/to/project   # start injecting lessons
peasant memory inject off --dir /path/to/project   # stop injecting lessons
```

`inject on` writes a `UserPromptSubmit` hook into the project's
`.claude/settings.local.json`. The hook runs `peasant memory retrieve` (using the
absolute path of the current binary) on every message, with a 15s timeout, so
relevant lessons are prepended to the agent's context. `inject off` removes that
hook entry. Both default `--dir` to the current directory.

> **Note:** because the hook calls `peasant memory retrieve`, the binary on the
> hook path must itself be an `-tags=experimental` build — otherwise the hook
> hits the experimental-gate error.

### `memory retrieve`

```bash
peasant memory retrieve --prompt "Fix the failing parser test"
echo "Fix the failing parser test" | peasant memory retrieve   # stdin, for hooks
```

Returns the most relevant lessons for the prompt, formatted for prepending to
agent context.

- `--prompt <text>` — prompt text (falls back to stdin when omitted)
- `--max <n>` — maximum lessons to return (default 3)
- `--min-similarity <f>` — minimum cosine similarity threshold (default 0.3)
- `--output-json` — structured JSON output

**Retrieval behavior:** returns up to `--max` lessons above the
`--min-similarity` threshold, fewer if nothing else is relevant, and stays silent
(no output) when nothing matches — it never forces lessons into context.

### `memory augment`

```bash
peasant memory augment --prompt "Fix the bug in parser.py" | benchmark-runner
```

Like `retrieve`, but emits the original prompt with lessons prepended — designed
for piping into benchmark harnesses (e.g. SWE-bench). Shares `--max` and
`--min-similarity` with `retrieve`.

### `memory eval`

```bash
peasant memory eval --cutoff 2026-04-10            # split by date
peasant memory eval --by-injection                 # split by injection status
peasant memory eval --cutoff 2026-04-10 --output-json
```

Compares friction-episode rates between a baseline and a treatment group.

- `--cutoff YYYY-MM-DD` — split by date (before = baseline, after = treatment)
- `--by-injection` — split by whether lesson injection was active (uses the
  `inject on/off` log)
- `--annotator <prefix>` — annotator-name prefix to filter by (default `llm-judge`)
- `--output-json` — structured JSON output

### `memory list`

```bash
peasant memory list
peasant memory list --session <session-id>
```

Lists stored lessons, optionally filtered by source session.

## Storage

Lessons live in the `lessons` table created by database migration **v28**
(`internal/store/schema_v28.go`), including the `situation_embedding` BLOB column
and indexes on `session_id` and `episode_annotation_id`. The store layer
(`internal/store/lessons.go`) provides lesson CRUD, embedding storage, and cosine
similarity, and is **ungated** — it stays in the default build because
`peasant export` (injection-window reporting) depends on it. Only the `memory`
CLI surface and the `internal/memory` package (embedder + retrieval) are gated.

## Build-tag gating

The experimental shelving uses a **paired-file seam** so the static command
registry in `cmd/peasant/main.go` never changes:

| File | Build tag | Provides |
|------|-----------|----------|
| `cmd/peasant/cmd_memory.go` | `//go:build experimental` | full `BuildMemoryCommand()` with all subcommands |
| `cmd/peasant/cmd_memory_stub.go` | `//go:build !experimental` | hidden stub `BuildMemoryCommand()` that errors |
| `internal/memory/embedder.go`, `retrieve.go`, `retrieve_test.go` | `//go:build experimental` | `Embedder` + retrieval logic |

Exactly one definition of `BuildMemoryCommand()` compiles per build, so
`main.go`'s `commands = [...]` array is identical in both builds — no conditional
`append()` restructuring.

```bash
# Default build: memory hidden + errors on invocation
go build ./cmd/peasant

# Experimental build: full memory command tree
go build -tags=experimental ./cmd/peasant

# Tests
go test -race ./...                                              # default
go test -race -tags=experimental ./internal/memory/... ./internal/store/...
```

Because the command group is gated, `peasant docgen` on the **default** build
does not emit per-subcommand pages for `memory` under [`docs/cli/`](cli/) — that
is expected. This document is the hand-written reference for the experimental
surface; do not force the gated subcommands back into the generated CLI docs.

## See also

- [Annotation CLI reference](cli/peasant_annotate.md) — commands that create and manage source
  annotations
- [`docs/bestiary.md`](bestiary.md) — harness/provider type model (style reference)
- `internal/store/lessons.go`, `internal/memory/` — implementation
