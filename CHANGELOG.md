# Changelog

All notable changes to Peasant are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and Peasant follows
[Semantic Versioning](https://semver.org/). Each version links to its GitHub
Release, which holds the signed artifacts and checksums.

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

[0.3.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.3.0
[0.2.1]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.1
[0.2.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.2.0
[0.1.0]: https://github.com/peasant-labs/peasant/releases/tag/v0.1.0
