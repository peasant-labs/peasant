# Changelog

All notable changes to Peasant are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Peasant follows
[Semantic Versioning](https://semver.org/). Each version links to its GitHub
Release, which holds the signed artifacts and checksums.

## [Unreleased]

### Fixed
- Claude discovery skips harness-injected `isMeta` notices when choosing session titles. Existing cached transcripts are read again once to refresh their titles.

### Fixed
- `--data-dir` now keeps default transcript copies beside its database under
  `<data-dir>/peasant/peasant-sync`, including web publication and reindexing.
  A configured custom output path is preserved. Existing copies are not moved;
  installations that previously used `--data-dir` can run `harvest --force`
  with that same data directory once to recreate them in the corrected location.
### Changed
- The local web commands accept decimal TCP ports from 1 to 65535 and reject
  invalid values before starting a server. Base-prefixed values such as `0x50`
  are refused; `080` means port 80.

## [0.13.5] - 2026-10-29

### Fixed
- Transcript search highlights matches and reveals results inside nested output.
- Mermaid diagrams accept safe line breaks, and tool disclosures start their
  transitions from the beginning.
- Fixed-width readers adapt to the space available beside the sidebars.

## [0.13.4] - 2026-10-28

### Changed
- The transcript uses the shared bounded layout and identifies recorded pull
  requests as unverified evidence. Their source opens the recorded command and
  result without making a GitHub request or changing access.

## [0.13.3] - 2026-10-28

### Changed
- The local transcript moves to Fairtrade 0.0.48. Model actions appear once per
  user turn after the first prose, with the action list open and each output
  closed. Compact mode collapses the lists. Thinking has its own boxed output,
  and Bash input keeps one prompt without a repeated icon.

### Fixed
- Turn selection follows clicks and rapid keyboard navigation, the reading
  width stays where it was set, and the outline follows the reading position.
  System turns can be hidden with the filters. Action disclosures and sidebars
  retain their contents while closing, and the condensed header exchanges
  space with the timeline without a jump, respecting reduced motion.

## [0.13.2] - 2026-10-27

### Changed
- The local transcript moves to Fairtrade 0.0.47. A turn's work is its own row
  after the prose, opened from its summary line, and the tool and thinking
  lines are inline detail rather than a footer on the turn. The timeline reads
  amber for agent turns again, the reader centres in the space it has, and the
  publish action reads `publish` in every state.

### Fixed
- Zen takes the app's own sidebar with it. The transcript fills the window
  while zen is on, and the sidebar's collapse state is untouched, so leaving
  zen restores exactly what the reader had.

## [0.13.1] - 2026-10-26

### Changed
- The local transcript reads as a ledger on Fairtrade 0.0.46: each turn leads
  with its role, number, time and copy link, its tool calls gather into one work
  panel, and a system notice is one compact row. The session views and the
  search trigger sit in a bottom bar under the body, the shortcut hint and the
  position counter stay available there, and the reader can set the reading
  width, enter zen, and copy a turn's anchor from the head. Provider identity
  stays on the header's meta row.

## [0.13.0] - 2026-10-26

### Added
- The session read carries the commits a session produced and the pull
  requests its `gh pr create` calls detected, so the local web shows both
  under a session instead of leaving them to the git history alone. A pulled
  request is bound to the evidence that named it, an unknown-time commit stays
  unattributed, and the session read advances to schema 0.34.1.
- The local reads expose a recorded Claude teammate identity and its lineage.
  A session qualifies only when a stored Claude Code child and its stored
  parent both name the same team and agent, and the parent's recorded spawns
  name that pair; the relation is read from the existing evidence cache with
  no schema change, and the durable detail payload never carries it.
- A native OpenCode fork is recorded as a `context_from` relationship. The
  child already materialises the parent prefix, so the relation carries the
  native evidence and an anchor rather than a copied prefix, and the snapshot
  digest binds the pointer and the boundary.
- The command pull-request detector unwraps shell wrappers around
  `gh pr create`, so a request created through a wrapper script is detected and
  its branches have pinned handling.

### Changed
- The local web moves to Fairtrade 0.0.46 and the ledger reading surface: a
  turn reads as a role-led line, its tool calls group into one work panel, a
  system notice is one compact row, and the session views sit in a bottom bar
  with the reading-width control, zen, and the shared work grid.
- Pulling asks peasant online for pull contract 0.3.0. Each pull read (the
  list, a transcript's metadata, its annotations and its content) sends the
  `Peasant-Pull-Contract` header, so a peasant online that serves 0.2.0 by
  default serves this CLI the 0.3.0 envelope, where each person is shown as you
  may see them. The response names the envelope it carries in the same header
  and is read as exactly that envelope; any 0.3.x patch is read as 0.3.0. A
  peasant online that predates the header is read as before.
- When peasant online no longer serves the pull contract this CLI reads, it
  answers 406 and the pull stops with the "pull-contract incompatible" error,
  which says to upgrade the CLI. The same error stops a pull when peasant
  online serves an envelope this CLI does not read.

### Fixed
- A Claude Code VS Code prompt stays one user turn. The extension writes
  injected editor context and the typed prompt as two text blocks, and the
  decomposition turned the prompt into a subagent turn while repeating the
  context; a user record written as a content-block array now stores its whole
  text on the one depth-0 carrier, and empty system control turns with a source
  reference no longer render as empty headers.
- A one-session re-index that leaves the stored evidence refused says so. The
  response used to report done while the same stored copy stayed in place, so
  the publish popup offered the same repair again; it now names the step that
  is actually left.
- An intact pair whose source refuses is settled once instead of repeating on
  every ingest, and an unrepairable stored pair is settled rather than retried.

## [0.12.0] - 2026-10-24

### Changed
- Pulling reads pull contract 0.3.0, where peasant online shows each
  transcript owner and annotation author as you may see them: their public
  profile, or `anon` when they are not discoverable to you. Lists, tables and
  the `context` header print `anon` for such a person, `--user` never matches
  one, and the pull manifest and local index store none of their identity,
  nor the project name or repository targets beside an anonymous owner. A
  transcript pulled before its owner or an annotation author became anonymous
  to you, or before its owner was renamed, is re-pulled in full on the next
  pull, so the stored copy drops the identity too.
- **JSON output:** `village transcripts list --json` (remote) prints each
  `owner` as an object, `{"kind":"profile","id","username","display_name",
  "avatar_url"}` or `{"kind":"anonymous"}`; the `ownerUserId` and
  `ownerUsername` fields are gone. `list --local --json` keeps `owner`, and
  `context --json` keeps `ownerUsername`, as the stored username, or `null`
  for an anonymous owner.
- This CLI pulls from peasant online on pull contract 0.2.0 or 0.3.0: it
  accepts the envelope peasant online serves when that version is one it
  reads, decodes exactly that envelope, and records it in the pull manifest.
  Older CLIs read only 0.2.0, so once peasant online moves to 0.3.0 they stop
  pulling with the "pull-contract incompatible" error and must upgrade.
  Publishing and every other command keep working on older CLIs.

## [0.11.1] - 2026-10-23

### Changed
- The local web finishes its redesign on Fairtrade 0.0.40. The sidebar puts
  search, the docs, community (soon) and the collectives link that opens online
  on top, the app's pages under them, and the account, a settings gear and the
  theme toggle in its footer, at the same width as online. Online leaves the
  section nav and the app switcher; the online page stays at `/village`,
  reached by its address like analytics, changes and the code map.
- Every local page sits in one page frame with one title size. Settings carry
  one "settings" title above their section bar, and each section's name heads
  its column. Nothing on the map, its rail or the project lists is smaller than
  14px, and every control is 44px tall on a touch screen.
- How-to lives in the docs: each task links to its docs section instead of
  being taught in the app, and pages and settings drop their explainer
  sentences and provenance chips.
- Plainer copy: the link to the hosted app reads "dashboard", the account row
  reads "signed in to peasant online with github" and names the server only
  when it is not the default one, the CLI and terminal UI call the hosted app
  "peasant online", and a publication reads "published to N collectives",
  "needs update" or "published".
- The publish popup re-indexes a session whose local index is stale, only when
  asked and only that session, names each check step, and says in one line
  what stopped a re-index, with the reason behind details.
- Re-pin Fairtrade to 0.0.40.

### Fixed
- A project link that cannot be opened shows a designed "project not found" or
  "couldn't open" page instead of the failed request, and `/review` lists the
  projects instead of one line of text.
- A recorded session with no indexed turns says so and offers a re-index, and
  the page reads a session again after a re-index instead of keeping an empty
  transcript.
- A session's local detail carries its stored token and duration totals, so
  the transcript header no longer shows "0 tokens".
- Session titles on Home, in the session heading and in the map rail drop
  Cursor's `<user_query>` wrapper, and the transcript shows Cursor prompts
  without it.
- Home's filters name projects by their short name, its status cell sits on
  the row's baseline, and an empty analytics page shows the nothing-recorded
  state instead of empty charts.
- One Claude Code record with an unreadable payload (a lone surrogate escape,
  a duplicate key or invalid UTF-8) no longer fails the share scan, export and
  publication of the whole session: the payload is kept only when the strict
  evidence reader accepts it. Claude Code sessions re-index once, on the first
  harvest after upgrading.
- A Cursor session whose project path has a component starting with "-" is
  filed under its own project, not a parent folder.

## [0.11.0] - 2026-10-23

### Added
- Scoped collective observations: `peasant village collectives fetch`, `list`,
  `track` and `untrack`, and `peasant village contributions list`, record the
  metadata of the collectives a signed-in account can see and dated evidence of
  its contributions, guarded against a credential change mid-command. The local
  web shows the saved observations offline on its online page (`/village`)
  (#637).

### Changed
- The local web is redesigned on Fairtrade 0.0.32. Fairtrade's sidebar replaces
  the one-row header: the registry's sections, a collectives link that opens
  online, search (still on ⌘K), the theme toggle, an app switcher that opens
  online and the docs and, when signed in, the account; on a phone it is a top
  bar with a drawer. Home is a session table with search, a filters menu (status
  and project) and a browse-by menu that keeps the project, helper-thread and
  every-session views. Settings open one section at a time (account, automatic
  publishing, redaction, projects, sources, advanced), and redaction leads with
  the four categories and the label each one prints. Pages and settings sections
  ease into view, and nothing moves for a reader who asks for reduced motion.
- The local web calls the hosted app "peasant online" (or "online") in its own
  copy, and the publish wizard says "publish" where it said "contribute" and
  "submit". CLI commands, routes and API paths keep their names.
- The offline notice sits at the top of the page column, pinned there on a
  roomy screen, and the page keeps the reader's place when it appears.
- Home loads its sessions through shared forward pages.
- Re-pin Fairtrade to 0.0.32.

### Fixed
- Home stays available when a recorded project directory is not a Git
  checkout (#640).

## [0.10.0] - 2026-10-16

### Added
- `peasant migrate` converts file-backed sessions to the harmonized content
  model in place, with a dry-run, resumable batches, and per-session verification
  before old data is removed. `peasant harvest verify --content` checks content
  and search integrity; `--repair` marks damaged content for repair (#586).
- Village transcript listing and bulk pull accept repeatable `--collective`
  selections, page through the union, and de-duplicate transcripts. Bulk pull
  shows live progress and supports cancellation while retaining completed pulls.
  `--user <username>` filters listing and bulk pull by contributor, including
  offline `list --local --user` over stored owner names (#631).
- Session thinking level and its changes are captured from Claude Code, Codex,
  OpenCode, and Pi evidence and carried through storage, session detail, export,
  and publication (#620).
- Harvest audits retain best-effort per-session outcomes and validated reason
  codes for the latest 100 runs, without copying transcript content, raw error
  messages, or filesystem locations. `docs/harvest-audit.md` explains their
  limits and read-only inspection; a harvest-audit skill documents the workflow
  (#601, #607, #610).

### Changed
- The harmonized content model gives each content string one storage home,
  unifies search, and bounds write batches by bytes and session count through
  `write.*` configuration. Session detail, publication transcript, and
  authoritative publication metadata documents share a 128 MiB limit; retained
  unknown payloads keep their separate 8 MiB limit (#586).
- Managed generations use one builder contract, including Claude Code format 2.
  A newer capture with unaccounted missing content is kept beside the last-good
  full copy: previews show the newer partial attempt, while normal full export
  and publication refuse rather than silently sending the older copy. Explicit
  `peasant export sessions --last-good` and hand-run
  `peasant village push --last-good` remain available; automatic publishing never
  substitutes the last-good copy (#627).
- The first harvest after the indexer and origin-rule updates may take longer
  while affected sessions are refreshed (#604, #620, #627).
- Re-pin Fairtrade to 0.0.29 and the browser Schema package to 0.29.1, using
  canonical typed redaction category bindings in both publication review paths.

### Fixed
- Home preserves parent relationships and groups subagents. Expanded children
  now occupy their own rows with their own project, turns, duration, time, and
  publication state (#619).
- Command-palette session-ID and project-hash matches display session/project
  identities instead of empty snippet rows (#544).
- Claude fork, agent-file, and continuation transcripts are accepted on verified
  structural ownership evidence; corrupt cached identities are re-mined rather
  than trusted (#604, #608, #609).
- Harvest recovers inherited-pair Codex results, retains native Codex delivery
  records, accepts successful Cursor turn endings, and preserves OpenCode's
  last-good authority during incomplete refreshes (#591, #595).
- Unavailable native sources no longer repeat repair warnings on every run;
  locator failures do not incorrectly mark a source unavailable (#600, #606).
- The retained kickstart publication path opens managed-session readers using
  the configured output location, so consented publication can read imported
  managed sessions without changing onboarding defaults.
- Invalid redaction review data blocks continuation and publication, while
  prerequisite recovery stays separate from publication retries (#634).
- Home keeps its existing search and load-more controls, with all supplied
  parent and child rows visible without an extra table pager or height cap.

### Documentation
- The experimental feature register clarifies that file familiarity /
  understanding coverage is incomplete, not a shipped end-to-end app feature;
  completion and an opt-in flag remain deferred (#630).

## [0.9.0] - 2026-10-07

### Added
- Native **Windows amd64** support: Peasant builds, passes CI, discovers and ingests
  Claude Code desktop sessions, serves the dashboard, and self-upgrades on Windows.
  Each release publishes a `peasant_<version>_windows_amd64.zip` (carrying
  `peasant.exe`, `LICENSE`, `README.md`, and `THIRD_PARTY_NOTICES`) and the bare
  `peasant_<version>_windows_amd64.exe` that `peasant upgrade` replaces in place;
  Windows arm64 is not published. Linux and macOS behavior is unchanged. The install
  guide is `docs/install/windows.md`; the maintainer guide is `docs/cross-platform.md`
  (#480, #563).
- `peasant reclaim`: a maintenance command that deletes superseded managed
  generations. It offers a dry-run that lists the sessions, generations, rows, and
  bytes it would reclaim and a resumable `--limit` batch mode; the destructive path
  requires consent or `--confirm` and prints the offline `VACUUM` step (#559).

### Performance
- The durable full-content writer prepares its manifest and chunk inserts once per
  transaction and re-binds them per row instead of preparing a statement for every
  row. A large full-content write dropped 24–28% in time and about half its
  allocations (#556).
- The managed-generation install prepares its per-row inserts once per install and
  re-binds them per row. A fresh-session install dropped 12–14% in time and about 38%
  of its allocations (#558).

### Fixed
- Publication repair selects only recoverable captures, and the pair-repair pass now
  owns pair-damaged maintenance candidates, settling missing-pair churn (#557).
- The IPv6-loopback refusal test verifies its bind-exclusivity premise and retries the
  rebind, so a parallel test that grabs the same port number no longer fails it
  spuriously while a genuinely leaked listener still does (#561).

### CI
- The local↔CI podman parity floor moved to 5.8.x, tracking the runner image.

## [0.8.2] - 2026-10-04

### Changed
- Managed-generation staging writes content blobs without a per-file sync and
  verifies every blob when a binding re-reads it, while the manifest and the
  directory stay durable. Staging a 1000-blob generation dropped from 1.40 s to
  0.067 s at 16 writers (19.09 s to 0.078 s at one writer), and a blob torn by
  a power loss refuses recovery instead of activating unservable content (#554).

### Fixed
- `peasant harvest index --dry-run` resolves the same managed-generation
  harness targets a real run uses. On a store upgraded to managed generations
  the forecast previously reported the retained baseline, planned a different
  set of sessions than the real run, and emitted false "stored producer
  revision is newer than this indexer's revision" refusals (#551).
- The stale-index pass at the end of a harvest reports the stored sessions the
  run's session selection left at an older index, or with an unfinished repair,
  with the remedy. A selected-mode kickstart run previously left thousands of
  stored sessions behind while the report showed only an "unchanged" total
  (#552).
- Publication readiness compares a stored capture's metadata schema version
  against the declared refresh-free set instead of exact equality, so a schema
  bump that adds no required field no longer marks every bound capture as
  needing a re-ingest. An ordinary harvest previously re-extracted and
  re-indexed thousands of unchanged sessions after the 10→11 bump (#555).

## [0.8.1] - 2026-10-04

### Changed
- Managed-generation staging and commits: candidates prepare their content
  files in parallel with bounded blob writers, each candidate commits through
  the serialized writer lane as its own staging completes, and the INDEX
  progress advances per committed session instead of per batch. A large
  OpenCode re-index no longer stalls behind one session's file writes (#546).
- FILTER maintenance passes run their per-session work through the bounded
  worker pool, read session locations in one bulk query per pass, and no longer
  rescan the whole output tree when the database already records a location.
  The stale-adapter and pair-repair inventories no longer take minutes on a
  large store (#547).

### Fixed
- OpenCode sessions refused with "transcript checksum does not match committed
  metadata": the staging arena released byte spans out of allocation order, so
  a later batch could recycle bytes a parser was still reading. Arena releases
  now advance the tail only over the contiguous released prefix (#548).
- Codex subagent sessions refused with "captured metadata is not
  self-consistent with the captured identity": a subagent rollout embeds the
  parent thread's envelope in a later `session_meta` record, and the parser
  re-checked every record. The first decodable metadata envelope now owns
  identity and context (#549).

## [0.8.0] - 2026-10-03

### Added
- Publishing from the transcript page: a publish bar and one popup replace the
  trip through the multi-session wizard. The popup mounts every state (not
  published, publishing, up to date, new turns, auto-publish on, outside the
  saved lists, connect, waiting, scanning, scan failed, not in a collective,
  stopped, waits for approval), counts new turns from the stored `publishedAt`,
  keeps the redaction scan cache across navigation, and opens from
  `/share?sessionId=<id>` on that transcript (#498, #536).
- Collective targets in the local API: list collectives and their linked
  repositories, add and remove shares, and serve publication state from the
  stored receipt with `outsideSelection` for sessions the saved selection
  leaves out. `/sync/push` accepts typed `collectives {add, remove}` and reports
  one result per step. A collective publish opens private and sends no license;
  removing a collective revokes its access (#497, #521).
- Automatic publishing: `hooks.yaml` binds a folder glob or a git remote to
  collectives, and a managed hook applies the binding on push. `peasant village
  auto` adds a rule for the repository and installs the hook; a bound push
  publishes private and shares only with the rule's collectives; a transcript
  that is public on Village is not updated. Installs are explicit, refuse
  unrecorded repositories, and keep foreign hooks intact with their remedy
  (#503, #530).
- Kickstart's one auto-publish question: "publish automatically?" records
  `push.autoPublishIntent` (with `push.sharePreference: share-later`), installs
  nothing, and lets the first publish popup offer automatic setup later (#9,
  #539).
- The local settings API: `GET /settings` returns every yaml-backed key with
  per-key metadata (`inPeasantConfig` derived from the terminal editor's
  registry); `PATCH /settings` accepts one key, validates the whole
  configuration, saves atomically, and applies the change live. Credentials are
  never returned (#501, #532).
- The local settings page: grouped rows with pending and settled feedback, a
  failed write restored with the API reason, the "not in peasant config" tag,
  explicit hook installation with per-repository disclosure, and rule editing
  that keeps every event (#502, #538).
- Home as a sessions-first page: a stats strip computed from the existing
  topics, search with turn-level matches, and one session list with a
  publish-state column, the all / not published / published / auto filters, a
  load-more control, and a link from every row into its project's review view.
  Selection notices and recovery are retained (#500, #537).
- A quiet local shell: a one-row header (`peasant`, search, settings, theme),
  fairtrade's offline notice with the start command and retry when the local
  server stops, route-only sections reachable by URL, and the tour unmounted
  (#499, #523).
- `peasant open` records one session and opens its transcript in the local web
  (#509).
- C4 architecture diagrams and call sequences in the documentation (#475), and
  the home-first local section registry documented (#511).

### Changed
- A web update keeps the audience of a published transcript: a collective-only
  transcript stays collective-only (#510).
- The local dashboard is served on loopback only and refuses requests that do
  not come from it (#508).
- Test-gate and CI performance work: a two-pass race-on budget gate, waits on
  signals and deadlines instead of fixed sleeps, a golden-template cache and
  store-open seam, the arm64 subset lane on releases only, and the GitHub Go
  cache skipped on the self-hosted pool (#522, #528, #534, #535).

### Fixed
- SQLite callback ownership is preserved across connection reuse (#541).
- Claude Code workflow subagent transcripts are ingested (#529).
- The staging arena wrap gap is released with its copy (#527).
- Code-map search short-circuits on a raw session id or project hash (#476).

## [0.7.0] - 2026-09-27

### Added
- Unknown harness data is retained under a per-harness record-kind registry. A
  well-formed kind this build does not recognize resolves to redacted opaque
  retained evidence instead of being dropped; refused kinds are aggregated by
  harness and kind in the harvest text and JSON reports; a generated
  `docs/record-kinds.md` documents the registry, and the retained evidence is
  certified through export and publication exactly when a placeholder accounts
  for it (#412, #455).
- Native repair activation through the harvester registry: a harness whose
  effective target is a managed generation is built, staged, and activated
  through the store instead of replacing bare entries, so repaired sessions keep
  their captured content and prior evidence; a store that cannot persist a
  managed generation keeps the retained baseline (#426).
- The generation activation records the publication-capture agreement in the
  same transaction as the managed-generation install, so a session repaired from
  a stale index is publishable immediately. An uncertifiable provenance kind
  records nothing and leaves stored provenance unchanged; an unchanged capture
  never moves; a changed capture advances its revision once; a disagreement
  refuses the activation (#429).
- Published payloads carry durable session provenance — relationships and their
  public anchors, the root session, the purpose, the input-submission count
  (including a measured zero), and retained earlier history — through the
  snapshot-first publish path, with the consent overlay and the metadata mirrors
  the receiver requires (#428).
- Mounted session navigation on the new detail surface: a stored context link
  opens the exact stored target, current-parent links navigate, and the retained
  earlier-history disclosure restores on Back, reload, and copied links without
  moving the stream position (#432).
- Grouped local browse and share: grouped local session lists on the home
  picker, grouped search and share flows, and a share chooser that selects
  explicit helper members (#426).
- `peasant push` scans payloads offline and negotiates receiver capabilities
  freshly before publishing (#426), and matches the prompt-request hint against
  either the base or the fork remote a request names (#461).
- The web app includes an inspect and feedback tool, development-gated and
  app-local (#422).
- `peasant -v` and `peasant --version` print the same version line as
  `peasant version` (#285, #442).

### Changed
- The durable session detail no longer carries the read-only navigation field;
  the viewer receives it as an adapter option, so sessions with relationships
  cook correctly (#432).
- The projects home no longer embeds the change graph (#425).
- The `changes` visual regression baselines were re-blessed (#427).

### Fixed
- A stored session origin is read past leading harness scaffolding, so a
  repaired-session origin is not misread from an unrecognized record (#452).
- Repair eligibility survives an interrupted pair install, so a session
  interrupted mid-repair is retried instead of being left settled incorrectly
  (#454).
- Session summaries and child-reference start times are emitted as UTC
  instants, so the grouped and flat session lists and the sync chooser decode
  against the Z-only wire contract on non-UTC hosts (#460).
- Review and upload read the committed publication inputs rather than a
  re-derived snapshot, so a published payload matches what was committed (#456).
- Published payloads derive the metadata publication mirrors — the
  input-submission count and the graph identity (root session, purpose,
  relationships) — from the same active generation snapshot as the durable
  detail; previously the metadata part could omit or diverge from those values,
  so a receiver could refuse an otherwise valid publish with a mirror
  disagreement (#433).
- Pi publications keep their recorded duration when publishing through the
  snapshot path; previously the duration was emitted as zero (#428).
- The generated Homebrew cask carries the frozen string literal comment, so the
  cask passes `brew style` (#477).

### Performance
- `peasant push` shares one lookup client and resolves the pushed repository
  once per run instead of twice (#453).

### CI
- CI calls the shared runner router, pulls the e2e images and the
  release-validate matrix from the project mirrors and publisher registries, and
  runs the Go suite on the self-hosted pool with a dedicated arm64 lane
  (#441, #445, #451, #474).
- The harvester version guard is routed to the runner pool, and the post-merge
  `make check` is skipped only with proven pull-request evidence (#473, #440).
- Release tooling: the release-PR gate re-runs only with a clear delta and
  passing evidence (#419); partial re-runs of a failed release are documented
  (#416); the release gate no longer runs the race detector (#414).
- The full-stack e2e harness runs RustFS as its S3-compatible object store in
  place of MinIO, whose official images were withdrawn from Docker Hub and Quay
  (#485).

### Dependencies
- Contract pins: schema `v0.24.0`, redact `v0.1.6`, fairtrade `0.0.20`. The
  full-stack e2e gate provisions the matching Village revision, so the release
  gate exercises provenance publication against a receiver that advertises the
  session-graph capability.

## [0.7.0-rc1] - 2026-09-16

### Added
- Native repair activation through the harvester registry: a harness whose
  effective target is a managed generation is built, staged, and activated
  through the store instead of replacing bare entries, so repaired sessions keep
  their captured content and prior evidence; a store that cannot persist a
  managed generation keeps the retained baseline (#426).
- The generation activation records the publication-capture agreement in the
  same transaction as the managed-generation install, so a session repaired from
  a stale index is publishable immediately. An uncertifiable provenance kind
  records nothing and leaves stored provenance unchanged; an unchanged capture
  never moves; a changed capture advances its revision once; a disagreement
  refuses the activation (#429).
- Published payloads carry durable session provenance — relationships and their
  public anchors, the root session, the purpose, the input-submission count
  (including a measured zero), and retained earlier history — through the
  snapshot-first publish path, with the consent overlay and the metadata mirrors
  the receiver requires (#428).
- Mounted session navigation on the new detail surface: a stored context link
  opens the exact stored target, current-parent links navigate, and the retained
  earlier-history disclosure restores on Back, reload, and copied links without
  moving the stream position (#432).
- Grouped local browse and share: grouped local session lists on the home
  picker, grouped search and share flows, and a share chooser that selects
  explicit helper members (#426).
- `peasant push` scans payloads offline and negotiates receiver capabilities
  freshly before publishing (#426).
- The web app includes an inspect and feedback tool, development-gated and
  app-local (#422).

### Changed
- The durable session detail no longer carries the read-only navigation field;
  the viewer receives it as an adapter option, so sessions with relationships
  cook correctly (#432).
- The projects home no longer embeds the change graph (#425).
- Release tooling: the release-PR gate re-runs only with a clear delta and
  passing evidence (#419); partial re-runs of a failed release are documented
  (#416); the release gate no longer runs the race detector (#414); x86_64 and
  architecture-neutral CI jobs run on the self-hosted runner pool (#424).
- The `changes` visual regression baselines were re-blessed (#427).
- The schema contract module is re-pinned to v0.22.0 and the full-stack e2e gate
  provisions the matching Village revision, so the release gate exercises
  provenance publication against a receiver that advertises the session-graph
  capability.

### Fixed
- Published payloads derive the metadata publication mirrors — the
  input-submission count and the graph identity (root session, purpose,
  relationships) — from the same active generation snapshot as the durable
  detail; previously the metadata part could omit or diverge from those values,
  so a receiver could refuse an otherwise valid publish with a mirror
  disagreement (#433).
- Pi publications keep their recorded duration when publishing through the
  snapshot path; previously the duration was emitted as zero (#428).

## [0.6.0] - 2026-09-14

### Added
- `peasant sessions list` accepts `--session <id>`, and its JSON output carries
  `projectHash`, so harness integrations such as the `/peasant` Claude Code
  plugin can record the current session and open its transcript deep link
  (#345).
- Pi Coding Agent sessions are discovered, indexed, redacted, exported, and
  published through the existing harvest and push paths (#329).
- Kickstart local import animates a progress bar for every stage, shows live
  per-stage timings and estimates, and quits on `q` or Ctrl+C (#303).
- `peasant village push` offers opt-in stage profiling and redaction metrics
  through `--profile-output` and `--profile-trace` (#315).
- Harvest reports measured index coverage, including sessions that indexed
  nothing and sessions that failed but kept prior entries, in the summary and
  JSON output (#394).
- Claude Code control records such as attachments, compaction boundaries,
  permission-mode changes, agent settings, PR links, cost snapshots, and titles
  are represented with their payloads instead of refused, so those sessions
  certify for export and publication (#411).
- Codex sessions keep their native content provenance and session hierarchy:
  block origin, actor, and ownership come from native evidence, and root,
  parent, and helper relationships are preserved instead of inferred from
  wrapper text (#401).
- Skill and slash-command invocations recorded at ingest are emitted on the
  wire as the turn's `Command` in session detail, export, and push (#362).

### Changed
- Each harness carries its own adapter, indexer, and index-format version, so
  a change to one harness re-indexes only that harness. Claude Code sessions
  re-index once after this upgrade (#343, #411).
- SQLite is the source of truth for saved sessions and publication metadata.
  Complete recorded text, tool inputs and outputs, and structured metadata
  persist in the database, and push, dry-run, wizard previews, and Share no
  longer need generated `metadata.json` files (#337, #338, #343).
- Active sessions are ingested by default, and repeated runs compare captured
  source evidence so unchanged sessions are skipped (#331).
- Explicit republication of a changed session updates the same Village
  transcript through its receipt without `--force` (#331).
- Publication eligibility validation is explicit at each consumer: the push
  pipeline, the wizard preview, and the Share redaction scan (#344).
- Transcript records up to 256 MiB are read whole; larger records are omitted
  with a diagnostic and a stored placeholder, and served detail stays under the
  contract cap (#343).

### Fixed
- Historical commit admission is preserved: the ingest-time branch
  reachability filter was reverted so a rewritten branch does not discard
  still-discoverable commits before ledger insertion (#334).
- OpenCode v2 and mixed v1/v2 stores ingest again, with native message
  decoding and live-session discovery across both layouts (#310).
- Harvest cancellation stops in-flight ingest and diff work, and harvest and
  kickstart share one progress model (#316).
- Kickstart shows filesystem paths for remote-less projects, drops the empty
  branch level for branchless projects, and unblocks selection there. Branch
  re-selection no longer narrows an unrestricted harness (#378, #391).
- Forced rebuilds no longer refuse intact sessions on a false checksum
  mismatch; transcript and metadata are read as one validated pair (#395).
- `--dry-run` opens the analytics database read-only instead of loading the
  whole file into memory (#406).
- Sessions stored before publication captures existed, including every
  session a v0.5.0 database holds, are captured again by an ordinary `peasant
  ingest` and can publish. The harvest previously settled such a session from
  its retained transcript pair, which cannot bind a capture, so `push` kept
  asking for an ingest that changed nothing (#413).

### Database
- Store migrations V50 through V61: captured-source fingerprints and
  source-proven capture provenance (V50, V51; #331, #337), durable prose
  separated from bounded previews (V52; #338), Pi harness admission (V53;
  #329), index representation, adapter output, input proofs, annotation
  retirement, and the closed `capture_format` column (V54 through V59; #343),
  and the managed-generation catalog with reverse logical-target indexes (V60,
  V61; #401).

### Build
- Release archives, `.deb`, `.rpm`, and the AUR PKGBUILD ship
  `THIRD_PARTY_NOTICES`, and CI guards dependency licenses (#366).
- Final releases publish the Homebrew cask to `peasant-labs/homebrew-tap`,
  verified by a macOS cask smoke job (#354).
- The Homebrew cask clears the download quarantine attribute through
  Homebrew's declarative `postflight_steps` stanza, which current `brew style`
  requires in place of a `postflight` block (#413).

### Tests
- End-to-end runs reap orphaned test containers whose owner process has exited
  and enforce a local memory cap (#331, #347).
- The Pi round-trip e2e test compiles against the schema v0.20.0 entry-ref
  type (#413).

### CI
- The release e2e gates check out the Village peer at a revision that pins the
  same schema contract, v0.20.0 (#413).
- The full-stack e2e pulls MinIO from Quay by digest, since the Docker Hub
  image is no longer served (#413).

### Dependencies
- Contract pins: schema `v0.20.0` (Village API 0.18.0, Local API 0.13.0,
  Types 0.20.0), redact `v0.1.6`, fairtrade `0.0.19` (#329, #361, #392).

## [0.5.0] - 2026-09-01

### Added
- `peasant upgrade` installs newer release artifacts from GitHub, protects raw
  archive installs with an explicit confirmation step, and reports safe
  downgrade guidance (#17, #269, #270).

### Changed
- Push sends repository labels and git remotes by default while keeping project
  paths as a fallback, and the push wizard and kickstart privacy guide use the
  same user-facing consent language (#224).
- The web transcript viewer now uses the shared Fairtrade transcript graph and
  helper components (#228).

### Fixed
- Harvest progress, OpenCode injected-turn classification, Claude meta-turn
  handling, and discovery filtering now match the intended recorded-session
  model (#217, #231, #253, #284).

### Performance
- Harvest indexing, annotation skip checks, and annotation persistence avoid
  repeated transcript and store work on warm runs (#230, #250, #255, #256,
  #273).

### Database
- Store migrations V47 through V49 add warm-index state, annotation run state,
  and durable annotation target anchors (#250, #254).

### Build
- Web build and validation now use Node.js 26 with pnpm workspace catalog
  policy (#235).

### CI
- Release packaging validation now runs only through release flows, and final
  releases no longer require a same-version release candidate (#280, #289).

### Dependencies
- Contract pins: schema `v0.1.3` (Village API 0.14.0, Local API 0.9.0,
  Types 0.14.0), redact `v0.1.5`, fairtrade `0.0.18` (#290).

## [0.5.0-rc3] - 2026-09-01

### Build
- Web build and validation now use Node.js 26 with pnpm workspace catalog
  policy, keeping frontend dependency resolution aligned with the release
  environment (#235).

### CI
- Release automation can cut a final release without first requiring a
  same-version release candidate, while keeping the existing guard, e2e, and
  package-validation gates (#289).

### Dependencies
- Contract pins: schema `v0.1.3` (Village API 0.14.0, Local API 0.9.0,
  Types 0.14.0) (#290).

## [0.5.0-rc2] - 2026-08-31

### Added
- `peasant upgrade` installs newer release artifacts from GitHub, refuses to
  replace a raw archive install until the user confirms it, and compares
  source-build versions with ordered development stamps (#17, #270).

### Fixed
- Upgrade refuses targets older than the current binary and recommends the
  latest stable release when it needs to show a safe downgrade command (#269,
  #270).
- OpenCode background-task result messages whose parts are all synthetic now
  ingest as system entries. Mixed messages with user-authored text stay user
  entries (#253).
- `peasant harvest` now shows live DIFF and FILTER progress, renders the
  progress block through Bubble Tea without flicker or leaked terminal replies,
  and prints a quieter summary with explicit duration and no unchanged detail
  rows (#284).

### Performance
- Annotation run-state skip checks now read the entry hash, compute version,
  metrics, and run state through one store lookup (#256).
- Annotation persistence batches writes, prefetches active annotations for
  deduplication, and keeps SQLite work behind one writer while COMPUTE and
  ANNOTATE can start as INDEX sessions finish (#255, #273).

### Database
- Store migration V49 adds `annotation_target_anchors`, so repaired annotation
  targets stay durable and unresolved targets fail closed instead of pointing at
  the wrong transcript entry (#254).

### CI
- Release packaging validation now runs through release flows only, so ordinary
  pull requests do not start snapshot packaging jobs while release PRs and rc
  tags still validate packages (#280).

## [0.5.0-rc1] - 2026-08-29

### Changed
- Push now sends the repository label (`host:owner/repo`, derived from the
  recorded git remote) and the git remote URL by default; the project path is
  sent only as a fallback for a project with no recognizable remote, in its
  canonical `/<PATH>/<project>` form, and is never paired with a label on the
  same publish. The three `push.fields` keys that gate this (`gitRemote`,
  `projectPath`, `projectName`) are now tri-state: an absent key defaults on,
  and an explicit `true` or `false` is kept exactly as written. `gitBranch` and
  `hostSlug` remain plain booleans defaulting off. `project.hash` is unaffected
  and remains always sent. The push wizard's consent screen and the kickstart
  privacy guide share one sentence describing this (#224).
- The web transcript viewer imports the shared transcript graph and helper
  components from fairtrade 0.0.18, so Peasant no longer keeps a separate graph
  engine path for transcript rendering (#228).

### Fixed
- Claude Code user entries marked `isMeta` are now treated as harness-injected
  turns during ingest, so they do not become user-visible transcript prompts
  (#217).
- Discovery lists hide sessions that have metadata but are not indexed yet. A
  direct session link still opens after the session is indexed (#231).

### Performance
- `peasant harvest index --all` parses INDEX work in parallel, streams eligible
  sessions from `drainLoop` into INDEX workers, keeps SQLite writes serialized,
  and avoids the transient SQLite lock storm seen on large copied corpora (#230,
  #250).
- Warm harvest runs skip unchanged `session_entries` rewrites with
  `sessions.session_entries_hash`, skip unchanged classifier annotation work with
  `annotation_run_state`, and batch classifier annotation writes (#250).
- `--profile-index` reports INDEX queue shape, write causes, stage timings, and
  annotation detail, and `docs/benchmarks/harvest-optimizations.md` records the
  copied-corpus benchmark method and results (#250).

### Database
- Store migrations V47 (`sessions.session_entries_hash`) and V48
  (`annotation_run_state`) support warm INDEX and annotation skip state (#250).

### Dependencies
- Contract pins: schema `v0.1.2` (Local API 0.9.0, Types 0.13.0), redact
  `v0.1.5`, fairtrade `0.0.18`.

## [0.4.0] - 2026-08-26

### Added
- Peasant decides at import who drove each recorded session (`user`, `agent`,
  or `unknown`) from evidence in the transcript, stores the verdict, and
  declares it on the wire as `sessionOrigin`. Agent-driven sessions (workers,
  reviewers, teammates, subagents) are hidden from discovery lists and from the
  kickstart picker, where a visible parent shows its child-session count. A
  direct link to a hidden session still opens it; hiding is discovery scope,
  never access control (#194, closes #71).
- Teammate sessions are re-parented to the session that spawned them when the
  identity pairing is unique, so a parent push carries its whole tree (#194).
- `GET /api/v1/session-summaries?ids=` resolves session links without any
  discovery scope (#194).
- OpenCode ingestion selects one canonical projection per session when the same
  session exists as JSON, legacy SQLite, and current SQLite (current, then
  legacy, then JSON). Discovery unions all three, freshness reads only the
  selected projection, and parents are emitted before children (#157, closes
  #127, #128, #179).

### Changed
- Project labels use the full host form (`github.com:owner/repo` instead of
  `github:owner/repo`) and are rendered by the shared schema rule, so peasant
  and village show byte-identical labels. Self-hosted forges keep their
  hostname (#195).
- The transcript session header condenses to its breadcrumb and actions row
  while the trace is scrolled, and restores at the top (fairtrade 0.0.16;
  #177, #181).
- Contract pins: schema `v0.1.2` (Local API 0.9.0, Types 0.13.0), redact
  `v0.1.3`, fairtrade `0.0.16`.

### Fixed
- Annotation broadcasts are drained on server shutdown, and closing the store is
  idempotent and race-safe. A background broadcast can no longer dereference a
  closed pool (#180, closes #178).
- Session titles now skip five more harness-injected first turns: the Codex
  plugin catalog and review-action envelope, a Claude Code agent message, the
  "Another Claude session sent a message:" turn that delivers one, and the
  "[Request interrupted by user" turn. A recompute of stored titles picks the
  change up (redact v0.1.3).
- A session title is taken from the first user turn that holds real user prose.
  A leading harness-injected turn (a slash command wrapper, local command output,
  a system reminder, an environment context block, or a skill body) is skipped,
  and a turn whose markup cannot be cleaned safely is never shown raw. This
  applies to the published title, the local display title, and the session
  heading in the web viewer, which now shows a plain placeholder when a session
  has no title yet (#175).

### Database
- Store migrations V45 (the OpenCode event-sequence change cursor) and V46
  (`sessions.session_origin` with a closed CHECK set, plus the origin evidence
  cache). Existing sessions receive an origin verdict on the next import.

## [0.3.0] - 2026-08-21

### Added
- Kickstart previews any discovered session before it is imported. The preview
  reads the harness transcript in place and writes nothing to disk or to the
  store (#160).
- The step tab strip scrolls to keep the active tab visible and marks overflow on
  each clipped side (#161).
- Kickstart shows the village login URL in the connecting spinner and in the
  standalone login, so a user without a browser on the machine can open it
  elsewhere (#151).
- The push wizard shows each transcript as it will be published, redacted at
  the configured level, in the selection preview (#164).

### Changed
- The village push wizard is rebuilt on the TUI kit: kit confirm prompts that
  open on `no`, the kit tree and preview split for selection, a scrollable
  consent panel, and a receipt that states what is pushed and that nothing is
  removed from this machine (#164).
- Terminal layout is centralized in the TUI kit. Panels paint their background
  behind every cell, so surfaces no longer end in ragged lines (#161).
- The kickstart privacy step uses lowercase example headings, a split pane with
  the examples beside the control, and wrapped option descriptions (#145).
- The kickstart final review groups settings under headed sections and shows a
  continue cue (#150).
- The village connect and login prompt is a heading with bullets (#149).
- The shared redaction scope sentence reads "known patterns" in sentence case
  on every surface (#164).

### Deprecated
- `peasant tui` is deprecated. Use `peasant web` for the dashboard, sessions,
  and trends, and `peasant annotate` for annotations. The command still runs and
  prints a notice. It will be removed after one release carries the notice
  (#167).

### Fixed
- Redaction placeholders such as `<EMAIL>` stay visible in rendered transcript
  previews. Markdown read them as HTML tags and dropped them (#170).
- The kickstart source-preview goldens follow the scrolled tab strip (#168).

### Performance
- Claude discovery caches the teammate evidence it mines from each transcript,
  keyed on path, size, and modification time. A rescan over unchanged
  transcripts reads no transcript content (#159).

## [0.2.1] - 2026-08-19

- OpenCode SQLite ingestion: safe source probing, legacy and current SQLite
  sessions.
- Kickstart first-run UX: interruptible village login, clearer selection step,
  explicit keep-local publication preference with a no-publish guard.
- Web UI update.

See the [v0.2.1 release](https://github.com/peasant-labs/peasant/releases/tag/v0.2.1).

## [0.2.0] - 2026-08-14

Second public release. See the
[v0.2.0 release](https://github.com/peasant-labs/peasant/releases/tag/v0.2.0).

## [0.1.0] - 2026-08-04

Initial public release. See the
[v0.1.0 release](https://github.com/peasant-labs/peasant/releases/tag/v0.1.0).

[0.13.5]: https://github.com/peasant-labs/peasant/releases/tag/v0.13.5
[0.13.4]: https://github.com/peasant-labs/peasant/releases/tag/v0.13.4
[0.13.3]: https://github.com/peasant-labs/peasant/releases/tag/v0.13.3
[0.11.1]: https://github.com/peasant-labs/peasant/releases/tag/v0.11.1
[0.11.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.11.0
[0.10.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.10.0
[0.9.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.9.0
[0.8.2]: https://github.com/peasant-labs/peasant/releases/tag/v0.8.2
[0.8.1]: https://github.com/peasant-labs/peasant/releases/tag/v0.8.1
[0.8.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.8.0
[0.7.0-rc1]: https://github.com/peasant-labs/peasant/releases/tag/v0.7.0-rc1
[0.6.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.6.0
[0.5.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.5.0
[0.5.0-rc3]: https://github.com/peasant-labs/peasant/releases/tag/v0.5.0-rc3
[0.5.0-rc2]: https://github.com/peasant-labs/peasant/releases/tag/v0.5.0-rc2
[0.5.0-rc1]: https://github.com/peasant-labs/peasant/releases/tag/v0.5.0-rc1
[0.4.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.4.0
[0.3.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.3.0
[0.2.1]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.1
[0.2.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.0
[0.1.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.1.0
