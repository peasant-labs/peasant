# Kickstart wizard

`peasant kickstart` mounts Peasant's project-first local-to-Village onboarding flow. It discovers
transcripts from the configured harness paths, lets you choose work by project, saves the reviewed
configuration, ingests the exact selected sessions, and can publish them to Village.

```bash
peasant kickstart
peasant kickstart --reset
```

`--reset` removes Peasant configuration, credentials, database, ingested data, and state before
starting again. Without `--reset`, kickstart loads the resolved `--config` or `--config-dir` file and
mutates that configuration. Unrelated settings, custom source paths, and other loaded values are
preserved; kickstart does not rebuild the file from defaults or silently write a different path.

## Mounted flow

Discovery diagnostics run before the interactive pages. Each operational harness reports its
discovered count. A failed or unavailable harness reports the real problem and cannot be selected;
it is not presented as an honest zero-session result.

The mounted journey is:

1. Choose Village authentication or stay local.
2. Select projects in the two-pane project picker. Untracked projects are on the left, tracked
   projects are on the right, and one search field remains above both panes. Sessions from different
   harnesses share one project row when they have the same canonical project identity. Kickstart
   focuses the project containing the invocation directory when it can identify one.
3. Narrow scope in one two-column page. The left column is **Project -> Branch -> Session** and the
   right column is the single global **Harnesses** filter. Harness filtering intersects the selected
   project scope; it does not erase or widen branch/session choices.
4. Choose whether newly discovered branches in fully selected projects should be included later.
5. Review the Standard redaction explanation and choose the schema-owned content license.
6. Optionally update Claude Code transcript retention.
7. Choose local-only, private Village publication, or public Village publication. Private is the
   default. Public requires acknowledgement that downloaded copies cannot be recalled and Creative
   Commons grants remain irrevocable.
8. Review the complete selection and give final consent.
9. Run the ordered journey and review its completion receipt or exact retry targets.

Changing project selection controls future discovery. It does not delete already stored transcripts
and does not delete or unpublish Village copies.

## Redaction policy

Standard is the only onboarding redaction policy. Kickstart never offers Minimal or Maximum.

- A loaded legacy Minimal setting is raised to Standard with disclosure.
- A direct request to run onboarding with Minimal is refused.
- Maximum is unsupported and is refused with an actionable instruction to set
  `redaction.level: standard` before ingest, publication, or hook installation.

Standard matching is best effort. Publication applies the production Standard boundary to the
outgoing metadata and transcript content while preserving the local indexed recording. Village's
server-side secret scan is an additional rejection boundary, not a claim that all identifying data
can be detected.

## Ordered execution and progress

Final consent starts one cancellable production journey in this order:

1. atomically save the reviewed loaded configuration, rejecting concurrent edits;
2. ingest, index, and store the exact selected sessions;
3. publish those sessions when Village was selected;
4. read complete authoritative receipts from local storage; and
5. process explicitly supplied hook work last. The mounted wizard currently supplies none, so this
   stage skips; hook lifecycle remains the standalone CLI described below.

The ingest stage streams the existing detailed production progress in the same execution view:
Discover, Diff, Filter, Extract, DB Insert, Index, Compute, Annotate, Cleanup, and Report. This is a
view of the ordered runner's single ingest execution, not a second ingest page or a replay.

Cancellation stops work that has not started and waits for the active operation to return its honest
persisted effects. A later failure never claims to roll back an earlier durable effect. Retry resumes
only the failed stage and exact session IDs, or the exact repository/event target, without replaying
completed publication. Completion displays authoritative Village URLs and applied visibility from
the stored receipt rather than echoing requested values.

## Current standalone boundaries

Kickstart does not currently mount either of these lifecycle UIs:

- Repository upload-hook installation, status, and removal use the explicit
  `peasant village hooks install|status|uninstall` CLI. Peasant does not overwrite unowned hooks or
  change `core.hooksPath`.
- Exact-preview local transcript deletion uses the standalone `peasant prune` CLI. Stopping future
  tracking in kickstart is configuration only and never prunes data.

Other confirmed deferred work is SQL-backed desired Village state plus an interactive
`peasant village config` editor, the broader schema-owned scriptable lifecycle API (including
organization access), selectable all-configured-project hook scope, and persistent cross-process
wizard resume.

## Keyboard and related commands

Use arrow keys or `j`/`k` to move, space to toggle, enter to confirm, and tab to move between the
scope columns. `b` goes back, `r` restarts before execution, and `q` or Ctrl+C quits/cancels. The
scope tree also supports left/right (or `h`/`l`) to collapse/expand and `f` to search.

See also:

- [Kickstart CLI reference](cli/peasant_kickstart.md)
- [TUI keyboard reference](TUI.md)
