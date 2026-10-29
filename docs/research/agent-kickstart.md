# Open the current session from the agent

Research proposal, checked against provider documentation on 2026-10-29. This is
an implementation plan, not a claim that these launch flows are shipped.

The first success is **open this exact session locally**. The audience is a
developer already working in Claude Code, Codex, Cursor, OpenCode, or Gemini CLI.
They should not have to switch to a terminal, answer the full setup wizard, or
let a model construct an installation command. Account connection, publication,
and automatic publishing are later, separate choices.

## Recommended direction

Use an explicit in-agent **open in peasant** action backed by one fixed Go
operation. For a browser-free destination, open a small native window containing
the existing Peasant viewer. Reuse Fairtrade and the mounted transcript body;
do not build a second transcript renderer. A provider-native embedded panel can
be an alternative where the provider offers one. The native destination is new
work: `peasant open` presently launches the default browser.

The entry and destination solve different problems. The provider knows which
conversation is active; a native launcher alone does not. Use the provider's
trusted runtime context to bind identity, then hand that exact identity to the
local helper. Never infer "current" from the newest file, focused application,
project directory, or a model's guess.

| Approach | User effort | Determinism and cost | Decision |
|---|---|---|---|
| In-agent action + fixed helper | One explicit action after installation | Provider identity can be authoritative; reuse the Go ingest/open path | Primary entry |
| Native companion/launcher | One installation; one click or shortcut | Can bundle the helper and avoid browser setup; needs a provider handoff or an honest session chooser | Preferred browser-free destination; secondary entry |
| Skill that asks the model to run commands | Familiar invocation | Model can alter shell arguments or choose the wrong session; tool approval is not proof of identity | Discovery/help only, not the execution authority |
| Generic MCP tool | Reusable across clients | A client connection is not automatically a conversation identity; model-supplied IDs are not trusted context | Optional transport only after binding and explicit invocation are proven |

A one-time installer/provider-manager flow must disclose the helper and identity
hooks it installs. The existing Claude plugin requires an installed `peasant`
binary; it does not solve first installation. Do not replace that gap with a
model-generated download, a repository script, or unattended hook installation.
A native companion can bundle the helper, but OS packaging and signing belong
in its own implementation work.

## Provider evidence and remaining proof

These are documented integration primitives, not an assertion that a skill or
an MCP server receives the same fields in every host. Restrict the first release
to local provider sessions whose recording can be verified on this machine.

