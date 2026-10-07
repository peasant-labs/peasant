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

### Performance work

Every performance-critical change follows
[`.claude/skills/design-performance/SKILL.md`](.claude/skills/design-performance/SKILL.md). This
includes the hot paths (harvest, ingest, the store write lane, search, migration, reclaim), every
performance claim and budget, and the design of the parallel work and the memory layout. The live
store stays read-only for agents; measure on a sandbox copy.

## Tests and fixtures

The test gate itself — two passes, the no-race registry, the four-rule
exactly-once screen, the run classes, and the committed budget — is documented in
`TESTING.md` under **Test gate**. Run `make check` as usual; the gate is the entry
point, not a wrapper you invoke by hand.

- Use an integration test first for behavior that involves I/O, state, or more than one
  component.
- Test the production path. Mock the dependencies, not the system under test.
- Put combinatorial, permutation, and shared test cases in the YAML fixture families that exist.
  Do not write them as inline tables.
- Reuse the helpers in `internal/testutil` for common values.
- Assert observable outcomes. Do not assert private implementation details.
- Add a compile-time interface guard for each new interface implementation.
- Use an external test package when `internal/testutil` would create an import cycle.
- The shared test filesystem decorators (`GatedFS`, `BoundedFS`, and the existing
  `CountingFS`) implement the contract in `internal/testkit/fsdecorator`. It is a
  standard-library-only leaf package so both `internal/testutil` and a white-box
  `package ingest` test can import it without an import cycle; do not move the
  declaration into `internal/testutil`. `fsdecorator.FileSystem` mirrors
  `ingest.FileSystem`, and a contract test keeps them identical.
- The decorators have two owners. `internal/testutil` owns the non-white-box
  decorators. The white-box `internal/ingest/fsfault_test.go` owns the decorators
  that need the package's unexported internals. Neither owner declares the shared
  contract; both implement it.
- The coverage map that records each moved, deleted, retained, or deferred test
  name — its `Inventory` and `CoverageMap` schema, the closed destination set,
  and the validators — lives in `internal/testkit/coveragemap`. `TESTING.md` describes
  the map and the decorator owners.
- A test wait names its wake source or carries a deadline. Real waits stay on the keep list with a
  reason. See `TESTING.md`.

## Types and boundaries

- Use named enum types and constants for closed sets, such as statuses, harnesses, formats, and
  roles.
- Validate raw strings through their `New*` constructors at input boundaries. Do not cast
  directly in production code.
- Keep reusable defaults in `internal/defaults`. Keep package-specific values local.
- Keep dependencies injectable. Production wiring uses real dependencies. Tests may replace them.
- Use atomic file operations for persisted data. Keep the existing XDG directory layout.
- The gate's exported shapes (per-invocation record, report document, registry,
  budget) and the shared stream library are frozen by contract tests. Change a
  shape only as a deliberate contract change; the `contract_test.go` files in
  `internal/testkit/testgate` and `internal/testkit/teststream` fail on a rename, removal, retype,
  retag, or reorder of a frozen field.

Run the ast-grep rules of the repository when you change Go types or literals:

```bash
ast-grep scan --config sgconfig.yml .
```

### Record-kind vocabulary

- Each harness owns one co-located vocabulary declaration under `internal/ingest/*_vocabulary.go`. Those
  declarations are the source of truth for the record and content-block kinds the parser recognizes.
  Never hand-edit `internal/ingest/record_kinds.yaml` or `docs/record-kinds.md`; both are generated
  output.
- `internal/indexformat.Outcome` is the interpretation IR: text, tool call, tool result, control,
  ignored, or opaque. Adapters declare only the outcome. The central lowering owns stored entry mode,
  preview eligibility, payload shape, and coordinate requirements.
- A well-formed valid kind that is absent from the vocabulary resolves to opaque retained evidence by
  default. Malformed known data and failed retention remain validation failures. The registry is
  reporting-only and never admits or refuses parser input.
- Rendering is a Fairtrade consumer concern. Do not add visualization state, renderer names, or
  viewer coverage to the registry or the harvest report.
- Keep the exact per-adapter production-census and required-name tests green. Do not reintroduce the
  retired AST scanner. After changing a vocabulary or its stored behavior, run `go generate
  ./internal/ingest`, the registry/docgen tests, and bump the relevant indexer version when settled
  sessions must be re-indexed.

## Cross-platform code

Peasant builds one module for Linux, macOS, and Windows. Platform-specific code lives in
build-tagged sibling files (`//go:build unix` / `//go:build windows`, or `unix` / `!unix`) with
identical signatures; the shared file stays platform-neutral, and the unix side must not change
behavior. A shared file must never reference a type that exists on only one target.
[`docs/cross-platform.md`](docs/cross-platform.md) records the split, the path rules, the
Windows-specific behavior, and the CI and release gates. When you add platform-specific code, add
a build-tagged test per side and add any Windows-only test name to the `windows` job's `-run` set
so it actually runs.

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
- Every JSONL harness reads, redacts and indexes a single source record up to
  `defaults.MaxJSONLRecordBytes` (256 MiB) in full. No record size ever fails a session and no
  record is ever silently dropped. A record over the limit is left out before redaction, reported
  with the `record_too_large` diagnostic naming its size, its line and the limit, and stored as an
  incomplete capture with the failure code `source_records_omitted`. The indexed entries hold a
  placeholder entry at the omitted record's position, carrying the typed omission record in
  `extra` and a reader-facing note in `contentPreview`. Those are existing `SessionEntry` fields,
  so the omission reaches every transcript UI with no schema-repository change; promoting it to a
  first-class wire field follows the contract ceremony.
