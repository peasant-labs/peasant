# Bestiary integration

[`bestiary`](https://github.com/peasant-labs/bestiary) is the external Go module that owns
the canonical registry of AI **coding tools / harnesses** (Claude Code, Gemini CLI, Codex,
OpenCode, Cursor, Antigravity), model **providers** (Anthropic, OpenAI, Google), and model
reference data. Peasant depends on it so that harness/provider/model identifiers have a
single source of truth shared across tools, rather than peasant maintaining its own.

## Harness — the central type

`bestiary.Harness` is a `type Harness string` enum identifying the coding tool driving a
session. Peasant re-exports it as a **type alias** so the canonical type travels unchanged
through the codebase:

```
bestiary.Harness
   └── schema (module):   type Harness = bestiary.Harness        (+ Harness* consts, Harnesses(), HarnessJSONSchema())
         └── internal/defaults:  type Harness = schema.Harness   (+ re-exported consts, AllHarnesses, Harnesses, LegacyHarness*)
               └── internal/ingest:  type Harness = schema.Harness (+ re-exported consts)
```

Because `schema.Harness` is an **alias**, not a defined type, it cannot carry its own methods
(this is the constraint behind the OpenAPI decision below) and there is no peasant-side
`NewHarness` constructor.

### Known harnesses vs. ingestion-supported (the 4/4/6 split)

There are intentionally three different harness counts across surfaces — document this when
touching any of them:

| Set | Value | Where |
|-----|-------|-------|
| `bestiary.Harnesses()` / `schema.Harnesses()` | **6** — claude-code, gemini-cli, codex, opencode, cursor, antigravity | all *known* harnesses; the OpenAPI enum + `HarnessJSONSchema()` |
| `schema.AllHarnesses` / `defaults.AllHarnesses` | **4** — claude-code, gemini-cli, codex, opencode | the *ingestion-supported* subset (cursor/antigravity have no adapter yet) |
| `validate/schema.json` `SchemaHarness` enum | **4** | the publish wire-format (ingestion subset) |

`cursor`/`antigravity` are recognized (so the CLI/config can give a useful message) but peasant
has no ingester for them yet — see C7 below.

## Models & providers

`peasant models` uses bestiary for model reference data instead of a hand-rolled models.dev
fetcher:

- `internal/ingest/models_bestiary.go` — pure adapter (`ModelFromBestiary` / `ModelsFromBestiary`),
  preserve-if-present `LastSynced`.
- `cmd/peasant/cmd_models.go` — a `ModelFetcher` injection seam; provider `IsKnown()` guard before
  fetch; on `*bestiary.ErrAPIUnavailable` falls back to `bestiary.StaticModels()` (exit 0 + a
  stderr warning naming the snapshot vintage); context cancellation propagates non-zero (no fallback).
- `bestiary.Provider` (Anthropic/OpenAI/Google) keys model data; peasant does not alias it the way
  it aliases `Harness`.

## Wire-value migration (claude → claude-code, gemini → gemini-cli)

The bestiary identifiers renamed the two harnesses peasant already supported:

| Old (pre-bestiary) | New (canonical) |
|--------------------|-----------------|
| `claude`           | `claude-code`   |
| `gemini`           | `gemini-cli`    |
| `opencode`, `codex` | unchanged       |

These flow end-to-end through the DB (`model_harness` column), the JSON/OpenAPI surfaces, config,
and the CLI. The DB rename is done by migration **V33** (`internal/store/schema_v33.go`), which is
gated by a TTY consent prompt + `.bak` backup on existing user databases
(`internal/store/migration_consent.go`).

## Deprecation and compatibility

Peasant accepts the old identifiers only to give a clear migration error, never to silently map
them.

| Surface | Behavior |
|---------|----------|
| CLI `--source-provider` | Legacy `claude`/`gemini` → "renamed to X, rerun with X"; the `defaults.LegacyHarnessClaude`/`LegacyHarnessGemini` constants exist solely for this (`resolveHarnessFlag`, `cmd/peasant/cmd_harvest.go`). |
| config YAML | Deprecated keys are **rejected with remediation, never silently mapped**: `sources.claude:` (→ `claude-code:`) and `selection.providers:` (→ `selection.harnesses:`). Implemented via `Deprecated*` capture fields + `validate()` in `internal/config/config.go`. |
| CLI + adapters | `cursor`/`antigravity` are recognized but unsupported → "planned for a future release", with **no specific version commitment**. |

## OpenAPI enum exposure

Because `schema.Harness` is an alias, it cannot implement swaggest's `Exposer` interface
(`JSONSchema()`) the way the sibling local enums (`Visibility`, `Role`, `SourceFormat`, …) do.
So the Harness enum is injected into the generated OpenAPI specs by an `InterceptSchema`
reflector hook:

- `openapi/harness_schema.go` (in the `github.com/peasant-labs/schema` module) — `registerHarnessSchema(r)` injects `schema.HarnessJSONSchema()`.
- Wired into every reflector that needs it: `village.go`, `peasantlocal.go`, `shared.go`.
- Guarded by that module's `openapi/harness_enum_test.go` (asserts the enum actually lands in the specs).

The schema module's tests assert that the enum is present in generated specifications. Any new
reflector must register the same schema hook so it cannot silently omit the enum.

## Key files

| File | Purpose |
|------|---------|
| `github.com/peasant-labs/schema` `types.go` | `Harness` alias, consts, `Harnesses()`, `HarnessJSONSchema()`, `HarnessDisplayName` |
| `internal/defaults/provider.go` | re-exports + `AllHarnesses` + `Harnesses` + TEMPORARY `LegacyHarness*` |
| `internal/ingest/models_bestiary.go` | model adapter |
| `cmd/peasant/cmd_models.go` | `ModelFetcher` seam + static fallback |
| `cmd/peasant/cmd_harvest.go` | `resolveHarnessFlag` compatibility and unsupported-harness errors |
| `internal/config/config.go` | config-key deprecation (`DeprecatedClaude`, `DeprecatedProviders`) |
| `internal/store/schema_v33.go` + `migration_consent.go` | DB wire-value migration + consent gate |
| `github.com/peasant-labs/schema` `openapi/harness_schema.go` | OpenAPI enum hook |

## Regenerating generated artifacts

- **CLI reference** (`docs/cli/`): `peasant docgen docs/cli`
- **OpenAPI specs / schemas**: regenerated in the `github.com/peasant-labs/schema` module
  (peasant consumes them via its `go.mod` pin; it no longer generates specs locally).
