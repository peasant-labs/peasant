# Cross-platform support

Peasant builds one Go module for Linux, macOS, and Windows. This document records the
architecture and the patterns that keep the targets honest — the build-tag split, the
path rules, the Windows-specific behavior, and the CI and release gates that prove it.
Read it before adding platform-specific code, and update it when the split changes.

## Supported platforms

| OS | Architectures | Install guide |
|----|---------------|---------------|
| Linux | amd64, arm64 | [ubuntu.md](install/ubuntu.md), [arch.md](install/arch.md), [nix.md](install/nix.md) |
| macOS | amd64, arm64 | [macos.md](install/macos.md) |
| Windows | amd64 | [windows.md](install/windows.md) |
| Windows via WSL2 | the distro's Linux binaries | [wsl.md](install/wsl.md) |

Windows arm64 is deliberately unpublished: no runner tests it and no consumer asks for
it. `.goreleaser.yml` ignores `windows/arm64`.

## The platform-split pattern

Platform-specific code lives in sibling files selected by a build tag. Two shapes are in
use:

- **`//go:build unix` / `//go:build windows`** — a real implementation on each side.
- **`//go:build unix` / `//go:build !unix`** — the unix implementation and a fallback for
  everything else (used where only a unix-vs-everything distinction matters, e.g. a test
  probe).

Rules that keep the split reviewable:

- **Identical signatures.** A caller in a shared file compiles against either sibling.
- **The shared file stays platform-neutral.** It must never reference a type that exists
  on only one target. The motivating incident: a `internal/store` test referenced
  `syscall.Stat_t`, which Windows does not have, and the whole test package failed to
  compile there while every Linux gate stayed green.
- **The unix side must not change behavior.** A platform extraction reproduces the
  previous code byte-for-byte on unix; the Windows side is additive.

| Facility | unix | windows | Files |
|----------|------|---------|-------|
| Advisory file lock (session) | `flock(2)` | `LockFileEx` / `UnlockFileEx`, one-byte range | `internal/store/session_lock_{unix,windows}.go` |
| Advisory file lock (shared package) | `flock(2)` | `LockFileEx` | `internal/filelock/flock_{unix,windows}.go` |
| Directory fsync after a rename | `open(dir)` + `Sync` | no-op (no directory flush exists) | `internal/{auth,config,store}/syncdir_{unix,windows}.go` |
| Background server detach | `Setsid` | `CREATE_NEW_PROCESS_GROUP \| DETACHED_PROCESS` | `cmd/peasant/procattr_{unix,windows}.go` |
| Stop a process | `SIGTERM` | `os.Process.Kill` | `cmd/peasant/terminate_{unix,windows}.go` |
| Hide child consoles | no-op | `CREATE_NO_WINDOW`, merged with `\|=` | `internal/proc/noconsole_{unix,windows}.go` |
| Socket-error classification | unix errno | unix errno + Winsock `WSA*` | `internal/api/winsock_{unix,windows}.go` |
| Replace the running executable | `os.Rename` | displace-and-install | `cmd/peasant/upgrade_replace_{unix,windows}.go` |
| Module-install process group | `Setpgid` + `kill(-pgid)` | `CREATE_NEW_PROCESS_GROUP` + `Kill` | `scripts/check-module-install/procgroup_{unix,windows}.go` |
| Test probes (inode, file mode, test home, pty, winsize, rusage, FS probe, process alive) | real | skip or stub | `internal/{config,store,defaults,testkit}/*_{unix,windows,other}_test.go`, `cmd/peasant/*_{unix,windows}_test.go` |

## Path handling

A session recorded on one OS is routinely read on another, so path handling cannot assume
the host.

- **Never hardcode `/` in a comparison.** Use `string(filepath.Separator)` or normalize
  both sides with `filepath.ToSlash`. A hardcoded `/` silently fails only on Windows.
