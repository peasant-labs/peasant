# Install Peasant and run Kickstart from a coding agent

**Proposal:** let a developer install Peasant and complete its initial setup
without assembling commands or leaving the coding-agent workflow. This must work
when `peasant` is not installed. A native installer/setup window is the preferred
destination; that integration and GUI are new work, not shipped behavior.

## What exists

Peasant has [official release artifacts](https://github.com/peasant-labs/peasant/releases)
and platform installation guides: [macOS](../install/macos.md),
[Linux](../install/ubuntu.md), [Windows](../install/windows.md), and
[Nix](../install/nix.md). Release archives/packages have published checksums.

Once installed, [`peasant kickstart`](../KICKSTART.md) discovers recordings,
lets the user choose projects, branches, and sessions, buffers settings for
review, then saves configuration, applies the chosen Claude retention setting,
and imports the selected recordings locally. It publishes nothing.

Kickstart currently needs an interactive terminal. Bare `peasant` does not start
it automatically, and running it through a noninteractive agent shell does not
complete setup. `peasant harvest` is an import command, not a headless wizard.

The existing Claude plugin checks for `peasant` on `PATH`; it opens recordings
or configures separately requested auto-publication. It cannot install Peasant
on a fresh machine.

## Proposed flow

1. **Start inside the agent.** Offer one explicit **install and set up peasant**
   action. A deterministic bootstrap checks for an existing installation and
   identifies the target machine's OS and architecture. Do not silently install
   on a remote agent host when the user expects their laptop.
2. **Approve installation.** Show the official distribution, artifact/version,
   destination, privileges, and changes before proceeding. Use a fixed installer
   or supported package-manager operation, checking the artifact against the
   distribution's published verification material. Never let a model invent a
   download-and-execute command or run an arbitrary repository installer. Respect
   host approval policy; report unsupported platforms and failed verification.
3. **Complete canonical Kickstart.** Launch the existing interactive command
   where the host provides a usable terminal. Otherwise report the terminal
   requirement and offer an honest handoff. A proposed native setup window should
   reuse Kickstart's settings, selection matcher, validation, atomic save,
   retention, and ingest engine; it must not create a second setup implementation.
4. **Review and finish.** Let the user choose import scope and review local
   effects. Account connection remains optional and requires an explicit choice.
   Never install hooks, log in, or publish silently. Saving an auto-publish
   preference installs no hook and publishes nothing. Preserve existing data and
   settings; installation is not a reset. Report installation, save, and import
   failures separately, with a retry.

Provider integrations should only launch this approved bootstrap. Use each
host's supported explicit action and permission surface; respect disabled hooks
and unavailable terminal access. Do not promise the same installation experience
in Claude Code, Codex, Cursor, OpenCode, or Gemini until tested in that host.

## Implementation boundary

Build the official bootstrap first, then the agent entry and terminal handoff;
add a native wizard only by adapting the existing engine. Validate fresh-machine
installation, existing installs, cancellation, verification failure, missing TTY,
and save/import retries with no unintended login, hooks, or publication.

Start with [release packaging](../../.goreleaser.yml),
[Kickstart wiring](../../cmd/peasant/cmd_kickstart_mount.go),
[terminal checks](../../cmd/peasant/terminal.go), and
[canonical settings](../../internal/tui/settings). Existing plugin code lives in
peasant-plugins under `plugins/peasant/{SKILL.md,scripts/open-session.sh}`.
