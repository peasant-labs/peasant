# Guided TUI screenshot harness

This manually invoked harness renders deterministic visual evidence for Peasant's guided setup UI.
It mounts the real `settings.Flow` and `kickstart.Program` production paths over synthetic fixture
data, composes their ANSI output into labeled contact sheets with Lip Gloss, and asks
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

The three PNGs are written to:

```text
out/test/screenshots/peasant-guided-final-<commit>/guided-dark.png
out/test/screenshots/peasant-guided-final-<commit>/guided-light.png
out/test/screenshots/peasant-guided-final-<commit>/selection.png
```

An explicit dirty capture uses `peasant-guided-final-<commit>-dirty` instead. Generated evidence is
local and gitignored.

## Capture contract

`testdata/captures.yaml` is the strict source of capture scenarios. It pins:

- all five guided sections in dark and light themes at `80x24` and `120x40`;
- the default and retained global-search selection states at both sizes;
- synthetic discovery data, row-count guards, required text, and exact matrix coverage; and
- the three accepted contact-sheet names and pixel dimensions.

Unknown fields, trailing YAML documents, duplicate rows, missing matrix entries, and changed count
declarations fail before any PNG is published. No local config, transcript, repository, or credential
data enters the screenshots.

Freeze's bundled JetBrains Mono is deliberate. Peasant emits terminal cells and ANSI styling but does
not control an end user's terminal font; the bundled face gives this evidence a pinned cell geometry
without adding a browser or a host-font dependency.