- **`filepath.Clean`/`Abs`/`Rel` are native.** `Clean` replaces `/` with `\` on Windows;
  `Rel` compares path elements case-insensitively there (`sameWord` = `EqualFold`).
- **Judge an absolute path by its own shape, not the host's.** `filepath.IsAbs` answers for
  the running platform, so a Windows recording is "relative" on Linux and vice versa. Use
  `hasAbsolutePathForm` (`internal/ingest/utils.go`).
- **Claude project slugs decode from a platform-shaped root.** A unix slug leads with one
  dash and decodes from `/`; a Windows slug leads with a drive letter and two dashes
  (`C--Users-alice-project` for `C:\Users\alice\project`) and decodes from the drive root.
  See `splitSlugRoot` (`internal/ingest/utils.go`).
- **Windows paths are case-insensitive and may be short-named or junctioned.** git reports
  a worktree's *realpath*, which can differ from `filepath.Abs` in case, in the 8.3 form,
  or across a junction; `git worktree list --porcelain` can use CRLF. `ExecGitResolver.Worktree`
  compares with `filepath.Rel` and falls back to resolving symlinks on both sides; trim
  only the line terminator, never the whole line.
- **The in-memory test filesystem (MemFS) is slash-keyed by contract.** Do not run
  `filepath` over its keys — that yields backslash keys on Windows and silently misses.

## Windows-specific behavior and sharp edges

- **A colon in a path component names an NTFS alternate data stream, not a file.** The
  background server's PID file is `web-<port>.pid`; the pre-rename `web:<port>.pid` is kept
  as a read-only fallback so an older server can still be stopped.
- **Winsock error codes are not the unix errno constants.** Go defines the unix errno names
  on Windows as synthetic source-compatibility values (`syscall.EADDRINUSE` is not a real
  socket error there), so classify a socket error with the Winsock `WSA*` code.
- **There is no directory fsync.** The rename has already completed; only the POSIX
  durability barrier is unavailable. Do not fail the write over it.
- **There is no inode identity.** `syscall.Stat_t` does not exist; the build-tagged
  `fileIdentity` reports "unsupported" and the assertion skips rather than lying.
- **A detached process has no console to lend.** Every console child it spawns allocates
  its own visible window. Set `CREATE_NO_WINDOW` (via `proc.HideConsoleWindow`) on every
  child spawn, merged with any flags the call site already sets.
- **A running image cannot be overwritten but can be renamed.** `peasant upgrade` moves the
  running `peasant.exe` to `peasant.exe.old`, installs the new binary into the vacated path,
  rolls back on failure, and sweeps a stranded sidecar best-effort once per run.
- **PowerShell decodes a BOM-less `.ps1` as the machine's ANSI code page.** Keep the
  installer ASCII-only; strip a BOM at the serving route; CRLF is tolerated (unlike bash,
  where a single CR breaks the first directive).
- **Compare identity, not strings.** In tests, `os.SameFile` avoids failing on a case,
  short-name, or separator difference between two spellings of the same directory.

## CI gates

- **`windows-latest` job** (`.github/workflows/tests.yml`, runs on every PR): `go build
  ./...`; a typecheck of every Windows test file (`go vet` via `scripts/vet-go.sh
  --skip-sqlite-audit`); the whole `internal/defaults`, `internal/config`, `internal/proc`
  packages; a named Windows test set; and `go build -o peasant.exe ./cmd/peasant` followed
  by `peasant.exe version`.
- **Cross-compile from Linux** (`check-arm64`, release-gated): `CGO_ENABLED=0 GOOS=windows
  GOARCH=amd64 go build ./...` and the same `GOOS=windows` vet wrapper.
- **Selection discipline.** The named run is one `go test` command for several packages. A
  Windows-only test that no `-run` alternative matches compiles but never executes; add the
  new test name to the set when you add the test.
- **Release smoke** (`.github/workflows/release.yml`, `windows-smoke`): downloads the
  published zip and bare `.exe`, verifies both against `checksums.txt`, asserts the zip
  payload, runs both binaries, and starts, probes, and stops the dashboard.

## Release assets

`.goreleaser.yml` publishes two Windows assets from one archive template: the
`peasant_<version>_windows_amd64.zip` (the documented install path — it carries `LICENSE`,
`README.md`, and `THIRD_PARTY_NOTICES`) and the bare `peasant_<version>_windows_amd64.exe`
(the direct download and what `peasant upgrade` replaces in place). `checksums.txt` names
both. The archive-name template is a frozen contract; see
[release-runbook.md](release-runbook.md).

## Adding platform-specific code

1. Put the platform-neutral logic in the untagged file and only the platform call in the
   tagged siblings. Give the siblings identical signatures.
2. Keep the unix body byte-identical to what it replaces.
3. Add a build-tagged test per side. On Windows, add the test name to the `windows` job's
   `-run` set so it actually runs.
4. If it touches paths, use separators or `filepath.ToSlash`; if it touches the filesystem
   contract, keep MemFS slash-keyed.
5. Locally run `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...`,
   `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 ./scripts/vet-go.sh`, and `make check`.
6. Update this document, and the [release runbook](release-runbook.md) or
   [release architecture](release-architecture.md) if the change adds an artifact or a gate.
