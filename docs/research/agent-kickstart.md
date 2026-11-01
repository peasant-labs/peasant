# Install, then ask your agent

Run one command in your terminal to install Peasant and start its existing Kickstart:

```sh
curl -fsSL https://peasantlabs.org/install | bash -s -- --kickstart
```

Then tell Claude or your coding agent:

> Open this session in Peasant.

Installation adds no project-picker window, account screen, or publishing step.
Kickstart keeps its existing interactive setup. Open the session in the local
web app and review it first. Publishing online is a separate explicit action,
with the destination resolved before anything leaves.

## Implementation

The website installer's optional `--kickstart` mode verifies and installs the
release, then invokes that exact binary from `~/.local/bin` with the terminal
attached. It works before the directory is on `PATH`. Bare installation remains
install-only; cancellation, failed verification, and missing terminals never
launch setup. On Windows, use the existing installer and the Kickstart command
it prints.

Reuse canonical Kickstart selection, validation, atomic save, retention, and
ingest. Preserve existing data and untouched settings. Do not install hooks,
log in, or publish as an installation side effect.

The current Claude Code `/peasant` plugin opens this session locally; it does
not upload it. The natural-language prompt relies on a supported agent integration;
installing the Peasant binary alone does not install that plugin. Without an
integration, use `peasant web start` to open the local dashboard and choose the
session. A later publishing request must use a supported CLI or the canonical
publish view, ask for missing audience information, and preserve explicit consent.

[Interactive proposal](https://peasant-kickstart-proposal.handysoftwarestudios.chatgpt.site).
