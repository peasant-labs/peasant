# Network Boundary Reference

This document describes **every piece of data** that Peasant sends over the network, **when** it is sent, and **what user action triggers it**.

Peasant is a local-first tool. All ingestion, indexing, and analysis happens on your machine. Data leaves your device only through the actions described below, and by default every one of them is something you run yourself.

**There is one exception, and you have to turn it on.** If you install a Git upload hook for a repository (`peasant village hooks install`), Peasant uploads that repository's sessions automatically from then on — on every commit, or every push, depending on the event you chose. Nothing installs a hook for you, installation is per repository, and `peasant village hooks status` reports what is installed and `peasant village hooks uninstall` removes it. Until you install one, nothing on this page happens without you typing a command.

## Summary

| Action | Trigger | Destination | Data Sent | Automatic? |
|--------|---------|-------------|-----------|------------|
| [Push transcripts](#1-push-transcripts) | `peasant push` | Village API | Session metadata + transcript | No — wizard confirmation |
| [Push annotations](#2-push-annotations) | `peasant push` | Village API | Annotation labels + scores | No — wizard confirmation |
| [Hook-triggered push](#6-hook-triggered-push) | `git commit` or `git push` | Village API | Same as the two rows above | **Yes — after an explicit per-repository install** |
| [Login](#3-login) | `peasant login` | Village API | OAuth code exchange | No — explicit command |
| [Logout](#4-logout) | `peasant logout` | Village API | API key revocation | No — explicit command |
| [Model registry fetch](#5-model-registry-fetch) | `peasant ingest` | models.dev | Nothing (GET only) | Yes — during ingest |

**No telemetry. No analytics. No crash reporting. No update checker. No phone-home.**

---

## 1. Push Transcripts

**Trigger:** `peasant push` command → interactive wizard → user confirms session selection.

**Endpoint:** `POST {village}/api/v1/transcripts/publish`

**Authentication:** Bearer token (obtained via `peasant login`)

**Consent mechanism:**
- User must first run `peasant login` (one-time OAuth flow)
- A push you run yourself shows an interactive wizard listing sessions to upload
- User selects/deselects individual sessions before confirming
- `--force` flag skips the wizard (for scripting)
- Non-TTY environments skip the wizard automatically
- `--non-interactive` (or `--yes`) skips the wizard deliberately. **This is what an installed Git hook runs**, so a hook-triggered push shows no wizard and asks nothing — see [Hook-triggered push](#6-hook-triggered-push), where installing the hook is the consent step instead.

**What is sent:**

The push payload is a `multipart/form-data` request with two parts:

### Part 1: Metadata JSON (`PublishRequest`)

| Field | Example | Privacy | Notes |
|-------|---------|---------|-------|
| `identity.sessionId` | `a1b2c3d4-...` | Low | Random UUID |
| `identity.schemaVersion` | `7` | None | |
| `identity.parentSessionId` | `e5f6g7h8-...` | Low | Random UUID, null for root sessions |
| `model.provider` | `"claude"` | None | Provider name enum |
| `model.model` | `"claude-sonnet-4-20250514"` | None | Model identifier |
| `model.harnessVersion` | `"2.1.47"` | None | CLI tool version |
| `model.hostSlug` | `"github.com--user--repo"` | **HIGH** | Git remote in slug form. Plain boolean, **default: omitted** |
| `timestamp.start` | `1709913600000` | Low | Unix milliseconds |
| `timestamp.end` | `1709917200000` | Low | Unix milliseconds |
| `timestamp.ingested` | `1709920800000` | Low | Unix milliseconds |
| `source.filePath` | `"/home/<USER>/.claude/projects/..."` | Medium | Redacted — username replaced with `<USER>`, slug patterns redacted |
| `source.format` | `"jsonl"` | None | |
| `git.remote` | `"git@github.com:user/repo.git"` | **HIGH** | Reveals git identity. Tri-state, **default: sent as-is** — not redacted at the standard level either |
| `git.branch` | `"feat/my-feature"` | Medium | May contain feature descriptions. Plain boolean, **default: omitted** |
| `git.worktree` | `"/home/<USER>/dev/project"` | Medium | Redacted path |
| `git.tracking` | `"origin/main"` | Low | |
| `git.associations[].id` | `"assoc-..."` | Low | Opaque durable ID for one local session-to-observed-commit fact |
| `git.associations[].observedCommitHash` | `"abc123..."` | Medium | Commit hash observed during the session; sent so Village can validate later association-target annotations |
| `project.hash` | `"a1b2c3..."` | **HIGH** | Per-installation salted, non-correlatable identifier. **Always sent**, never field-visibility-gated |
| `project.filePath` | `"/<PATH>/project"` | Medium | Canonical-form path (everything before the project folder replaced by `<PATH>`). Tri-state, **default: sent only when no repository label went out** — see "Field Visibility Controls" |
| `project.name` | `"github.com:owner/repo"` | Low | The repository label (`host:owner/repo`) derived from the recorded git remote — **never** the raw project name/basename. Tri-state, **default: sent when the remote is recognizable** — see "Field Visibility Controls" |
| `stats.turnCount` | `42` | None | Aggregate count |
| `stats.toolCallCount` | `15` | None | Aggregate count |
| `stats.subagentCount` | `3` | None | Aggregate count |
| `stats.durationMs` | `360000` | None | Session duration |
| `stats.tokensIn` | `50000` | None | Token count |
| `stats.tokensOut` | `12000` | None | Token count |
| `quality.*` | (various metrics) | None | Computed quality scores |
| `entries[].contentPreview` | `"Let me help you..."` | **HIGH** | Up to 500 chars of conversation content. **Redacted by pattern — see [Transcript content is redacted, by pattern](#transcript-content-is-redacted-by-pattern).** |
| `entries[].toolInput` | `"{\"path\": \"/home/<USER>/...\"}"` | **HIGH** | Tool call arguments, including file paths. **Redacted by pattern.** |
| `entries[].toolOutput` | `"file contents..."` | **HIGH** | Tool call results, including file contents. **Redacted by pattern.** |
| `entries[].toolNamesCsv` | `"Read,Write,Bash"` | Low | Tool names only |
| `diagnostics.warnings[]` | (structured errors) | Medium | Locations/messages redacted |
| `subagents[].sessionId` | `"x1y2z3..."` | Low | Random UUID |

### Part 2: Transcript File

The full session transcript, built from the indexed entries. Contains the complete conversation including user prompts, assistant responses, tool calls, and tool results.

**Redaction applied before upload:**

1. **Metadata** is re-redacted at the effective level immediately before upload, with your custom patterns applied. At the offered Standard level, recognized identifying patterns are replaced in file paths, the working directory, and diagnostic locations. The git remote, git branch, host slug, and project name are identity fields published as recorded at every offered level when their field-visibility settings include them.
2. **Then content.** The same rules run over conversation content, tool arguments and tool results before the body is uploaded.

### Transcript content is redacted, by pattern

Peasant redacts conversation content, tool arguments, and tool results on the way out, at your configured level, using the same rules and the same code path as `peasant redact`. One redaction pipeline, two entrypoints.

Redaction is **pattern matching**. It finds the shapes it recognizes — API keys, email addresses, home-directory paths — and replaces each match with a canonical placeholder such as `<ANTHROPIC_KEY>` or `<EMAIL>`. It cannot promise it found every one.

**What that means for you, stated plainly:**

- **Your code is published, with matched tokens replaced.** Structure survives: a fenced block stays a fenced block, and a key inside it becomes `<ANTHROPIC_KEY>`. A published transcript can therefore differ from what you see locally.
- **Personal data that no pattern matched is published.** Matching removes the shapes it recognizes and cannot guarantee it found every one. This is the limit that matters, and it applies to content and metadata alike.
- **Identifying paths are redacted where a pattern matches them**, in content as well as in the metadata fields listed above. A home directory becomes `/Users/<USER>/...` — the username goes and the rest of the path stays.
- **The village scans the transcript it receives for secrets** and rejects a publish carrying them. That check does not depend on Peasant's rules, so it catches some of what Peasant's patterns would miss — but it reads the transcript part only, not the metadata published beside it, so treat it as a backstop for part of a publish rather than a second net under all of it.

If a session contains something you do not want published, exclude it at the wizard, or narrow what is eligible with the selection settings, rather than relying on redaction to remove it — pattern matching is a net, not a guarantee. `peasant redact` rewrites your stored transcripts in place at a chosen level if you want the local copy cleaned too; it is a separate, explicit action.

> **Changed in this version.** Content used to be published exactly as recorded, and the levels below Maximum used to replace every fenced code block with a `<CODE_BLOCK>` placeholder — which removed the artifact without scanning what was inside it. Both are fixed: content is redacted, and code survives with matched tokens replaced.

### Field Visibility Controls

The following fields can be controlled from the push payload via `config.yaml`:

```yaml
push:
  fields:
    gitRemote: false     # tri-state, default: on — omit the git remote URL
    gitBranch: false     # plain boolean, default: false — omit branch name
    projectPath: false   # tri-state, default: on — omit the local project path
    hostSlug: false      # plain boolean, default: false — omit host slug
    projectName: false   # tri-state, default: on — omit the repository label
```

`gitBranch` and `hostSlug` are plain booleans: an absent key and an explicit `false`
are the same thing (both off).

`gitRemote`, `projectPath`, and `projectName` are **tri-state**: an *absent* key
resolves to **on** (peasant sends the value by default); an *explicit* `true` or
`false` is kept exactly as written. This means a fresh `config.yaml` with no
`push.fields` block at all still sends the repository label and the git remote URL —
see `config.ProjectIdentitySentence` for the exact user-facing wording, shared
verbatim by the push wizard's consent screen and the kickstart privacy guide.

`projectName` and `gitRemote` gate the SAME decision, not two independent ones: a
recognizable git remote (`host:owner/repo`) is only rendered as the repository
label when both are visible — withholding the raw remote also withholds the label
derived from it, because the label names the exact git identity in a different
shape. `projectPath` is sent only when no label went out (no recognizable remote,
or the label was withheld): a label already identifies the project, so pairing it
with the local filesystem path would leak directory structure for no benefit. The
project path sent in this fallback case is redacted to the canonical form
(`/<PATH>/<project>`), never the raw path.

`project.hash` (a per-installation salted, non-correlatable identifier) is never
gated by any of these fields — it is always sent so the village can group a single
user's sessions by project without recovering any plaintext.

A field set to `false` produces `null` or empty values in the JSON — it never
reaches the network.

Association rows are different from annotations. Ingest creates them as durable
source facts only when commit detection observes commits, and the V40 migration
backfills legacy current-projection rows. Push includes current association
anchors with transcript metadata. Normal ingest and migration backfill do not
auto-create association-target annotations from those facts.

---

## 2. Push Annotations

**Trigger:** `peasant push` command (same wizard as transcripts)

**Endpoint:** `POST {village}/api/v1/annotations`

**What is sent per annotation:**

| Field | Example | Privacy | Notes |
|-------|---------|---------|-------|
| `contentHash` | `"sha3-256-hex"` | None | Deduplication key |
| `targetKind` | `"session"` | None | Enum: session, entry, annotation, project, or association |
| `sessionId` | `"a1b2c3..."` | Low | Random UUID |
| `targetAssociationId` | `"assoc-..."` | Low | Present only when `targetKind` is `"association"`; references a durable association anchor already published for the same owner |
| `typeId` | `"outcome"` | None | Annotation type |
| `value` | `"success"` | Low | Annotation value |
| `isPrimary` | `true` | None | |
| `confidence` | `0.85` | None | |
| `reason` | `"Task completed successfully"` | Medium | Free-text justification |
| `annotatorName` | `"auto-classifier"` | None | |
| `projectHash` | `"a1b2c3..."` | **HIGH** | Same unsalted hash as transcripts |

Annotations are batched (500 per request). The first batch is sent
synchronously; remaining batches use bounded parallelism.

Association-target annotations are explicit semantic records. They are sent only
when an eligible local producer has written them, including an intentional local
import. Pulled foreign annotations and data are one-way and never become local
push or re-push candidates. The push command sends transcript metadata, including
durable association anchors, before annotation batches; a Village that cannot
resolve the referenced owner-scoped association rejects the annotation batch
rather than accepting a dangling target.

---

## 3. Login

**Trigger:** `peasant login` command

**Endpoint:** `{village}/api/v1/auth/cli/login` (browser redirect) + `{village}/api/v1/auth/cli/exchange` (code exchange)

**Flow:**
1. Peasant starts a local HTTP server on `127.0.0.1:17249`
2. Opens browser to village login page with OAuth state parameter
3. User authenticates in browser
4. Village redirects to local callback with authorization code
5. Peasant exchanges code for API key via POST to `/api/v1/auth/cli/exchange`

**What is sent:** OAuth authorization code + state token
**What is received:** API key, key ID, user ID, username
**What is stored locally:** API key + key ID in `~/.config/peasant/auth.json`

---

## 4. Logout

**Trigger:** `peasant logout` command

**Endpoint:** `DELETE {village}/api/v1/auth/api-keys/{keyId}`

**What is sent:** Authorization header with the key being revoked (DELETE request, no body)

---

## 5. Model Registry Fetch

**Trigger:** Automatic during `peasant ingest` pipeline

**Endpoint:** `GET https://models.dev/api.json`

**What is sent:** Nothing — this is a simple GET request with no authentication, no cookies, no identifying headers beyond the default Go HTTP client User-Agent.

**What is received:** Public model registry data (provider names, model IDs, pricing, context windows).

**Fallback:** If the fetch fails (offline, timeout), Peasant falls back to a static model registry embedded in the binary. No user data is exposed.

---

## 6. Hook-triggered push

This is the only path on this page that sends data without you running a command, and it exists only after you install it.

**Trigger:** `git commit` (a `post-commit` hook) or `git push` (a `pre-push` hook), in a repository where you installed one.

**Endpoint:** the same publish endpoints as [Push transcripts](#1-push-transcripts) and [Push annotations](#2-push-annotations). The data sent is identical — this is not a different upload, it is the same one on a different trigger.

**Consent mechanism:** installation is the consent step, and it is the whole of it.

- Nothing installs a hook for you. You run `peasant village hooks install --event post-commit` (or `--event pre-push`) yourself, **per repository**, naming the event explicitly — there is no default and no all-repositories option.
- Install writes only into an empty hook slot, or over a hook Peasant itself wrote. Anything else already in that slot — including a file from another tool — is left exactly as it is and reported, never edited, wrapped, or renamed. `peasant village hooks status` is the read-only preview: it reports what Git would run for each event without changing anything.
- From then on, **every matching Git operation uploads without asking.** The hook runs `peasant village push --non-interactive --quiet`, which deliberately skips the wizard described in section 1. There is no per-commit prompt, and by design there cannot be one: Git is waiting.
- `peasant village hooks status` reports exactly what is installed and what it will publish at, including a warning if your configuration would make the upload fail.
- `peasant village hooks uninstall` removes it. It removes only a hook Peasant wrote.

**Scope:** the upload is confined to the repository Git is acting on, and it honours the same selection settings as a manual push. It does not publish other projects.

**Failure behaviour:** a failed upload prints a warning and lets your commit or push proceed. Peasant never blocks or undoes Git work because the village was unreachable, rejected the payload, or your login expired. The hook always exits successfully.

**Latency:** the upload is synchronous and runs inside your Git command, so it adds its own network time to every commit or push. `--timeout` bounds it.

**What redaction covers:** exactly as in section 1 — see [Transcript content is redacted, by pattern](#transcript-content-is-redacted-by-pattern). A hook does not add protection and does not remove any; it changes when the upload happens, not what it contains.

---

## Village URL

| Environment | URL |
|-------------|-----|
| Production | `https://api.village.peasantlabs.org` |
| Local dev | `https://localhost:8443` |
| Override | `PEASANT_VILLAGE_URL` environment variable or `village.url` in `config.yaml` |

---

## What Peasant Does NOT Do

- **No telemetry or analytics** — no usage data, feature flags, or behavioral tracking
- **No crash reporting** — errors are logged locally only
- **No update checker** — no version polling or auto-update
- **No background network calls** — nothing runs on a timer, in a daemon, or between commands. The only automatic fetch is `models.dev` during an explicit `peasant ingest`. A [Git upload hook](#6-hook-triggered-push), if you installed one, uploads inside your own `git commit` or `git push` — it is automatic in the sense that it needs no separate command, and it still runs only when you run Git in that repository
- **No data sharing without an explicit user action that enables it** — publishing requires a login, and either a wizard confirmation (`peasant push`) or a per-repository hook you installed yourself. Peasant never publishes a repository you did not either push by hand or install a hook for
