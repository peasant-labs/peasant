# Kickstart wizard

`peasant kickstart` mounts Peasant's project-first local-to-Village onboarding flow. It discovers
transcripts from the configured harness paths, lets you choose work by project, saves the reviewed
configuration, ingests the exact selected sessions, and can publish them to Village.

<!-- verified-example: kickstart-commands -->
```bash
peasant kickstart
peasant kickstart --reset
```

`--reset` removes Peasant configuration, credentials, database, ingested data, and state before
starting again. Without `--reset`, kickstart loads the resolved `--config` or `--config-dir` file and
changes that configuration. It preserves unrelated settings, custom source paths, and other loaded
values. Kickstart does not rebuild the file from defaults or silently write a different path.

## Restore saved choices

On a later run, kickstart restores each available saved project, branch, and explicit session choice.
It applies these saved choices after the first successful project-tree load. If the tree refreshes
during the same run, it keeps edits that you already made on the screen.

A project that Peasant finds for the first time starts clear. This is also true after you used
**Select all projects** on an earlier run. That action saves only the projects on the current screen.
It does not select projects that Peasant finds later.

A new branch in a selected project follows the saved `autoIngestNewBranches` value:

- `true` lets discovery include a branch that is not in the saved branch list.
- `false` keeps discovery on the saved branch list. A project choice with no branch list still means
  all branches in that selected project.

An exact branch exclusion still wins when `autoIngestNewBranches` is `true`. Clearing one available
branch records a denial for that physical clone and branch. It does not disable automatic discovery
for another branch or a sibling clone.

Kickstart keeps a saved project, clone path, branch, or session when the current scan cannot offer it
for editing. Another visible edit does not remove the unavailable choice. Kickstart removes an
available choice only after you clear it and save. If an unavailable branch belongs to an available
project, clearing that project and saving removes the branch with its parent.

## Physical clone identity

Peasant identifies a local clone with a resolved absolute physical path. It resolves symbolic links
before it compares or saves the path. A symbolic link and its target identify the same clone.

One Git remote entry can contain several `clonePaths`. One shared `branches` list applies to every
path in that entry. Peasant can use a Git remote or project name only when the current scan finds one
physical clone with that value. Peasant does not use a remote or name alone to choose between
ambiguous clones. An old selection entry without `clonePaths` stays readable and follows the same
uniqueness rule until kickstart migrates it from stored clone evidence.

