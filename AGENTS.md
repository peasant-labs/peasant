# Contributor Engineering Guide

This file records repository-specific constraints for people and automated contributors. Read
[`CONTRIBUTING.md`](CONTRIBUTING.md) before starting.

## Quality gates

Use the Nix development shell when possible:

```bash
nix develop
make check
```

`make check` runs formatting, vetting, static checks, and the Go test suite. Go tests must use the
race detector, including focused runs:

```bash
go test -race ./...
go test -race ./internal/ingest -run TestName -v
```

Build the CLI with `make build`. Full-stack tests require Podman and the companion Village service;
run them with `make e2e`. See [`TESTING.md`](TESTING.md) and [`docs/e2e.md`](docs/e2e.md) for prerequisites.

### TUI visual review

Every user-visible terminal UI change must follow
[`.claude/skills/tui-visual-review/SKILL.md`](.claude/skills/tui-visual-review/SKILL.md). This includes
changes to mounted layout, hierarchy, copy, previews, focus, search, facets, row annotations, themes,
forms, and navigation.

The required workflow runs the opt-in mounted screenshot tests, rasterizes deterministic ANSI states
with Freeze, and manually inspects the changed states in both themes at `80x24` and `120x40`. If the
strict capture fixture does not represent the changed state, extend it before capturing. ANSI goldens
and successful PNG generation are not visual self-review. Dirty captures are disposable development
evidence; interface-changing PRs require clean-revision screenshots and durable GitHub-hosted review
evidence. Generated PNGs remain untracked.

## Tests and fixtures

- Prefer integration tests for behavior involving I/O, state, or multiple components.
- Test the production path and mock dependencies, not the system under test.
- Put combinatorial, permutation, and shared test cases in the existing YAML fixture families rather
  than inline tables. Reuse `internal/testutil` helpers for common values.
- Assert observable outcomes rather than private implementation details.
- Add compile-time interface guards for new interface implementations.
- Use external test packages when importing `internal/testutil` would otherwise create an import
  cycle.

## Types and boundaries

- Use named enum types and constants for statuses, harnesses, formats, roles, and other closed sets.
- Validate raw strings through their `New*` constructors at input boundaries; do not cast directly
  in production code.
- Keep reusable defaults in `internal/defaults`; keep package-specific values local.
- Keep dependencies injectable. Production wiring must use real dependencies, while tests may
  replace those dependencies.
- Use atomic file operations for persisted data and preserve the existing XDG directory layout.

Run the repository's ast-grep rules when changing Go types or literals:

```bash
ast-grep scan --config sgconfig.yml .
```

## Data and contract invariants

- `SessionDetailPayload` is produced through one conversion path:
  `store.ListEntries()` → `api.EntriesToTurns()` → `api.SessionToDetail()`. Changes must remain
  consistent for the WebSocket viewer and session export.
- Previous database migrations are immutable. Add a new migration and its own focused test. Only
  the final-schema assertions in `store_test.go` should change for a new migration.
- The API and wire types are owned by [`github.com/peasant-labs/schema`](https://github.com/peasant-labs/schema).
  Contract changes land and receive a module tag there before Peasant updates its dependency. Do
  not hand-edit generated OpenAPI artifacts.
- Redaction rules and their canonical fixtures are owned by
  [`github.com/peasant-labs/redact`](https://github.com/peasant-labs/redact). Keep Peasant's mounted
  command, ingest, configuration, and API integration coverage when updating that dependency.
- The Village publish and pull formats are shared contracts. Coordinate schema, producer, and
  server changes so validation remains documented and enforced consistently.
- Widening the license set requires a new SQLite migration that rebuilds both local tables carrying
  license CHECK constraints.

## Frontend integration

Peasant consumes published `@peasant-labs/fairtrade` and
`@peasant-labs/transcript-browser` packages. Transcript turn rendering belongs in the browser
package; Peasant's session-detail code is an adapter for local data, navigation, and annotations.
Avoid duplicating shared rendering behavior in this repository.

For visual changes, use `web/scripts/visual/` and capture the real built binary path. Confirm that
the server is serving the newly built assets before trusting screenshots or computed-style probes.

## Documentation

- Keep user-visible behavior and examples accurate; do not document planned behavior as shipped.
- Generate CLI reference pages with `make docs-cli`; do not hand-edit generated CLI pages.
- Never include credentials, private transcript content, personal filesystem paths, or private
  project history in issues, fixtures, logs, screenshots, or documentation.
