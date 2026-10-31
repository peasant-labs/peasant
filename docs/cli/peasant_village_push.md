## peasant village push

Push ingested transcripts to peasant online

### Synopsis

Upload locally ingested session transcripts to peasant online for sharing and analytics.

Performance — the --concurrency default is max(1, NumCPU/2), tuned for the common case: a
steady-state / no-change re-push, where the server manifest skips already-pushed annotations
and unchanged transcripts so very little goes over the wire. A one-time LARGE COLD push (e.g.
hundreds of new transcripts + tens of thousands of new annotations) benefits from a higher
--concurrency. Throughput is bounded by the per-transcript S3 round-trip and peasant online's
CPU — NOT by the DB connection pool: a pooled connection is held only for the brief row
insert (the S3 upload itself holds none), so the peasant online pool (default ~2x its vCPUs) has
headroom. A genuinely cold run of N new transcripts costs roughly
ceil(N / effective-concurrency) x the per-upload round-trip. For such a push set
--concurrency to about 2x NumCPU to scale throughput up to peasant online's capacity; confirm
with --timing. The default stays at max(1, NumCPU/2) — sufficient for the common
steady-state re-push, where the manifest skip means little goes over the wire.)

Auto-publish rules — when a rule in hooks.yaml in the config directory binds a session
this push sends (matched against the repository the session was recorded in), the push
publishes collectives-only: private, with no license, and each bound transcript it sent
is shared with its rule's collectives. --visibility and --license are then refused. A
session a paused rule covers, or whose transcript is already public, is not published.

Last-good — a session may keep a newer incomplete attempt beside its last-good full
generation. Such a session is refused by default: the incomplete attempt is never
published as the session, and readiness stays not-ready so stale content is never sent
automatically. --last-good publishes the last-good full transcript instead, as an
explicit hand-run choice; it is refused together with --auto-publish-event and
--require-auto-publish-rule, and no generated hook file carries it.

Named sessions — --session and --sessions-from push only the sessions they name, with the
subagent sessions of each named main session, nested ones included (--without-subagents
leaves the whole tree out). Every
other narrowing and readiness still applies, with one exception: a subagent session pulled in
with a named session follows that session's saved-selection decision instead of its own branch,
as long as the named session is sent by the same run (subagent sessions often run on worktree
branches you never chose to exclude). A subagent session named on its own keeps its own
check. --dry-run --json prints a plan with one entry
per named session saying what the run would do and why. --collective shares each sent
transcript with the collectives it names, publishing private and unlicensed without
consulting the auto-publish rules for those sessions, so the push asks once before it runs.

Exit status — a caller that branches on it, including a generated Git hook, needs the
one distinction it cannot read out of prose: whether anything was published at all.

  0  the run succeeded.
  1  the run failed after it had started talking to peasant online, so part of the work
     may be published and recorded as published. That includes a run that sent some
     sessions and could not send one: an upload error, an upload peasant online refused,
     or a failed share. A session that is not ready, left out or unchanged is not a
     failure.
  3  the run failed BEFORE it sent peasant online anything to write — a missing or expired
     login, an unreadable config, a store that would not open, a repository scope that
     would not resolve, a named session the store does not hold, an unknown collective
     (found by the one read that resolves --collective names), or a confirmation that
     could not be asked. Nothing was published and nothing was recorded as published.

```
peasant village push [flags]
```

### Options

