# Contributor Engineering Guide

This file records the rules that apply to work in this repository. People and automated
contributors follow the same rules. Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before you start.

## Quality gates

Use the Nix development shell when it is available:

```bash
nix develop
make check
```

`make check` runs the formatting, the vetting, the static checks, and the Go test suite. Run every
Go test with the race detector. Do this also for a focused run:

```bash
go test -race ./...
go test -race ./internal/ingest -run TestName -v
```

Build the CLI with `make build`. The full-stack tests need Podman and the companion Village
service. Run them with `make e2e`. [`TESTING.md`](TESTING.md) and [`docs/e2e.md`](docs/e2e.md)
list the prerequisites.

### TUI visual review

Every change that a person can see in the terminal UI follows
[`.claude/skills/tui-visual-review/SKILL.md`](.claude/skills/tui-visual-review/SKILL.md). This
includes the mounted layout, hierarchy, copy, previews, focus, search, facets, row annotations,
themes, forms, and navigation.

Compose every terminal surface from the layout primitives in `internal/tui/kit`. Do not pad,
place, align, or paint a background at the surface. [`docs/tui-layout.md`](docs/tui-layout.md)
explains the rule. A grep gate enforces it.

The workflow:

1. Run the opt-in mounted screenshot tests.
2. Rasterize the deterministic ANSI states with Freeze.
3. Inspect the changed states yourself, in both themes, at `80x24` and `120x40`.

When the strict capture fixture does not show the changed state, extend the fixture first. ANSI
goldens and a successful PNG generation are not a visual self-review. Dirty captures are
development evidence only. An interface-changing PR needs clean-revision screenshots and durable
GitHub-hosted review evidence. Generated PNGs stay untracked.

## Tests and fixtures

- Use an integration test first for behavior that involves I/O, state, or more than one
  component.
- Test the production path. Mock the dependencies, not the system under test.
- Put combinatorial, permutation, and shared test cases in the YAML fixture families that exist.
  Do not write them as inline tables.
- Reuse the helpers in `internal/testutil` for common values.
- Assert observable outcomes. Do not assert private implementation details.
- Add a compile-time interface guard for each new interface implementation.
- Use an external test package when `internal/testutil` would create an import cycle.

## Types and boundaries

- Use named enum types and constants for closed sets, such as statuses, harnesses, formats, and
  roles.
- Validate raw strings through their `New*` constructors at input boundaries. Do not cast
  directly in production code.
- Keep reusable defaults in `internal/defaults`. Keep package-specific values local.
- Keep dependencies injectable. Production wiring uses real dependencies. Tests may replace them.
- Use atomic file operations for persisted data. Keep the existing XDG directory layout.

Run the ast-grep rules of the repository when you change Go types or literals:

```bash
ast-grep scan --config sgconfig.yml .
```

## Data and contract invariants

- Produce `SessionDetailPayload` through one conversion path:
  `store.ListEntries()` → `api.EntriesToTurns()` → `api.SessionToDetail()`. Keep a change
  consistent for the WebSocket viewer and for the session export.
- Shipped migrations are immutable. Add a new migration with its own focused test. For a new
  migration, only the final-schema assertions in `store_test.go` change.