- Because the placeholder accounts for what is missing, that capture is written as FULL content
  and previews, export and publication all carry it, with the placeholder and with
  `diagnostics.partial` set. It is the ONE incompleteness that may be published. The same failure
  code is also raised where nothing stands in the gap, by an OpenCode part this build cannot
  render and by an orphan graph part. There the content is simply missing, the capture stays a
  bounded preview, and export and publication stay refused. What decides is the stored entries,
  not the code: a capture is certified full exactly when a placeholder accounts for the omission.
- OpenCode reads rows and legacy documents rather than lines and has no per-record size limit at
  all, so no OpenCode record fails or is dropped at any size and the over-limit omission does not
  apply to it.
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
- Publishing is a separate, user-initiated action: the transcript publish popup or the multi-session `/share` wizard. Nothing is published
  without an explicit act: a click, or a binding the developer set up. It draws only from the
  sessions the user recorded. Pulled transcripts are not re-pushable. Governance for re-sharing
  pulled sessions is a tracked follow-up.
- The consented publication paths are the transcript publish popup, the `/share` wizard, the upload hook installed by
  `peasant village hooks install`, the auto-publish hook, and attaching the prompts behind a pull
  request. The auto-publish hook is the same managed hook, installed by `peasant village auto` or
  by the settings install route, for a repository that an auto-publish rule in `hooks.yaml`
  covers. The rule is the binding. `autopublish.Decide` is the one matcher of rules,
  server-side; do not implement it again in React. `peasant village push`, which a managed hook
  runs, applies it per session, to the repository the session was recorded in (a linked
  worktree counts as its main repository). A gone directory is matched by its
  recorded path and ancestors because its repository root is no longer known;
  a deleted nested repository can inherit a containing folder rule. A push that sends a bound session publishes
  collectives-only: private, no license, and each bound transcript is shared with its rule's
  collectives, never with the public. A session a paused rule (no event) covers is not
  published, a transcript that is public on Village is not updated, and a collective that
  rejected or lost a transcript is not asked again. Kickstart's publication and the local web's
  publish do not read the rules. A rule-installed hook requires an active matching binding; deleting or
  pausing the last binding stops its publication while retaining its hook file.
  Separately installed plain terminal hooks retain their independent consent.
  A rule installs no hook by itself: `peasant village auto` and
  the settings install route install one repository at a time, by an explicit act, and only in
  a repository Peasant has recorded sessions in. Any managed hook applies the rules, including
  one installed with `peasant village hooks install`, so saving a rule changes what an installed
  hook publishes. Attaching is a GitHub-side path: it uploads nothing and publishes nothing, and only widens who may read
  transcripts already published. Do not add a path that publishes without one of these, and do
  not create a binding for the developer.
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
- The `changes` label and the `/review` routes stay in force. In the home-first registry
  `changes` is a route-only section (`inNav: false`): it keeps its id, label and routes, and
  `home` leads the nav instead. Do not rename or delete the label or the routes silently.
- `/share` is the canonical publish route: one-session links open the transcript popup;
  multi-session and wizard-step links retain the wizard. It stays
  outside the fairtrade section registry, and the local header does not link to it. `LOCAL_APP_SECTIONS` in fairtrade owns the registry:
  `home | settings` in the nav, then `analytics | changes | code map` by route only.
  `GRAPH_APP_SECTIONS` is its deprecated alias with the earlier three-entry value. Derive the nav
  and routes from the registry (`web/src/lib/nav/sections.ts`) and fail loudly on an unmapped id.
  A nav section this app has no page for stays out of the header until its page ships, so the
  header never carries a dead link. Nothing in the header or the command palette links to a
  route-only section; its routes still resolve by URL. Do not add a `/push` alternate route.
- The local header is one row: the `peasant` home link, search (⌘K), the nav sections other than
  home, and an icon-only theme toggle. It carries no connection indicator. When the local app is
  unreachable (`GET /api/v1/health` fails or the WebSocket stays down), fairtrade's
  `LocalOfflineBanner` shows at the top of the page under the header, with the start command and
  `try again`. Its copy says the app on this computer is not running and that the internet is
  fine; never replace it with copy that reads as an internet outage. `web/scripts/visual/testdata/shell-header.yaml` is the
  required-name manifest for the header, and the component tests and the mounted shell gates
  read it.
- When you replace the share-bridge UI, keep these semantics: auto-scan of uncached selections;
  caching of success and of honest failure, keyed by `(level, session)`, across navigation;
  explicit re-scan; continuation disabled when any session failed; fail-closed behavior on a
  category inconsistency.
- peasant-labs/fairtrade-design-system#3 tracks the official review, redaction, consent, and
  share composition. Per-category filtering in `RedactionReview` is
  peasant-labs/fairtrade-design-system#4. Peasant owns the transcript popup and `/share` scan, app-level cache, auth, and
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

## Git staging and commits

- Stage intended changes with `git add -- <path>...`; inspect `git diff --cached` before committing.
- Never use `git add .`, `git add -A`, or wildcard staging.
- Commit with `git agent-commit -m "..."`.
