# Guided TUI screenshot harness

This manually invoked harness renders deterministic visual evidence for Peasant's guided setup UI.
It mounts the real `settings.Flow`, `kickstart.Program`, `push.PushWizardModel`,
`pullprogress.Model`, and `discoveryprogress.Spinner` production paths
over synthetic fixture data, composes their ANSI output into labeled contact sheets with Lip Gloss, and asks
[Freeze](https://github.com/charmbracelet/freeze) to rasterize the sheets.

The harness is guarded by the `guided_screenshots` build tag. It is not compiled or run by
`go test ./...`, `make check`, or a production Peasant build.

## Run

Freeze is provided by the workspace Nix development shell:

```bash
nix develop
make guided-screenshots
```

The command requires a clean Peasant worktree so the directory name honestly identifies the source
revision. While developing the harness itself, generate explicitly marked disposable evidence with:

```bash
go run -tags=guided_screenshots ./cmd/peasant-guided-screenshots --allow-dirty
```

Run the opt-in fixture and mounted-render tests with:

```bash
make guided-screenshots-test
```

The eight PNGs are written to:

```text
out/test/screenshots/peasant-guided-final-<commit>/guided-dark.png
out/test/screenshots/peasant-guided-final-<commit>/guided-light.png
out/test/screenshots/peasant-guided-final-<commit>/selection.png
out/test/screenshots/peasant-guided-final-<commit>/push.png
out/test/screenshots/peasant-guided-final-<commit>/ingest-progress.png
out/test/screenshots/peasant-guided-final-<commit>/ingest-completion.png
out/test/screenshots/peasant-guided-final-<commit>/pull-progress.png
out/test/screenshots/peasant-guided-final-<commit>/discovery-progress.png
```

An explicit dirty capture uses `peasant-guided-final-<commit>-dirty` instead. Generated evidence is
local and gitignored.

## Capture contract

`testdata/captures.yaml` is the strict source of capture scenarios. It pins:

- all seven guided sections in dark and light themes at `80x24` and `120x40`;
- the default and retained global-search selection states at both sizes;
- project, branch, and scrubbed transcript preview states in both themes at both sizes;
- the start, selection, consent, receipt, and keyboard help screens of the push wizard, in both
  themes at both sizes, over a synthetic candidate inventory that carries every redaction state and one session the
  branch-aware selection withheld;
- local ingest progress and completion, including multiple nonfatal warnings, scrolled remediation
  and next steps, and a no-warning control, in both themes and both sizes;
- the bulk collective-pull progress bar — running, canceling, and canceled — mounted from the real
  `pullprogress.Model` over scrubbed synthetic counts, in both themes and both sizes;
- the inline discovery spinner — mid-run and complete file counts with required re-reads, and an adapter
  that does not report progress — in both themes and both sizes;
- synthetic discovery data, required names and text, and exact matrix coverage; and
- the eight accepted contact-sheet names and pixel dimensions.

Unknown fields, trailing YAML documents, duplicate rows, and missing required names or matrix entries
fail before any PNG is published. No local config, transcript, repository, or credential
data enters the screenshots.

Peasant emits terminal cells and ANSI styling but does not control an end user's terminal font.
Freeze's bundled JetBrains Mono gives the fallback renderer a pinned cell geometry, but the version
bundled with Freeze 0.2.2 has no Braille spinner glyphs and renders them as missing-glyph boxes.
For a spinner review, use Freeze's optional `rsvg-convert` backend with a Braille-capable monospaced
font configured through fontconfig, and record the backend and font with the evidence. The font's
cell advance must match the bundled face (0.6 em); a narrower face overlaps the capture columns.
Inspect the glyph itself; the ANSI text assertion alone cannot prove that the PNG rendered it. The font and
optional backend are not pinned by this harness, so their exact paths or versions must be recorded.
