# Open the current session from the agent

Research proposal; provider documentation checked 2026-10-29. These flows are
**not shipped**. Existing `peasant open` launches the default browser.

## Recommendation

Provide an explicit **open in peasant** action backed by a fixed Go operation:
verify the host's exact session identity, import only that recording, and open
it locally. Prefer a small native window reusing Fairtrade's existing viewer;
use a provider panel where available. Keep the browser destination working.
Show provider, title, time, and **not published** state.

A launcher needs a provider handoff or an honest session chooser. Skills can
explain setup; model-authored commands and IDs cannot authorize execution. MCP
is only a possible transport after identity binding and explicit invocation are
proven. Installation must disclose and obtain approval for helper/identity hooks;
never substitute model-generated downloads or unattended installation. Respect
disabled hooks; do not fall back to model execution.

## Provider limits

Start with verifiable local recordings. These primitives still need mounted
proof of identity, invocation order, concurrency, and completion behavior.

| Provider | Available context and remaining proof |
|---|---|
| Claude Code | Skills expose `CLAUDE_SESSION_ID`; hooks provide `session_id`, `transcript_path`, `cwd`. Reuse user-typed `UserPromptExpansion` interception. The installed shim requires Peasant, extracts JSON with shell text processing, and guesses the newest CWD transcript when ID is missing. Replace those shortcomings in the strict entry. [Skills](https://code.claude.com/docs/en/skills#available-string-substitutions), [hooks](https://code.claude.com/docs/en/hooks#userpromptexpansion). |
| Codex | Hooks provide `session_id`, nullable transcript path, and CWD; subagent hooks identify the parent. Prove reserved-command interception/expansion order; explicit skills alone do not authenticate later tool calls. Honor hook trust review. Null paths/hosted sessions are unavailable. [Hooks](https://learn.chatgpt.com/docs/hooks), [skills](https://learn.chatgpt.com/docs/build-skills), [trust](https://learn.chatgpt.com/docs/hooks#review-and-trust-hooks), [MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli). |
| Cursor | `beforeSubmitPrompt` can intercept submission; hooks expose conversation ID, nullable transcript path, workspace roots. Verify ID agreement with Peasant's adapter; disabled transcripts/cloud sessions are unavailable. [Hooks](https://cursor.com/docs/hooks), [skills](https://cursor.com/help/customization/skills). |
| OpenCode | Tool context supplies session/message IDs, directory, worktree outside model arguments. Prove a directly authorized plugin command/button; model access is insufficient. Shell preprocessing supplies prompt content, not proven session identity. [Tools](https://opencode.ai/docs/custom-tools/#context), [commands](https://opencode.ai/docs/commands/#shell-output), [plugins](https://opencode.ai/docs/plugins/). |
| Gemini CLI | Hooks expose session ID, absolute transcript path, CWD; `BeforeAgent` can block submission. TOML shell commands feed a prompt, not model-free completion. **Implement ingestion first**: Peasant recognizes the name but lacks a configured/registered adapter. [Hooks](https://geminicli.com/docs/hooks/reference/), [commands](https://geminicli.com/docs/cli/custom-commands/). |

## Identity, consent, and security

- Require a user-invoked host action/approved button. Transcript text containing
  `/peasant` is not authorization. Never accept model-authored IDs, paths, URLs,
  source roots, or action strings; never interpolate untrusted content into shell.
- Parse hook JSON in Go. Use the canonical adapter registry and native ID formats,
  preserving root/helper distinctions. Verify configured recording roots, ownership,
  file type, and ID agreement; prevent symlink escape and validation/read replacement.
- Import exactly one session through existing identity, ingest, redaction,
  projection, and store paths. Prove minimal first-run configuration without
  importing all history or widening saved selection. Local import grants no
  publication consent and does not promise a redacted local original.
- Reuse exact-session readiness: loopback only, correct service/data directory,
  destination constructed from verified project/session. No model-returned URL.
- Keep login, publication, retention, licensing, hook installation, and automatic
  publishing separate. Preserve `/peasant auto` as its own explicit flow.
- Missing, rotated, disabled, conflicting, unsupported, or remote recordings fail
  with recovery. Never guess from newest file, CWD, focus, or model output. Offer
  **choose a session** when exact identity is unavailable; bind concurrent chats
  per session/connection. Preserve explicitly named legacy convenience flows.

Hook fields are not cryptographic attestations; this boundary does not defend
against processes with the user's full OS permissions.

## Implementation order

1. Factor “prepare this session locally” from its destination; preserve CLI/browser
   behavior. Start with strict Claude parsing and named YAML fixtures covering
   identity, one-session import, hostile paths/content, cancellation, and zero
   publication/credential/retention/hook/unrelated-config effects. Add no onboarding API.
2. Prove the native viewer's real data/chrome, keyboard access, both themes, and
   browser-free opening; measure cold/warm latency before claiming speed.
3. Prove Codex, Cursor, OpenCode handoffs individually. Add Gemini ingestion and
   required Bestiary/Schema releases separately before offering its entry.

Source map: [open/readiness](../../cmd/peasant/cmd_open.go),
[fixtures](../../cmd/peasant/testdata/open-output.yaml),
[harvest](../../cmd/peasant/cmd_harvest.go),
[configuration](../../internal/config/config.go) (`BaseConfig`, `SaveAtomic`, canonical matcher),
[registry](../../internal/defaults/provider.go), [adapters/identity](../../internal/ingest),
[setup](../KICKSTART.md), [service](../../cmd/peasant/cmd_web.go),
[browser](../../internal/browser/open.go). Provider packaging belongs in
peasant-plugins; inspect `plugins/peasant/{hooks/hooks.json,SKILL.md,scripts/open-session.sh}`.