| Provider | Exact identity available from runtime | Proposed entry and unresolved proof |
|---|---|---|
| Claude Code | Skill substitution exposes `CLAUDE_SESSION_ID`; hooks include `session_id`, `transcript_path`, and `cwd`. `UserPromptExpansion` distinguishes a user-typed command before model expansion. | Reuse the existing `/peasant` interception. Make the new strict path reject missing identity. Skill-only fallback still involves the model. [Skills](https://code.claude.com/docs/en/skills#available-string-substitutions), [hooks](https://code.claude.com/docs/en/hooks#userpromptexpansion). |
| Codex | Hooks document `session_id`, nullable `transcript_path`, and `cwd`; subagent hooks use the parent session ID. | Prototype a user-submission hook for an exact reserved invocation, with its runtime ID passed directly to Go. Skills support explicit invocation and disabling implicit invocation; that does not itself authenticate a later model tool call. Prove the command/skill expansion order and user-visible completion behavior. Null paths and hosted sessions are unavailable cases, not permission to guess. [Hooks](https://learn.chatgpt.com/docs/hooks), [skills](https://learn.chatgpt.com/docs/build-skills). |
| Cursor | Agent hooks expose stable `conversation_id`, nullable `transcript_path`, and workspace roots. `beforeSubmitPrompt` receives the submitted prompt and can stop submission. | Prototype exact-command interception in the local editor. Verify the hook ID against the recording identity Peasant's Cursor adapter actually uses. Disabled transcripts and cloud sessions need an explicit unavailable state. Command/skill text alone is not an identity channel. [Hooks](https://cursor.com/docs/hooks), [skills](https://cursor.com/help/customization/skills). |
| OpenCode | A custom tool receives `context.sessionID`, `messageID`, `directory`, and `worktree` outside its model arguments. | A zero-argument tool can bind the right session. Require explicit host/user authorization to invoke it; model access alone is insufficient. Verify a plugin command/button that calls it directly before promising model-free execution. Shell preprocessing produces prompt content; the command documentation does not establish a current-session variable. [Tool context](https://opencode.ai/docs/custom-tools/#context), [commands](https://opencode.ai/docs/commands/#shell-output), [plugins](https://opencode.ai/docs/plugins/). |
| Gemini CLI | Hook input includes `session_id`, absolute `transcript_path`, and `cwd`. `BeforeAgent` receives the submitted prompt and can block the turn. | Prototype an exact user-command hook after ingestion support exists. A TOML custom command's shell block is confirmed by the host and feeds output into a prompt; do not describe it as an argument-free identity channel or zero-model completion. [Hook reference](https://geminicli.com/docs/hooks/reference/), [custom commands](https://geminicli.com/docs/cli/custom-commands/). |

Codex requires review/trust of non-managed hook definitions and changed hooks.
Honor that review; do not bypass it. Its MCP documentation describes transports
and configuration, not an automatic current-local-transcript binding.
[Hook trust](https://learn.chatgpt.com/docs/hooks#review-and-trust-hooks),
[MCP](https://learn.chatgpt.com/docs/extend/mcp?surface=cli).

**Gemini ingestion is a prerequisite.** The inspected Peasant source recognizes
the harness name but has no Gemini source configuration/registered adapter;
`internal/ingest/testdata/record_kinds_validation.yaml` explicitly rejects it as
an unsupported adapter. An identity hook is not an ingestion implementation.
Verify the chosen build's actual registry before exposing a working entry.
A Gemini adapter and any necessary Bestiary/Schema release ceremony are separate
work; this bootstrap proposal changes no contract or migration.

## Small first-run flow

1. The user installs/enables the integration through a provider manager or native
   installer and approves its fixed helper/identity hook. Preserve provider policy
   when hooks are disabled; report the limitation without silently falling back
   to model-generated execution.
2. Inside the active agent, the user chooses **open in peasant**. The host passes
   its conversation identity directly. No model-authored session ID, path, URL,
   source root, or action string participates in that call.
3. Go validates the identity and recording, imports only that session, and reuses
   or starts the local service. Existing `--session` harvest semantics already
   avoid widening the saved discovery selection. On a fresh install, prove and,
   where necessary, implement the minimal canonical configuration save for this
   exact session; do not default to importing every provider's history.
4. The native transcript window opens at that session, with provider, title,
   timestamp, and **not published** state. Until a native destination exists,
   describe the existing browser launch honestly rather than marketing it away.
5. Broader selection, account connection, retention, licensing, and automatic
   publishing remain accessible in their existing flows. Opening a recording
   authorizes none of those changes.

Do not make the user type a session ID. When exact identity is unavailable, show
an explicit session chooser with provider, project, time, and title; that is
**choose a session**, not **open current session**. With concurrent chats, keep
bindings per session/connection, never one global "current session" file.

## Deterministic boundary

The trust boundary is the fixed operation, not a clever prompt:

- Only a user-invoked provider action/approved native button may trigger opening.
  Checking whether transcript text contains `/peasant` is not authorization.
- The operation has no publish, login, auto-publish, install-hook, arbitrary-shell,
  arbitrary-URL, or arbitrary-file mode. Keep existing `/peasant auto` as a distinct
  explicit flow; never infer it from text or forward free-form arguments to it.
- Parse provider hook JSON with a real parser in Go. Treat runtime fields as
  identity evidence to verify, not executable text. Do not interpolate prompt,
  command arguments, tool output, or repository content into a shell.
- Resolve providers through the canonical supported-harness/adapter registry.
  Accept that provider's native ID format; do not assume all IDs are UUIDs.
- Resolve recording roots from trusted user configuration/provider discovery,
  canonicalize paths, check file ownership/type and ID agreement, and prevent
  symlink escape or file replacement between validation and reading. A path from
  a hook is not permission to read a different user's arbitrary file.
- Use the existing ingest, native-identity, redaction, projection, and store path.
  Local originals remain local under existing policy; this operation does not
  claim that importing has redacted the local copy or granted publication consent.
- Reuse the existing exact-session service check. Bind loopback locally, reject
  a different data directory/server, and construct the destination from the
  verified project/session. Do not follow a model-returned URL.
- Missing/rotated recordings, conflicting IDs, disabled transcripts, unsupported
  providers, or a remote host fail with a specific recovery. No newest-file or
  CWD-only fallback. Keep any legacy recent-session convenience explicitly named
  and separate, rather than deleting an existing user flow.

Hook fields are not cryptographic attestations. This boundary limits the new
entry's effects against hostile repository/transcript/model content; it does not
claim to defeat another process with the user's full OS permissions or secure
unrelated broadly privileged agent tools.

## Implementation order and source map

First factor the existing Go open operation into **prepare this session locally**
and a destination adapter. Preserve the current CLI/browser behavior as one
adapter. Add provider-native parsing around the operation, then test the browser-
free destination. Do not add a new REST/WebSocket API merely for onboarding.

| Work | Existing source/authority |
|---|---|
| Exact-session harvest, lookup, service readiness, and output | [`cmd/peasant/cmd_open.go`](../../cmd/peasant/cmd_open.go), [`cmd_open_test.go`](../../cmd/peasant/cmd_open_test.go), [`open-output.yaml`](../../cmd/peasant/testdata/open-output.yaml) |
| Typed harness resolution, targeted import, saved-selection behavior | [`cmd/peasant/cmd_harvest.go`](../../cmd/peasant/cmd_harvest.go), [`internal/config/config.go`](../../internal/config/config.go), `BaseConfig`, `SaveAtomic`, and the canonical selection matcher |
| Supported providers and native identity | [`internal/defaults/provider.go`](../../internal/defaults/provider.go), [`internal/ingest`](../../internal/ingest), especially Claude identity and Codex/Cursor/OpenCode adapters; keep root-vs-helper identity distinctions |
| Existing setup and its effects | [`docs/KICKSTART.md`](../KICKSTART.md), [`cmd/peasant/cmd_kickstart.go`](../../cmd/peasant/cmd_kickstart.go) |
| Existing service and destination seams | [`cmd/peasant/cmd_web.go`](../../cmd/peasant/cmd_web.go), [`internal/browser/open.go`](../../internal/browser/open.go); a native window is new work, not a renamed browser call |
| Existing Claude integration, in the separate peasant-plugins repo | `plugins/peasant/hooks/hooks.json`, `plugins/peasant/SKILL.md`, `plugins/peasant/scripts/open-session.sh` |
| Provider-specific entries | Add thin installed integrations in peasant-plugins; fixed helper owns behavior. Keep native plugin/hook packaging separate from the existing git publication hooks. |

The existing Claude shim already intercepts `UserPromptExpansion` and delegates
to `peasant open`. Its missing-ID fallback searches the newest transcript in a
CWD-derived folder, and its JSON field extraction uses shell text processing.
The strict new entry should instead parse trusted native context in Go and
refuse ambiguous identity. The old entry's explicit publication flow and public
CLI behavior remain separately reviewable.

Implementation checkpoints:

1. **Core and Claude:** fixture-drive exact identity, narrow first-run setup,
   direct-session readiness, and failures; reuse the installed command path.
2. **Local native destination:** prove the existing viewer loads with its actual
   data and persistent chrome, keyboard access, both themes, and no browser tab.
   Measure cold and warm open time; do not advertise latency before measuring it.
3. **Codex, Cursor, OpenCode:** prototype each identity handoff, user invocation,
   concurrency, and displayed completion on the real provider. Ship only the
   provider paths whose identity and recordings match; expose unsupported states.
4. **Gemini:** complete ingestion support separately, then run the same handoff
   and end-to-end gates. Do not substitute another provider's parser or invent
   support from a recognized harness name.

Future validation belongs in named YAML fixtures: two simultaneous chats in one
CWD; same ID across providers; resumed/forked sessions; a child vs root session;
missing/disabled/rotated transcript; unsupported adapter; Unicode/space paths;
symlink escape and replacement; malicious prompt/arguments/path; source-ID
mismatch; stale binding; existing service on the wrong data directory; no-TTY
first run; partial import failure; and explicit cancellation. Assert one-session
import and zero publication, credential, retention, hook, and unrelated-config
effects. Use mounted provider tests only for the identity/invocation behavior
that a unit fixture cannot observe; keep lifecycle cleanup and runtime bounded.
