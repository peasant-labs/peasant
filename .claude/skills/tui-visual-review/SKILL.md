---
name: tui-visual-review
description: Use for every Peasant terminal UI change that affects mounted layout, hierarchy, copy, focus, themes, previews, row annotations, search, facets, forms, or navigation. Runs the guided screenshot gates, rasterizes deterministic ANSI states with Freeze, and requires manual visual self-review of attributable evidence.
---

# TUI Visual Review

Use this skill for every user-visible terminal UI change. Read `AGENTS.md`, `CONTRIBUTING.md`, and
the "Guided TUI screenshot harness" section of `TESTING.md` first.

## Required Tool

[`charmbracelet/freeze`](https://github.com/charmbracelet/freeze) is required. The harness must use
the `freeze` executable to rasterize its deterministic ANSI contact sheets; output from another
terminal renderer does not satisfy this skill.

Peasant's Nix development shell provides the pinned executable through the `charm-freeze` package.
Run the workflow through `nix develop`, or set `FREEZE_PATH` to the intended Freeze executable when
testing that explicit boundary. If Freeze cannot be found or executed, the visual-review gate is
blocked. If `freeze` is not found on `PATH` and `FREEZE_PATH` does not name an executable, stop the
workflow and report the blocker to the user. Do not install a renderer, skip rasterization, silently
substitute another tool, or claim that visual review passed.

This workflow has three distinct gates:

1. Semantic and mounted tests prove behavior.
2. ANSI goldens and the screenshot harness prove deterministic rendering and fixture coverage.
3. Manual inspection of Freeze PNGs proves the rendered interface is visually usable.

None substitutes for another. A successful screenshot command is not visual self-review.

## Applicability

Run this workflow when a change affects any mounted TUI surface, including:

- hierarchy, ordering, labels, annotations, or expansion in a tree or list;
- preview content, focus, scrolling, or pane proportions;
- search, filters, facets, selection state, or empty/loading/error states;
- guided fields, review screens, help/footer text, or keyboard affordances;
- terminal sizing, wrapping, clipping, spacing, color, or theme behavior; or
- any production wiring that changes which TUI component users see.

Backend-only changes with no rendered effect do not require screenshots. When uncertain, run the
workflow.

## Evidence Model

The harness at `cmd/peasant-guided-screenshots/` mounts the real `settings.Flow` and
`kickstart.Program` production presentation paths over strict synthetic fixture data. It does not
read local transcripts, configuration, credentials, or repository history.

The strict fixture is `cmd/peasant-guided-screenshots/testdata/captures.yaml`. Before running a
capture, verify that it explicitly contains the changed mounted state, representative data, both
supported themes, and both `80x24` and `120x40` terminal sizes.

If the changed state or a required theme/size is absent, extend the YAML fixture, its row-count
guards, and the mounted harness tests first. Do not claim visual coverage from an unrelated state.
Combinatorial capture cases belong in the YAML fixture, never inline in a Go test.

The screenshot harness proves presentation only. Synthetic paths may exercise a scanner fallback,
not real Git topology or filesystem integration. Keep separate production-path integration tests
for behavior that depends on Git, storage, networking, or other external boundaries.

## Extending Contact Sheets

Add a capture whenever the existing sheets do not show the exact screen, cursor position, preview,
focus state, or overflow behavior under review. Extend the mounted harness rather than constructing
ANSI text or calling a presentation component outside its production parent.

For another guided section or selection state:

1. Add scrubbed representative data and a state row to
   `cmd/peasant-guided-screenshots/testdata/captures.yaml`. Give `wantContains` markers that prove the
   intended row is highlighted and the intended preview or body is visible; generic shell text is
   insufficient.
2. Add the state's strongly typed constant and validation in
   `cmd/peasant-guided-screenshots/fixture.go`. Include it in the required-state list and make
   `requiresBothThemes` return true for every user-visible state being reviewed.
3. Add the exact capture matrix in `selectionCaptures` or `guidedCaptures`. Capture both dark and
   light themes at `80x24` and `120x40` for a changed surface. Use the canonical generated name,
   such as `selection-<state>-<theme>-<width>x<height>`.
4. Increment both the Go `required*Count` guard and matching YAML `expected*Count` declaration.
   The selection capture count grows by four for a state captured in both themes and sizes. Never
   weaken or remove the strict count, name, marker, or cross-product checks.
5. If added rows need a taller PNG, update the sheet viewport in `captures.yaml` and its exact
   expected dimensions in `validateSheets` together. Leave enough height for every full terminal
   pair and label; do not crop a row to retain an old dimension.
6. Drive the state in `renderSelectionCapture` or `renderGuidedCapture`, run the mounted tests, then
   regenerate and inspect the entire contact-sheet set.

If the screen belongs to neither existing sheet kind, add a typed `sheetName` and `sheetKind`, a
strict fixture shape and validator, a renderer that mounts the real production parent, and a composer
case in `renderSheets`. Pin the new sheet's name, dimensions, row count, markers, themes, and terminal
sizes before publishing it. Do not use an ad hoc script or an unvalidated fourth PNG.

### Moving The Selection Cursor

`renderSelectionCapture` begins on the project row after declining optional login. Move through the
real `kickstart.Program` with Bubble Tea key messages:

- zero `j` presses keep the project selected;
- one settled `j` press selects its branch; and
- two settled `j` presses select the first visible session in the current fixture.

Continue one settled `j` press per visible row to reach a later session. Subagent listings are
summarized by their parent and therefore are not separate cursor stops. If fixture ordering changes,
recount from the rendered project-first tree and update the state's strict row and preview markers.
Do not assume that a raw listing's index equals its visible cursor index.

Send each key through `sendProgramMessage`, for example:

```go
program = sendProgramMessage(program, tea.KeyPressMsg{Code: 'j', Text: "j"})
```

That helper drains the cursor-triggered asynchronous preview command before the next key or capture.
Use the same path for `/` search input and `Ctrl+L` preview focus. Never mutate private tree cursor
fields, replace the mounted program with a test-only view, or capture before the returned commands
settle. A session-preview state must provide scrubbed synthetic turns through the normal
`NewListingPreview` loader and assert distinctive transcript text in `wantContains`; a title or
session ID alone does not prove that the transcript preview loaded.

## Development Capture

From the exact feature worktree under review, run:

```bash
nix develop --command make guided-screenshots-test
nix develop --command go run -tags=guided_screenshots ./cmd/peasant-guided-screenshots --allow-dirty
```

The first command must pass before trusting an image. The second command creates disposable
development evidence under:

```text
out/test/screenshots/peasant-guided-final-<commit>-dirty/
```

The `-dirty` suffix is mandatory for uncommitted source. Dirty captures are suitable for iterative
self-review only. They are not final commit or PR evidence.

Freeze is supplied by the Nix shell. If it is unavailable, enter the shell or set `FREEZE_PATH` to
the intended Freeze executable. Do not silently substitute another rasterizer.

## Manual Self-Review

Open every generated PNG, not just the one that looks most relevant. Agents should use the image
read/view capability and inspect the full-resolution files. The standard set is:

```text
out/test/screenshots/peasant-guided-final-<commit>[-dirty]/guided-dark.png
out/test/screenshots/peasant-guided-final-<commit>[-dirty]/guided-light.png
out/test/screenshots/peasant-guided-final-<commit>[-dirty]/selection.png
```

Inspect every changed state in both themes and at both terminal sizes. If the current selection
sheet does not contain both themes for a changed selection surface, extend the harness before
accepting the change.

Check all of the following:

- the changed mounted state is present and populated with representative fixture data;
- project, branch, session, or field hierarchy is understandable and correctly ordered;
- previews describe the highlighted row and do not show stale content;
- search, facet, checkbox, imported/tracked, focus, and overflow indicators remain legible;
- content does not clip, overlap, wrap into adjacent chrome, or disappear at either size;
- narrow layouts retain the primary action and enough context to understand the screen;
- dark and light themes have readable contrast and no missing style runs;
- pane dividers, focus markers, footer hints, headings, and full-line styles align correctly;
- no panel is unexpectedly blank, stale-looking, or dominated by fallback text; and
- screenshots contain only scrubbed fixture data, never personal paths or transcript content.

Record concrete observations, including problems found and the changes made to resolve them. If a
finding changes code or fixtures, regenerate and inspect a fresh complete set. Never hide an element
from captures or weaken a fixture marker to make the gate pass.

When sharing a related local set, use one absolute-path expression so every expansion exists:

```text
/absolute/worktree/out/test/screenshots/peasant-guided-final-<commit>[-dirty]/{guided-dark,guided-light,selection}.png
```

## Clean Revision Evidence

After implementation, tests, and self-review are complete, commit the intended changes. Confirm the
worktree is clean and generate attributable evidence:

```bash
git status --short
git rev-parse --short=8 HEAD
nix develop --command make guided-screenshots-test
nix develop --command make guided-screenshots
```

The output directory must be `peasant-guided-final-<HEAD>` with no `-dirty` suffix. Verify the
printed paths and inspect the complete clean set again; a prior dirty inspection does not prove the
committed bytes.

Generated PNGs are local and gitignored. Do not commit them. For an interface-changing PR, upload
the inspected clean-revision screenshots to durable GitHub-hosted PR evidence and link them from the
PR body or a linked PR comment. A local path alone does not satisfy review evidence because remote
reviewers cannot access it.

Record with the PR or task:

- exact commit hash and branch;
- the one-line absolute local path expression;
- surfaces, themes, and terminal sizes inspected;
- concise visual observations and any fixed regressions; and
- the durable GitHub evidence URL.

## Failure Handling

- If `guided-screenshots-test` fails, fix the mounted implementation or truthful fixture assertion.
- If the changed state is absent, add it to `captures.yaml`; do not rely on a nearby state.
- If Freeze or PNG dimension validation fails, fix the tool/environment or harness and rerun.
- If the worktree is dirty during final capture, commit first; `--allow-dirty` cannot create final evidence.
- If an image reveals a regression, fix it and repeat tests, generation, and inspection for the whole set.
- If durable PR upload is unavailable, report the blocker instead of claiming the visual gate passed.

## Completion Checklist

- [ ] Changed mounted states are explicit in strict YAML fixtures.
- [ ] Both themes and `80x24`/`120x40` are represented for each changed surface.
- [ ] `make guided-screenshots-test` passes.
- [ ] Dirty development captures were generated and manually inspected when iterating.
- [ ] All visual findings were fixed and a fresh complete set was inspected.
- [ ] Clean commit-attributed captures were generated after landing the local commit.
- [ ] Every clean PNG was manually inspected at full resolution.
- [ ] Exact provenance, paths, observations, and durable PR evidence were recorded.