For a project with no Git remote, Peasant uses the resolved path as identity. Kickstart shows the
project name with a short path when it must distinguish equal names. See the
[Selection index example](../README.md#selection-index) for the saved YAML shape.

## Exact exclusions

Each harness can store exact branch and session exclusions. Peasant applies positive project and
session rules first. It then applies these exact exclusions.

- A branch exclusion contains one resolved `clonePath` and one or more branch names. It does not
  exclude the same branch in another clone.
- A session exclusion contains one exact session ID. It still excludes that session when a project
  rule admits the session.
- Selecting the branch or session again removes its exclusion.

Harvest, viewer lists, the save gate, push, and `peasant prune --unselected` use the same matcher.
Direct links to stored history remain available. An exclusion changes discovery. It does not delete
local data.

## Convert old pathless selected rules

An old `selection.mode: selected` entry can identify a project by name or Git remote without a
`clonePaths` list. On the next kickstart run, Peasant compares that entry with sessions in the local
store. It migrates every matching stored physical clone to exact paths. A matching clone found only
by the current scanner starts clear.

When several saved rules apply to one matching stored clone, Peasant unions their branch lists,
removes duplicates, and sorts the result. For example, `[dev, main]` plus `[main, release]` becomes
`[dev, main, release]`. Disjoint lists such as `[main]` and `[release]` retain the clone with both
branches. If any matching rule has no branch list, the migrated clone remains unrestricted. An
already path-bound rule for that stored clone also contributes its saved branches while kickstart
canonicalizes the cohort.

Same-remote Git clones share one entry only when their resulting branch unions are equal. Unresolved
stored sessions remain exact session choices only when the canonical positive matcher admits them
after exact exclusions.

This migration runs in memory before the project editor opens. Peasant writes the exact paths only
after you confirm the final save. No, Back, quit, or cancel leaves the old file unchanged.

## Convert an old all-projects setting

An old `selection.mode: all` value contains no exact project list. On the next kickstart run, Peasant
prepares an exact `selected` list in memory. It uses sessions that are in the local store at the time
of that run.

- A scanned project starts selected only when Peasant can match it to a current stored session by
  harness and resolved physical path.
- A newly scanned project with no matching stored session starts clear.
- A stored session whose project is not available in the scan stays saved as an explicit session ID.
- The saved `autoIngestNewBranches` value does not change.

The conversion does not write the config when the screen opens. It writes only after you review and
confirm the final save. If you decline or cancel, the file stays unchanged and the conversion runs
again on the next kickstart run. Peasant does not have an old project snapshot. It cannot reconstruct
which projects existed when the old setting was written.

## Save with no effective project

Before the final write, kickstart checks the current selection against evidence from the current
discovery scan and the local session store. It asks for confirmation only when no effective project
and no available selected child session remain. A selected child with canonical project evidence
makes its parent project effective. A selected explicit session that is already in the local store can
also suppress the warning even when the discovery scan cannot find its project or path. Peasant keeps
its exact descendant identity and does not guess a project identity. A saved choice that is absent
from both the scan and the local store stays in the config, but it cannot suppress the warning or
create an unknown project row.

The confirmation states these effects:

- Existing projects stay ingested and indexed.
- The web viewer does not list them.
- They are not available for a future push until you change the selection.
- Peasant does not delete data.

The No choice is selected by default. Kickstart asks again on every run that reaches this state.
Choosing No or Back returns without a write. If you cancel, kickstart exits without a write. These
paths do not start ingest. Yes saves the no-project choice through the normal atomic config write. It
does not delete existing data.

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
4. Choose whether a new branch in a selected project can be included later.
5. Review the Standard redaction explanation and choose the schema-owned content license.
6. Optionally update Claude Code transcript retention.
7. Choose local-only, private Village publication, or public Village publication. Private is the
   default. Public requires acknowledgement that downloaded copies cannot be recalled and Creative
   Commons grants remain irrevocable.
8. Review the complete selection and give final consent.
9. Run the ordered journey and review its completion receipt or exact retry targets.

## Viewer lists and stored data

Home, Map, and the command palette show the same project list. If one available child session is
selected, its stored parent project appears in that list. Showing the parent does not select its
sibling sessions. The normal push chooser still offers only sessions that the saved selection admits.

When the saved selection hides every stored project, Home and Map show one recovery panel. The panel
shows aggregate hidden project and session counts. It does not show project names, session IDs, or
paths. It states that the data stays ingested and indexed, is absent from the web viewer, and is not
available for a future push. It also states that Peasant did not delete data. The panel provides a
copyable `peasant kickstart` command. A store with no recorded projects keeps the first-use ingest
message instead.

Selection scopes discovery, viewer lists, and future push choices. It is not an access-control rule
for stored history. A direct link to a stored session or canonical project still resolves. Saving a
narrow selection does not delete files or database rows, and it does not unpublish Village copies.

`peasant prune --unselected` is a separate destructive command. It requires
`selection.mode: selected`. It checks harness, project, clone, branch, explicit session, and exact
exclusion rules. A stored session stays only when the complete saved selection admits its recorded
branch and exact identity.

Preview the deletion first. The second command shows the deletion preview and asks for confirmation
before it permanently deletes the listed sessions from the local database and filesystem.

<!-- verified-example: unselected-prune-commands -->
```bash
peasant prune --unselected --dry-run
peasant prune --unselected
```

Kickstart never runs this command. Changing the selection does not delete stored data.

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

On the transcript-selection step, `Ctrl+l` moves input to the preview pane beside the tree
and `Ctrl+h` moves it back; the divider marks which side is active. While the preview holds
input, the movement keys scroll it instead of moving the tree cursor. The preview renders
the session's first recorded message as markdown, with fenced code syntax-highlighted.

See also:

- [Kickstart CLI reference](cli/peasant_kickstart.md)
- [TUI keyboard reference](TUI.md)
