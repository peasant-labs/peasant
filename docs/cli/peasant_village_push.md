## peasant village push

Push ingested transcripts to the Peasant village

### Synopsis

Upload locally ingested session transcripts to the Peasant village for sharing and analytics.

Performance — the --concurrency default is max(1, NumCPU/2), tuned for the common case: a
steady-state / no-change re-push, where the server manifest skips already-pushed annotations
and unchanged transcripts so very little goes over the wire. A one-time LARGE COLD push (e.g.
hundreds of new transcripts + tens of thousands of new annotations) benefits from a higher
--concurrency. Throughput is bounded by the per-transcript S3 round-trip and the village's
CPU — NOT by the DB connection pool: a pooled connection is held only for the brief row
insert (the S3 upload itself holds none), so the village pool (default ~2x its vCPUs) has
headroom. A genuinely cold run of N new transcripts costs roughly
ceil(N / effective-concurrency) x the per-upload round-trip. For such a push set
--concurrency to about 2x NumCPU to scale throughput up to the village's capacity; confirm
with --timing. The default stays at max(1, NumCPU/2) — sufficient for the common
steady-state re-push, where the manifest skip means little goes over the wire.)

Exit status — a caller that branches on it, including a generated Git hook, needs the
one distinction it cannot read out of prose: whether anything was published at all.

  0  the run succeeded.
  1  the run failed after it had started talking to the village, so part of the work
     may be published and recorded as published.
  3  the run failed BEFORE a single village request was made — a missing or expired
     login, an unreadable config, a store that would not open, a repository scope that
     would not resolve. Nothing was published and nothing was recorded as published.

```
peasant village push [flags]
```

### Options

```
      --annotation-hash stringArray   Only push annotations with these content hashes (repeatable; default: all).
      --annotation-id stringArray     Only push these annotation IDs (repeatable; default: all). Counterpart to the share wizard's label selection.
      --concurrency int               Number of parallel uploads and HTTP connection-pool size. Must be >= 1. Overrides push.concurrency in config. Default: max(1, NumCPU/2) (tuned for steady-state re-push). For a one-time large COLD push, use ~22 to saturate the village pool toward the <5s target.
      --dry-run                       Show what would be pushed without uploading
      --force                         Re-push all sessions (including already-pushed ones)
  -h, --help                          help for push
      --json                          Output as JSON instead of human-readable
      --license string                Override the content license for this run (CC0-1.0, CC-BY-4.0, CC-BY-SA-4.0)
      --non-interactive               Run without the interactive wizard or public-consent prompt (for CI/scripts)
      --quiet                         Suppress the summary and redaction report; print only errors and a final result line
      --repository string             Only push sessions carrying this Git repository's canonical project identity (a path). Identity comes from the normalized origin remote when there is one Peasant can normalize, so separate clones of that origin share it; with no origin remote — or an origin that is not a network remote, such as a local path or a file:// URL — it is instead the worktree paths the sessions were recorded in, which belong to that directory alone. A repository nested inside another keeps its own identity and never inherits the outer one's. Which of the two was used is printed when the push runs. Default: every configured session
      --source-provider string        Filter to a specific provider (claude-code, gemini-cli, codex, opencode, cursor, strike)
      --timeout duration              Overall time budget for the whole upload (e.g. 5s). The per-request client timeout does not bound a push, which issues several requests in sequence, so a village that accepts a connection and never answers can stall for minutes. On expiry the push gives up and reports what did and did not reach the village. Default: no budget. Git hooks always pass one.
      --timing                        Measure and report per-phase push timing (connection setup/server split, redaction, annotation batches) to stderr, plus a per-upload JSONL log under the state dir. Off by default.
      --verbose                       Show per-session detail
      --visibility string             Override visibility for this run (public, private, group)
      --yes                           (alias for --non-interactive)
```

### Options inherited from parent commands

```
      --config string       Path to config file (default "~/.config/peasant/config.yaml")
      --config-dir string   Override the config directory (default: $XDG_CONFIG_HOME or ~/.config); config lives under <config-dir>/peasant
      --data-dir string     Override the data directory (default: $XDG_DATA_HOME or ~/.local/share); the DB + peasant-sync live under <data-dir>/peasant
      --state-dir string    Override the state directory (default: $XDG_STATE_HOME or ~/.local/state); logs/PID live under <state-dir>/peasant
```

### SEE ALSO

* [peasant village](peasant_village.md)	 - Interact with the Peasant village

###### Auto generated by spf13/cobra on 11-Aug-2026