- [`github.com/peasant-labs/schema`](https://github.com/peasant-labs/schema) owns the API and
  wire types. A contract change lands in that repository and gets a module tag before Peasant
  updates its dependency. Do not hand-edit generated OpenAPI artifacts.
- The Peasant web endpoints are part of the schema-owned Peasant Local API contract. A new or
  changed HTTP or WebSocket route, method, request, response, message, capability token, or
  status behavior is part of this contract. Such a change needs a schema-repository PR that
  updates the Peasant Local OpenAPI specification. You may prototype the consumer and the
  contract together on unmerged branches. Before the Peasant change merges or ships, land and
  tag the schema contract, re-pin Peasant to that release, and check the implementation against
  it. See [`docs/contract/versioning-procedure.md`](docs/contract/versioning-procedure.md).
- [`github.com/peasant-labs/redact`](https://github.com/peasant-labs/redact) owns the redaction
  rules and their canonical fixtures. When you update that dependency, keep the coverage of the
  mounted command, of ingest, of configuration, and of the API integration.
- The Village publish and pull formats are shared contracts. Coordinate the schema, producer, and
  server changes so that validation stays documented and enforced.
- A wider license set needs a new SQLite migration. The migration rebuilds both local tables that
  carry a license CHECK constraint. The tests derive the accept-sets from `schema.AllLicenses`,
  so a wider menu fails the tests until the migration lands. Village `AGENTS.md`, section
  "Adding a license", gives the canonical cross-repo procedure.

### Redaction policy

The engine, [`github.com/peasant-labs/redact`](https://github.com/peasant-labs/redact), defines
the semantic categories: `secrets`, `pii`, `paths`, and `project`. The engine also defines the
rules, the canonical fixtures, and the rule-set versioning. Git remotes, import paths, Docker
refs, branch names, and CI project variables are semantically `project`. Do not add a separate
git-context category.

- Activation is independent of the category. `Rule.MinimumLevel` is optional. An empty value
  inherits the category minimum. A set value must name a stricter level. User patterns inherit
  their category default and carry no override of their own.
- The built-in git-context rules `git_remote_https`, `git_remote_ssh`, and `git_branch_output`
  set `MinimumLevel: Maximum`. This is a legacy configuration. It cannot fire from any offered
  level.
- The level dispositions live in the policy in `internal/config`. `standard` is the only offered
  level. `minimal` is accepted in a stored configuration, but the app silently raises it to
  `standard`. `maximum` is refused.
- A rendered consumer label comes only from the engine's `Category.String()`. `secrets` prints
  CREDENTIAL. `pii` prints PII. `paths` prints PATH. `project` prints INTERNAL. Do not use a
  private web-category or an internal mapping.
- An unknown category fails closed. Validate it at the trust boundary. The server, the generated
  mocks, and the frontend reject an unknown or group/item-inconsistent category. They never
  relabel it. Use the redact actionable-error machinery, so that what, why, where, when,
  meaning, and fix stay visible.

## Frontend integration

Peasant consumes the published `@peasant-labs/fairtrade` package. Transcript turn rendering
belongs in the `/ui` and `/graph` entries of fairtrade. The session-detail code of Peasant is an
adapter for local data, navigation, and annotations. Do not duplicate shared rendering behavior
in this repository.

For visual changes, use `web/scripts/visual/` and capture the real built binary path. Make sure
the server serves the newly built assets before you trust a screenshot or a computed-style probe.

## Session selection and discovery

- Kickstart stores a `config.SelectionConfig`. It holds a `mode` of `all` or `selected`, one
  entry per harness project (`gitRemote`/`name`, with optional branches), and explicit session
  IDs.
- `ingest.SelectionMatcher` is the canonical matcher. Ingest, push, and prune already use it. Do
  not implement its semantics again in React.
- A selection scopes discovery and lists only. The scoped surfaces are the WS/REST session and
  project lists, the Home and Map project pickers, the command palette, and the share chooser. A
  selection is not an access-control boundary over stored data.
- A session that is already ingested stays reachable through a direct deep link.
- Ingest filters each newly discovered, unselected session before the session is stored.
- Only a manual `peasant prune` removes historical rows. A narrower selection never deletes data
  by itself.
- Do not add a fail-closed gate on deep links. An earlier attempt was withdrawn as a misread of
  the user's intent. Do not reintroduce it without a new, explicit ratification.
- Publishing is a separate, user-initiated action: the `/share` wizard. It never runs
  automatically or in the background. It draws only from the sessions the user recorded. Pulled
  transcripts are not re-pushable. Governance for re-sharing pulled sessions is a tracked
  follow-up.
- One requirement is not yet landed. The live tracker is #3. When `mode` is `selected`, the
  user-facing lists show only the configured selection. An explicit session selection must not
  widen visibility to the sibling sessions of its project. Apply the boundary server-side.
  Derive the counts and the empty states from the same visible set.

## Git history and session UX

- `session_commits` records which Git commits belong to a recorded session. The review-list wire
  collapses this association into `CommitRef.hasSession`. That boolean cannot name or link a
  session.
- The target experience, tracked as epic #4: Git is the timeline spine, and the associated user
  sessions annotate it. Keep bound and candidate/temporal associations distinct. Keep unattached
  sessions discoverable.
- A wire change lands through the schema contract ceremony first.
- The `changes` label and the `/review` routes stay in force until a user-ratified replacement
  lands. Do not rename or delete them silently.
- `/share` is the canonical share surface. The persistent top-nav action routes there. It stays
  outside the fairtrade graph-section registry. `GRAPH_APP_SECTIONS` in fairtrade owns the
  registry and fixes it to `analytics | changes | code map`. Do not add a `/push` alternate
  route.
- When you replace the share-bridge UI, keep these semantics: auto-scan of uncached selections;
  caching of success and of honest failure, keyed by `(level, session)`, across navigation;
  explicit re-scan; continuation disabled when any session failed; fail-closed behavior on a
  category inconsistency.
- peasant-labs/fairtrade-design-system#3 tracks the official review, redaction, consent, and
  share composition. Per-category filtering in `RedactionReview` is
  peasant-labs/fairtrade-design-system#4. Peasant owns the `/share` scan, cache, auth, and
  network orchestration, and the Village transport.
- The code map is a known comprehension gap. Future work needs progressive, task-oriented
  disclosure and a real-project user acceptance test. Do not add more text around the same dense
  graph. Do not fork the canonical graph visuals of fairtrade inside this repository.

## Documentation

- Keep the user-visible behavior and the examples accurate. Do not document planned behavior as
  shipped.
- Generate the CLI reference pages with `make docs-cli`. Do not hand-edit generated CLI pages.
- Never put credentials, private transcript content, personal filesystem paths, or private
  project history into issues, fixtures, logs, screenshots, or documentation.