```
      --annotation-hash stringArray   Only push annotations with these content hashes (repeatable; default: all).
      --annotation-id stringArray     Only push these annotation IDs (repeatable; default: all). Counterpart to the share wizard's label selection.
      --auto-publish-event string     Require consent for this hook event (post-commit or pre-push)
      --collective stringArray        Share each sent transcript with this collective: a name, UUID or page URL, as in village pull (repeatable; needs --session or --sessions-from). Publishes private and unlicensed and skips your auto-publish rules for the named sessions, so it asks for confirmation once unless --yes is given. A --dry-run reads the collective list to resolve the names, and the visibility of any transcript it would update, and writes nothing
      --concurrency int               Number of parallel uploads and HTTP connection-pool size. Must be >= 1. Overrides push.concurrency in config. Default: max(1, NumCPU/2) (tuned for steady-state re-push). For a one-time large COLD push, raise it to about 2x NumCPU and confirm throughput with --timing.
      --dry-run                       Show what would be pushed without uploading
      --force                         Re-push all sessions (including already-pushed ones)
  -h, --help                          help for push
      --json                          Output as JSON instead of human-readable
      --last-good                     Publish the last-good full transcript when a newer incomplete attempt is kept beside it. A hand-run choice: it skips the push wizard (the wizard previews the newer attempt, while this flag publishes the last-good authority) and does not imply --non-interactive, so the public-consent prompt still applies. It is refused together with --auto-publish-event/--require-auto-publish-rule and is never written into a hook file, so an automatic publication never sends stale last-good content. The store resolves the last-good authority server-side, so no session or generation identifier is accepted
      --license string                Override the content license for this run (CC0-1.0, CC-BY-4.0, CC-BY-SA-4.0). Also relicenses every already-published session the run selects; a Creative Commons grant cannot be withdrawn. Without it an update keeps the license a transcript has on peasant online, except that a session with no publication receipt for this peasant online account on this machine is sent the configured license
      --non-interactive               Run without the interactive wizard or public-consent prompt (for CI/scripts)
      --profile-output string         Write a local JSON v1 push profile to this file (local diagnostic only, mode 0600). Parent directory must exist. Enables profiling; prints path and bottleneck hints to stderr unless --quiet. Works with --json and --timing.
      --profile-trace string          Write an optional JSONL trace of profile events to this file (mode 0600). Requires --profile-output and a distinct regular-file destination with an existing parent. JSON records an opaque trace reference, not the path; the actual path is printed to stderr unless --quiet.
      --quiet                         Suppress the summary and redaction report; print only errors, a waiting prompt request, and a final result line
      --repository string             Only push sessions carrying this Git repository's canonical project identity (a path). Peasant first uses the normalized checkout upstream remote when there is one it can normalize, then the normalized origin remote, so separate clones using the same remote share an identity. If neither is usable — including when there is no origin remote or a remote is a local path or file:// URL — identity instead comes from the worktree paths the sessions were recorded in, which belong to that directory alone. A repository nested inside another keeps its own identity and never inherits the outer one's. Which identity was used is printed when the push runs. Default: every configured session
      --require-auto-publish-rule     Publish only sessions currently bound by an active auto-publish rule (used by rule-installed hooks)
      --session stringArray           Push only this session (repeatable; combines with --sessions-from). The selection is an AND with --repository, --source-harness, the saved selection and readiness; a session that fails one is reported, never sent. The subagent sessions of each named main session travel with it, nested ones included, and follow its saved-selection decision. Skips the push wizard. A real run refuses an id the local store does not hold
      --sessions-from string          Read session ids from a file, or from standard input when the value is -. Accepts one id per line, a JSON array of ids, or the JSON array that a --json listing prints
      --source-harness string         Filter to a specific harness (claude-code, gemini-cli, codex, opencode, cursor, strike, pi)
      --timeout duration              Overall time budget for the whole upload (e.g. 5s). The per-request client timeout does not bound a push, which issues several requests in sequence, so a peasant online server that accepts a connection and never answers can stall for minutes. On expiry the push gives up and reports what did and did not reach peasant online. Default: no budget. Git hooks always pass one.
      --timing                        Measure and report per-phase push timing (connection setup/server split, redaction, annotation batches) to stderr, plus a per-upload JSONL log under the state dir. Off by default.
      --verbose                       Show per-session detail
      --visibility string             Override visibility for this run (private, public). Also changes every already-published session the run selects, including ones shared with collectives on peasant online, which otherwise keep the visibility they have there
      --without-subagents             Leave out the subagent sessions of the named main sessions, the whole tree; the plan counts them under subagentsOmitted
  -y, --yes                           (alias for --non-interactive)
```

### Options inherited from parent commands

```
      --config string       Path to config file (default "~/.config/peasant/config.yaml")
      --config-dir string   Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant
      --data-dir string     Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant
      --state-dir string    Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant
  -v, --version             Print the version
```

### SEE ALSO

* [peasant village](peasant_village.md)	 - Interact with peasant online

###### Auto generated by spf13/cobra on 30-Oct-2026
