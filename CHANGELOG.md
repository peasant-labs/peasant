# Changelog

All notable changes to Peasant are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Peasant follows
[Semantic Versioning](https://semver.org/). Each version links to its GitHub
Release, which holds the signed artifacts and checksums.

## [Unreleased]

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

[0.5.0-rc1]: https://github.com/peasant-labs/peasant/releases/tag/v0.5.0-rc1
[0.4.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.4.0
[0.3.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.3.0
[0.2.1]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.1
[0.2.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.0
[0.1.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.1.0
