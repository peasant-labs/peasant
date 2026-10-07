# Changelog

All notable changes to Peasant are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Peasant follows
[Semantic Versioning](https://semver.org/). Each version links to its GitHub
Release, which holds the signed artifacts and checksums.

## [Unreleased]

## [0.9.0] - 2026-10-07

### Added
- Native Windows support on amd64. Each release now publishes
  `peasant_<version>_windows_amd64.zip` (the executable with its LICENSE, README
  and third-party notices) and the bare `peasant_<version>_windows_amd64.exe`,
  both listed in `checksums.txt`. Peasant discovers Claude Code desktop sessions
  below `%USERPROFILE%\.claude\projects` and decodes their drive-letter project
  names, serves the dashboard without opening a console window for each
  subprocess it runs, and `peasant upgrade` replaces its own running executable
  by moving it aside rather than writing over it. A `windows-latest` CI job and a
  post-publish `windows-smoke` release job cover the platform. Installation is
  documented in `docs/install/windows.md`; Windows arm64 is not published (#480).
- `peasant reclaim` removes the managed generations a session no longer reads:
  their rows in one transaction, then their blob directories through the
  ownership-verified cleanup path. The active generation and any session with a
  pending activation are left alone, and an interrupted run is safe to repeat
  (#559).
- A cross-platform support guide recording how Linux, macOS and Windows are
  maintained from one module: the build-tag file split, the path-handling rules,
  the Windows-specific behaviour, and a checklist for adding platform-specific
  code (#563).

### Changed
- Durable full-content writes prepare their manifest and chunk inserts once per
  transaction instead of once per row. One full-content write of a fresh
  2,000-entry session dropped from 93.7 ms to 71.0 ms, and its allocations from
  75,482 to 37,998 (#556).
- The managed-generation install prepares each per-row insert once per install
  instead of once per row, removing repeated statement parsing from the largest
  tables in the database (#558).

### Fixed
- Publication repair no longer selects captures that were never recovered, and
  pair-damaged sessions are handled by the pair-repair pass alone, so the two
  repair paths no longer churn the same sessions (#557).

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
